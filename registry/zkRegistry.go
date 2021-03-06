package registry

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samuel/go-zookeeper/zk"
	cluster "github.com/weibocom/motan-go/cluster"
	motan "github.com/weibocom/motan-go/core"
	"github.com/weibocom/motan-go/log"
)

const (
	DEFAULT_SESSION_TIMEOUT_INTERVAL = 10 * 1000000 //ms
)

const (
	ZOOKEEPER_REGISTRY_NAMESPACE = "/motan"
	ZOOKEEPER_REGISTRY_COMMAND   = "/command"
	ZOOKEEPER_REGISTRY_NODE      = "/node"
	PATH_SEPARATOR               = "/"
)

const (
	ZOOKEEPER_NODETYPE_SERVER             = "server"
	ZOOKEEPER_NODETYPE_UNAVAILABLE_SERVER = "unavailableServer"
	ZOOKEEPER_NODETYPE_CLIENT             = "client"
	ZOOKEEPER_NODETYPE_AGENT              = "agent"
)

type ZkRegistry struct {
	url            *motan.Url
	timeout        time.Duration
	sessionTimeout time.Duration
	zkConn         *zk.Conn
	nodeRs         map[string]ServiceNode

	subscribeMap     map[string]map[string]motan.NotifyListener
	subscribeLock    sync.Mutex
	subscribeCmdLock sync.Mutex
	registryLock     sync.Mutex

	watchSwitcherMap map[string]chan bool
}

func (z *ZkRegistry) Initialize() {
	z.sessionTimeout = time.Duration(
		z.url.GetPositiveIntValue(motan.SessionTimeOutKey, DEFAULT_SESSION_TIMEOUT_INTERVAL)) * time.Millisecond
	z.timeout = time.Duration(z.url.GetPositiveIntValue(motan.TimeOutKey, DEFAULT_TIMEOUT)) * time.Millisecond
	if c, _, err := zk.Connect([]string{z.url.GetAddressStr()}, z.sessionTimeout); err == nil {
		z.zkConn = c
	} else {
		vlog.Errorf("zk connect error:%+v\n", err)
	}
	z.subscribeMap = make(map[string]map[string]motan.NotifyListener)
	z.nodeRs = make(map[string]ServiceNode)
	z.StartSnapshot(GetSanpshotConf())
}

func ToGroupPath(url *motan.Url) string {
	return ZOOKEEPER_REGISTRY_NAMESPACE + PATH_SEPARATOR + url.Group
}

func ToServicePath(url *motan.Url) string {
	return ToGroupPath(url) + PATH_SEPARATOR + url.Path
}

func ToCommandPath(url *motan.Url) string {
	return ToGroupPath(url) + ZOOKEEPER_REGISTRY_COMMAND
}

func ToNodeTypePath(url *motan.Url, nodeType string) string {
	return ToServicePath(url) + PATH_SEPARATOR + nodeType
}

func ToNodePath(url *motan.Url, nodeType string) string {
	if nodeType == ZOOKEEPER_NODETYPE_SERVER {
		return ToNodeTypePath(url, nodeType) + PATH_SEPARATOR + url.GetAddressStr()
	} else {
		return ToNodeTypePath(url, nodeType) + PATH_SEPARATOR + url.Host
	}
}

func ToAgentPath(url *motan.Url) string {
	return ZOOKEEPER_REGISTRY_NAMESPACE + PATH_SEPARATOR + ZOOKEEPER_NODETYPE_AGENT + PATH_SEPARATOR + url.Parameters["application"]
}

func ToAgentNodeTypePath(url *motan.Url) string {
	return ToAgentPath(url) + ZOOKEEPER_REGISTRY_NODE
}

func ToAgentNodePath(url *motan.Url) string {
	return ToAgentNodeTypePath(url) + PATH_SEPARATOR + url.GetAddressStr()
}

func ToAgentCommandPath(url *motan.Url) string {
	return ToAgentPath(url) + ZOOKEEPER_REGISTRY_COMMAND
}

func (z *ZkRegistry) RemoveNode(url *motan.Url, nodeType string) error {
	var (
		path string
		err  error
	)
	if IsAgent(url) {
		path = ToAgentNodePath(url)
	} else {
		path = ToNodePath(url, nodeType)
	}

	if isexist, stats, err := z.zkConn.Exists(path); err == nil {
		if isexist {
			if rmerr := z.zkConn.Delete(path, stats.Version); rmerr != nil {
				err = rmerr
			}
		}
	} else {
		vlog.Infof("zk query err:%+v\n", err)
	}
	return err
}

func (z *ZkRegistry) CreateNode(url *motan.Url, nodeType string) error {
	var (
		path     string
		nodePath string
		errc     error
	)
	if IsAgent(url) {
		path = ToAgentNodeTypePath(url)
		nodePath = ToAgentNodePath(url)
	} else {
		path = ToNodeTypePath(url, nodeType)
		nodePath = ToNodePath(url, nodeType)
	}
	if isexist, _, err := z.zkConn.Exists(path); err == nil {
		if !isexist {
			z.CreatePersistent(path, true)
		}
		var data []byte
		if _, errc := z.zkConn.Create(nodePath, data,
			zk.FlagEphemeral, zk.WorldACL(zk.PermAll)); errc != nil {
			vlog.Errorf("create node: %s, error, err:%s", nodePath, errc)
		} else {
			vlog.Infof("create node: %s", nodePath)
		}
	} else {
		errc = err
	}
	return errc
}

func (z *ZkRegistry) CreatePersistent(path string, createParents bool) {
	if _, err := z.zkConn.Create(path, nil, 0, zk.WorldACL(zk.PermAll)); err != nil {
		if createParents {
			parts := strings.Split(path, "/")
			parentPath := strings.Join(parts[:len(parts)-1], "/")
			z.CreatePersistent(parentPath, createParents)
			z.CreatePersistent(path, createParents)
		} else {
			vlog.Errorf("err create Persistent Path: %s", path)
		}
	} else {
		vlog.Infof("create Persistent node: %s", path)
	}
}

func (z *ZkRegistry) Register(url *motan.Url) {
	vlog.Infof("start zk register %s\n", url.GetIdentity())
	if url.Group == "" || url.Path == "" || url.Host == "" {
		vlog.Errorf("register fail.invalid url : %s\n", url.GetIdentity())
	}
	var nodeType string
	if nt, ok := url.Parameters["nodeType"]; !ok {
		nodeType = "unkonwn"
	} else {
		nodeType = nt
	}
	z.RemoveNode(url, nodeType)
	errc := z.CreateNode(url, nodeType)
	if errc != nil {
		vlog.Errorf("register failed, service:%s, error:%+v\n", url.GetIdentity(), errc)
	} else {
		vlog.Infof("register sucesss, service:%s\n", url.GetIdentity())
	}
}

func (z *ZkRegistry) UnRegister(url *motan.Url) {
	var nodeType string
	if nt, ok := url.Parameters["nodeType"]; !ok {
		nodeType = "unkonwn"
	} else {
		nodeType = nt
	}
	z.RemoveNode(url, nodeType)
}

// @TODO extInfo from java Obj Pase
func buildUrl4Nodes(nodes []string, url *motan.Url) []*motan.Url {
	result := make([]*motan.Url, 0, len(nodes))
	for _, node := range nodes {
		nodeinfo := strings.Split(node, ":")
		port, _ := strconv.Atoi(nodeinfo[1])
		refUrl := url.Copy()
		refUrl.Host = nodeinfo[0]
		refUrl.Port = port
		result = append(result, refUrl)
	}
	return result
}

func (z *ZkRegistry) Subscribe(url *motan.Url, listener motan.NotifyListener) {
	z.subscribeLock.Lock()
	defer z.subscribeLock.Unlock()
	vlog.Infof("start subscribe service. url:%s\n", url.GetIdentity())
	subkey := GetSubKey(url)
	idt := listener.GetIdentity()
	if listeners, ok := z.subscribeMap[subkey]; ok {
		if _, exist := listeners[idt]; !exist {
			listeners[idt] = listener
		}
	} else {
		lmap := make(map[string]motan.NotifyListener)
		lmap[idt] = listener
		z.subscribeMap[subkey] = lmap
		serverPath := ToNodeTypePath(url, ZOOKEEPER_NODETYPE_SERVER)
		if _, _, ch, err := z.zkConn.ChildrenW(serverPath); err == nil {
			vlog.Infof("start watch %s\n", subkey)
			ip := ""
			if len(motan.GetLocalIps()) > 0 {
				ip = motan.GetLocalIps()[0]
			}
			url.Parameters["nodeType"] = ZOOKEEPER_NODETYPE_CLIENT
			url.Host = ip
			z.Register(url)
			go func() {
				for {
					select {
					case evt := <-ch:
						if evt.Type == zk.EventNodeChildrenChanged {
							if nodes, _, chx, err := z.zkConn.ChildrenW(serverPath); err == nil {
								z.buildNodes(nodes, url)
								ch = chx
								if listeners, ok := z.subscribeMap[subkey]; ok {
									for _, l := range listeners {
										l.Notify(z.url, buildUrl4Nodes(nodes, url))
										vlog.Infof("EventNodeChildrenChanged %+v\n", nodes)
									}
								}
							}
						}
					}
				}
			}()
		} else {
			vlog.Infof("zk Subscribe err %+v\n", err)
		}
	}
}

func (z *ZkRegistry) buildNodes(nodes []string, url *motan.Url) {
	servicenode := &ServiceNode{
		Group: url.Group,
		Path:  url.Path,
	}
	nodeInfos := []SnapShotNodeInfo{}
	for _, addr := range nodes {
		info := &SnapShotNodeInfo{Addr: addr}
		nodeInfos = append(nodeInfos, *info)
	}
	servicenode.Nodes = nodeInfos
	z.nodeRs[getNodeKey(url)] = *servicenode
}

func (z *ZkRegistry) Unsubscribe(url *motan.Url, listener motan.NotifyListener) {
	z.subscribeLock.Lock()
	defer z.subscribeLock.Unlock()
	subkey := GetSubKey(url)
	idt := listener.GetIdentity()
	if listeners, ok := z.subscribeMap[subkey]; ok {
		delete(listeners, idt)
	}
}

func (z *ZkRegistry) Discover(url *motan.Url) []*motan.Url {
	nodePath := ToNodeTypePath(url, ZOOKEEPER_NODETYPE_SERVER)
	if nodes, _, err := z.zkConn.Children(nodePath); err == nil {
		z.buildNodes(nodes, url)
		return buildUrl4Nodes(nodes, url)
	} else {
		vlog.Errorf("zookeeper registry discover fail! discover url:%s, err:%s\n", url.GetIdentity(), err.Error())
		return nil
	}
}

func (z *ZkRegistry) SubscribeCommand(url *motan.Url, listener motan.CommandNotifyListener) {
	vlog.Infof("zookeeper subscribe command of %s\n", url.GetIdentity())
	var commandPath string
	if IsAgent(url) {
		commandPath = ToAgentCommandPath(url)
	} else {
		commandPath = ToCommandPath(url)
	}
	if isexist, _, err := z.zkConn.Exists(commandPath); err == nil {
		if !isexist {
			vlog.Infof("command didn't exists, path:%s\n", commandPath)
			return
		}
	} else {
		vlog.Errorf("check command exists error: %+v\n", err)
	}
	if _, _, ch, err := z.zkConn.GetW(commandPath); err == nil {
		z.watchSwitcherMap[commandPath] = make(chan bool)
		vlog.Infof("start watch command %s\n", commandPath)
		go func() {
			watchData := true
			for {
				select {
				case evt := <-ch:
					if evt.Type == zk.EventNodeDataChanged {
						if data, _, chx, err := z.zkConn.GetW(commandPath); err == nil {
							if watchData {
								ch = chx
							} else {
								// @TODO check this close if UnSubscribeCommand is still write sth
								close(z.watchSwitcherMap[commandPath])
								break
							}
							cmdInfo := string(data)
							listener.NotifyCommand(z.url, cluster.SERVICE_CMD, cmdInfo)
							vlog.Infof("command changed, path:%s, data:%s\n", commandPath, cmdInfo)
						} else {
							vlog.Infof("command changed, get cmdInfo error, err: %+v\n", err)
						}
					}
				case checkWatch := <-z.watchSwitcherMap[commandPath]:
					watchData = checkWatch
				}
			}
		}()
	} else {
		vlog.Warningf("zookeeper subscribe command fail. url:%s, err:%s, zk_path:%s, urlx:%+v\n", url.GetIdentity(), err.Error(), commandPath, url)
	}
}

func (z *ZkRegistry) UnSubscribeCommand(url *motan.Url, listener motan.CommandNotifyListener) {
	var commandPath string
	if IsAgent(url) {
		commandPath = ToAgentCommandPath(url)
	} else {
		commandPath = ToCommandPath(url)
	}
	z.watchSwitcherMap[commandPath] <- false
}

func (z *ZkRegistry) DiscoverCommand(url *motan.Url) string {
	vlog.Infof("zookeeper Discover command of %s\n", url.GetIdentity())
	var (
		res         string
		commandPath string
	)
	if IsAgent(url) {
		commandPath = ToAgentCommandPath(url)
	} else {
		commandPath = ToCommandPath(url)
	}
	if isexist, _, err := z.zkConn.Exists(commandPath); err == nil {
		if !isexist {
			vlog.Infof("zookeeper command didn't exist, path:%s\n", commandPath)
			return res
		}
	} else {
		vlog.Infof("zookeeper command check err: %+v\n", err)
		return res
	}
	if data, _, err := z.zkConn.Get(commandPath); err == nil {
		vlog.Infof("zookeeper Discover command %s\n", commandPath)
		res = string(data)
	} else {
		vlog.Warningf("zookeeper DiscoverCommand error. url:%s, err:%s\n", url.GetIdentity(), err.Error())
	}
	return res
}

func (z *ZkRegistry) Available(url *motan.Url) {

}

func (z *ZkRegistry) Unavailable(url *motan.Url) {

}

func (z *ZkRegistry) GetRegisteredServices() []*motan.Url {
	return nil
}

func (z *ZkRegistry) GetUrl() *motan.Url {
	return z.url
}

func (z *ZkRegistry) SetUrl(url *motan.Url) {

}

func (z *ZkRegistry) GetName() string {
	return "zookeeper"
}

func (z *ZkRegistry) StartSnapshot(conf *motan.SnapshotConf) {
	if _, err := os.Stat(conf.SnapshotDir); os.IsNotExist(err) {
		if err := os.Mkdir(conf.SnapshotDir, 0774); err != nil {
			vlog.Infoln(err)
		}
	}
	go func(z *ZkRegistry) {
		ticker := time.NewTicker(conf.SnapshotInterval)
		for range ticker.C {
			saveSnapshot(conf.SnapshotDir, z.nodeRs)
		}
	}(z)
}
