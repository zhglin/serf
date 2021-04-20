/*
memberlist is a library that manages cluster
membership and member failure detection using a gossip based protocol.

The use cases for such a library are far-reaching: all distributed systems
require membership, and memberlist is a re-usable solution to managing
cluster membership and node failure detection.

memberlist is eventually consistent but converges quickly on average.
The speed at which it converges can be heavily tuned via various knobs
on the protocol. Node failures are detected and network partitions are partially
tolerated by attempting to communicate to potentially dead nodes through
multiple routes.
*/
package memberlist

import (
	"container/list"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	multierror "github.com/hashicorp/go-multierror"
	sockaddr "github.com/hashicorp/go-sockaddr"
	"github.com/miekg/dns"
)

var errNodeNamesAreRequired = errors.New("memberlist: node names are required by configuration but one was not provided")

type Memberlist struct {
	sequenceNum uint32 // Local sequence number  // ping消息的序列号
	incarnation uint32 // Local incarnation number	// 当前live节点的标识号 变更一次+1
	numNodes    uint32 // Number of known nodes (estimate)	// 当前有多少个活跃节点
	pushPullReq uint32 // Number of push/pull requests		// tcp进行节点信息同步时的请求数

	advertiseLock sync.RWMutex //advertise变更互斥锁
	advertiseAddr net.IP       //advertise地址
	advertisePort uint16       //advertise端口号

	config     *Config       // memberList配置
	shutdown   int32         // Used as an atomic boolean value 是否已经关闭
	shutdownCh chan struct{} // memberlist 关闭的通知 终止对报文的处理
	// 当前节点是否已离开集群
	leave          int32         // Used as an atomic boolean value
	leaveBroadcast chan struct{} // leave消息完成广播后的通知

	shutdownLock sync.Mutex // Serializes calls to Shutdown
	leaveLock    sync.Mutex // Serializes calls to Leave

	transport NodeAwareTransport //节点进行交互的网络组件

	// udp中suspectMsg,aliveMsg,deadMsg,userMsg类型的数据包队列 进行异步处理
	handoffCh            chan struct{} // 通知有消息进入队列
	highPriorityMsgQueue *list.List    // 低优先级 aliveMsg 消息类型队列
	lowPriorityMsgQueue  *list.List    // 高优先级 suspectMsg,deadMsg,userMsg 消息类型队列
	msgQueueLock         sync.Mutex    //list操作的加锁保护

	// cluster中节点
	nodeLock   sync.RWMutex          // node操作的读写锁
	nodes      []*nodeState          // Known nodes							// 所有活跃节点
	nodeMap    map[string]*nodeState // Maps Node.Name -> NodeState			// 所有活跃节点map
	nodeTimers map[string]*suspicion // Maps Node.Name -> suspicion timer   // suspect节点的定时器 目标nodeName=>suspicion
	awareness  *awareness

	tickerLock sync.Mutex
	tickers    []*time.Ticker // schedule中开启的go定时任务数
	stopTick   chan struct{}  // schedule中的终止信号
	probeIndex int            // 下一次进行节点探测的nodes的下标

	// 异步的响应处理 Probe
	ackLock     sync.Mutex
	ackHandlers map[uint32]*ackHandler

	broadcasts *TransmitLimitedQueue

	logger *log.Logger
}

// BuildVsnArray creates the array of Vsn
// 支持的协议版本号 memberlist 最小版本 最大版本 使用版本 serf 最小版本 最大版本 使用版本
func (conf *Config) BuildVsnArray() []uint8 {
	return []uint8{
		ProtocolVersionMin, ProtocolVersionMax, conf.ProtocolVersion,
		conf.DelegateProtocolMin, conf.DelegateProtocolMax,
		conf.DelegateProtocolVersion,
	}
}

// newMemberlist creates the network listeners.
// Does not schedule execution of background maintenance.
// newMemberlist创建网络监听器。不安排后台维护的执行。
// 创建memberList
func newMemberlist(conf *Config) (*Memberlist, error) {
	if conf.ProtocolVersion < ProtocolVersionMin {
		return nil, fmt.Errorf("Protocol version '%d' too low. Must be in range: [%d, %d]",
			conf.ProtocolVersion, ProtocolVersionMin, ProtocolVersionMax)
	} else if conf.ProtocolVersion > ProtocolVersionMax {
		return nil, fmt.Errorf("Protocol version '%d' too high. Must be in range: [%d, %d]",
			conf.ProtocolVersion, ProtocolVersionMin, ProtocolVersionMax)
	}

	if len(conf.SecretKey) > 0 {
		if conf.Keyring == nil {
			keyring, err := NewKeyring(nil, conf.SecretKey)
			if err != nil {
				return nil, err
			}
			conf.Keyring = keyring
		} else {
			if err := conf.Keyring.AddKey(conf.SecretKey); err != nil {
				return nil, err
			}
			if err := conf.Keyring.UseKey(conf.SecretKey); err != nil {
				return nil, err
			}
		}
	}

	if conf.LogOutput != nil && conf.Logger != nil {
		return nil, fmt.Errorf("Cannot specify both LogOutput and Logger. Please choose a single log configuration setting.")
	}

	logDest := conf.LogOutput
	if logDest == nil {
		logDest = os.Stderr
	}

	logger := conf.Logger
	if logger == nil {
		logger = log.New(logDest, "", log.LstdFlags)
	}

	// Set up a network transport by default if a custom one wasn't given
	// by the config.
	// 创建默认transport组件
	transport := conf.Transport
	if transport == nil {
		nc := &NetTransportConfig{
			BindAddrs: []string{conf.BindAddr},
			BindPort:  conf.BindPort,
			Logger:    logger,
		}

		// See comment below for details about the retry in here.
		makeNetRetry := func(limit int) (*NetTransport, error) {
			var err error
			for try := 0; try < limit; try++ {
				var nt *NetTransport
				if nt, err = NewNetTransport(nc); err == nil {
					return nt, nil
				}
				if strings.Contains(err.Error(), "address already in use") {
					logger.Printf("[DEBUG] memberlist: Got bind error: %v", err)
					continue
				}
			}

			return nil, fmt.Errorf("failed to obtain an address: %v", err)
		}

		// The dynamic bind port operation is inherently racy because
		// even though we are using the kernel to find a port for us, we
		// are attempting to bind multiple protocols (and potentially
		// multiple addresses) with the same port number. We build in a
		// few retries here since this often gets transient errors in
		// busy unit tests.
		limit := 1
		if conf.BindPort == 0 { // 内核选择空闲的端口，因为会同时建立tcp以及udp链接，可能会导致端口冲突
			limit = 10
		}

		nt, err := makeNetRetry(limit)
		if err != nil {
			return nil, fmt.Errorf("Could not set up network transport: %v", err)
		}
		// 如果内核选择端口号 设置选择的端口号
		if conf.BindPort == 0 {
			port := nt.GetAutoBindPort()
			conf.BindPort = port
			conf.AdvertisePort = port
			logger.Printf("[DEBUG] memberlist: Using dynamic bind port %d", port)
		}

		// 设置memberList的transport
		transport = nt
	}

	// transport是否实现NodeAwareTransport接口
	nodeAwareTransport, ok := transport.(NodeAwareTransport)
	if !ok {
		logger.Printf("[DEBUG] memberlist: configured Transport is not a NodeAwareTransport and some features may not work as desired")
		nodeAwareTransport = &shimNodeAwareTransport{transport}
	}

	m := &Memberlist{
		config:               conf,
		shutdownCh:           make(chan struct{}),
		leaveBroadcast:       make(chan struct{}, 1),
		transport:            nodeAwareTransport,
		handoffCh:            make(chan struct{}, 1),
		highPriorityMsgQueue: list.New(),
		lowPriorityMsgQueue:  list.New(),
		nodeMap:              make(map[string]*nodeState),
		nodeTimers:           make(map[string]*suspicion),
		awareness:            newAwareness(conf.AwarenessMaxMultiplier),
		ackHandlers:          make(map[uint32]*ackHandler),
		broadcasts:           &TransmitLimitedQueue{RetransmitMult: conf.RetransmitMult},
		logger:               logger,
	}
	m.broadcasts.NumNodes = func() int {
		return m.estNumNodes()
	}

	// Get the final advertise address from the transport, which may need
	// to see which address we bound to. We'll refresh this each time we
	// send out an alive message.
	// 从传输中获得最终的广播地址，这可能需要查看我们绑定到哪个地址。
	// 我们每发送一条alive信息就刷新一次。
	// 校验或者设置advertise地址
	if _, _, err := m.refreshAdvertise(); err != nil {
		return nil, err
	}

	go m.streamListen()  // 读取tcp链接数据
	go m.packetListen()  // 处理udp报文
	go m.packetHandler() // udp队列报文
	return m, nil
}

// Create will create a new Memberlist using the given configuration.
// This will not connect to any other node (see Join) yet, but will start
// all the listeners to allow other nodes to join this memberlist.
// After creating a Memberlist, the configuration given should not be
// modified by the user anymore.
// Create将使用给定的配置创建一个新的Memberlist。
// 这将不会连接到任何其他节点(参见Join)，但将启动所有侦听器，以允许其他节点加入这个成员列表。
// 在创建Memberlist之后，用户不应该再修改给定的配置。
// 创建memberList 打开tcp udp链接
func Create(conf *Config) (*Memberlist, error) {
	m, err := newMemberlist(conf)
	if err != nil {
		return nil, err
	}
	if err := m.setAlive(); err != nil {
		m.Shutdown()
		return nil, err
	}
	m.schedule()
	return m, nil
}

// Join is used to take an existing Memberlist and attempt to join a cluster
// by contacting all the given hosts and performing a state sync. Initially,
// the Memberlist only contains our own state, so doing this will cause
// remote nodes to become aware of the existence of this node, effectively
// joining the cluster.
//
// This returns the number of hosts successfully contacted and an error if
// none could be reached. If an error is returned, the node did not successfully
// join the cluster.
// Join用于获取一个现有的成员列表，并尝试通过联系所有给定的主机并执行状态同步来加入集群。
// 最初，Memberlist只包含我们自己的状态，因此这样做将导致远程节点意识到这个节点的存在，从而有效地加入集群。
// 这将返回成功联系的主机数量，如果没有连接到主机，则返回一个错误。
// 如果返回错误，则说明该节点没有成功加入集群。
func (m *Memberlist) Join(existing []string) (int, error) {
	numSuccess := 0
	var errs error
	for _, exist := range existing {
		// 解析网络ip nodeName
		addrs, err := m.resolveAddr(exist)
		if err != nil {
			err = fmt.Errorf("Failed to resolve %s: %v", exist, err)
			errs = multierror.Append(errs, err)
			m.logger.Printf("[WARN] memberlist: %v", err)
			continue
		}

		for _, addr := range addrs {
			hp := joinHostPort(addr.ip.String(), addr.port)
			a := Address{Addr: hp, Name: addr.nodeName}
			// 从对方节点拉取对方节点的node信息 放给自己
			if err := m.pushPullNode(a, true); err != nil {
				err = fmt.Errorf("Failed to join %s: %v", addr.ip, err)
				errs = multierror.Append(errs, err)
				m.logger.Printf("[DEBUG] memberlist: %v", err)
				continue
			}
			numSuccess++
		}

	}
	if numSuccess > 0 {
		errs = nil
	}
	return numSuccess, errs
}

// ipPort holds information about a node we want to try to join.
type ipPort struct {
	ip       net.IP
	port     uint16
	nodeName string // optional
}

// tcpLookupIP is a helper to initiate a TCP-based DNS lookup for the given host.
// The built-in Go resolver will do a UDP lookup first, and will only use TCP if
// the response has the truncate bit set, which isn't common on DNS servers like
// Consul's. By doing the TCP lookup directly, we get the best chance for the
// largest list of hosts to join. Since joins are relatively rare events, it's ok
// to do this rather expensive operation.
func (m *Memberlist) tcpLookupIP(host string, defaultPort uint16, nodeName string) ([]ipPort, error) {
	// Don't attempt any TCP lookups against non-fully qualified domain
	// names, since those will likely come from the resolv.conf file.
	if !strings.Contains(host, ".") {
		return nil, nil
	}

	// Make sure the domain name is terminated with a dot (we know there's
	// at least one character at this point).
	dn := host
	if dn[len(dn)-1] != '.' {
		dn = dn + "."
	}

	// See if we can find a server to try.
	cc, err := dns.ClientConfigFromFile(m.config.DNSConfigPath)
	if err != nil {
		return nil, err
	}
	if len(cc.Servers) > 0 {
		// We support host:port in the DNS config, but need to add the
		// default port if one is not supplied.
		server := cc.Servers[0]
		if !hasPort(server) {
			server = net.JoinHostPort(server, cc.Port)
		}

		// Do the lookup.
		c := new(dns.Client)
		c.Net = "tcp"
		msg := new(dns.Msg)
		msg.SetQuestion(dn, dns.TypeANY)
		in, _, err := c.Exchange(msg, server)
		if err != nil {
			return nil, err
		}

		// Handle any IPs we get back that we can attempt to join.
		var ips []ipPort
		for _, r := range in.Answer {
			switch rr := r.(type) {
			case (*dns.A):
				ips = append(ips, ipPort{ip: rr.A, port: defaultPort, nodeName: nodeName})
			case (*dns.AAAA):
				ips = append(ips, ipPort{ip: rr.AAAA, port: defaultPort, nodeName: nodeName})
			case (*dns.CNAME):
				m.logger.Printf("[DEBUG] memberlist: Ignoring CNAME RR in TCP-first answer for '%s'", host)
			}
		}
		return ips, nil
	}

	return nil, nil
}

// resolveAddr is used to resolve the address into an address,
// port, and error. If no port is given, use the default
func (m *Memberlist) resolveAddr(hostStr string) ([]ipPort, error) {
	// First peel off any leading node name. This is optional.
	nodeName := ""
	if slashIdx := strings.Index(hostStr, "/"); slashIdx >= 0 {
		if slashIdx == 0 {
			return nil, fmt.Errorf("empty node name provided")
		}
		nodeName = hostStr[0:slashIdx]
		hostStr = hostStr[slashIdx+1:]
	}

	// This captures the supplied port, or the default one.
	hostStr = ensurePort(hostStr, m.config.BindPort)
	host, sport, err := net.SplitHostPort(hostStr)
	if err != nil {
		return nil, err
	}
	lport, err := strconv.ParseUint(sport, 10, 16)
	if err != nil {
		return nil, err
	}
	port := uint16(lport)

	// If it looks like an IP address we are done. The SplitHostPort() above
	// will make sure the host part is in good shape for parsing, even for
	// IPv6 addresses.
	if ip := net.ParseIP(host); ip != nil {
		return []ipPort{
			ipPort{ip: ip, port: port, nodeName: nodeName},
		}, nil
	}

	// First try TCP so we have the best chance for the largest list of
	// hosts to join. If this fails it's not fatal since this isn't a standard
	// way to query DNS, and we have a fallback below.
	ips, err := m.tcpLookupIP(host, port, nodeName)
	if err != nil {
		m.logger.Printf("[DEBUG] memberlist: TCP-first lookup failed for '%s', falling back to UDP: %s", hostStr, err)
	}
	if len(ips) > 0 {
		return ips, nil
	}

	// If TCP didn't yield anything then use the normal Go resolver which
	// will try UDP, then might possibly try TCP again if the UDP response
	// indicates it was truncated.
	ans, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}
	ips = make([]ipPort, 0, len(ans))
	for _, ip := range ans {
		ips = append(ips, ipPort{ip: ip, port: port, nodeName: nodeName})
	}
	return ips, nil
}

// setAlive is used to mark this node as being alive. This is the same
// as if we received an alive notification our own network channel for
// ourself.
// setAlive用于标记该节点为活着的。这与我们自己的网络频道接收到一个活的通知是一样的。
// 设置当前节点是alive
func (m *Memberlist) setAlive() error {
	// Get the final advertise address from the transport, which may need
	// to see which address we bound to.
	// 获取node通信的ip port 每次setAlive变更一次
	addr, port, err := m.refreshAdvertise()
	if err != nil {
		return err
	}

	// Check if this is a public address without encryption
	ipAddr, err := sockaddr.NewIPAddr(addr.String())
	if err != nil {
		return fmt.Errorf("Failed to parse interface addresses: %v", err)
	}
	ifAddrs := []sockaddr.IfAddr{
		sockaddr.IfAddr{
			SockAddr: ipAddr,
		},
	}
	_, publicIfs, err := sockaddr.IfByRFC("6890", ifAddrs)
	if len(publicIfs) > 0 && !m.config.EncryptionEnabled() {
		m.logger.Printf("[WARN] memberlist: Binding to public address without encryption!")
	}

	// Set any metadata from the delegate.
	// 校验tag长度
	var meta []byte
	if m.config.Delegate != nil {
		meta = m.config.Delegate.NodeMeta(MetaMaxSize)
		if len(meta) > MetaMaxSize {
			panic("Node meta data provided is longer than the limit")
		}
	}

	// 设置为alive
	a := alive{
		Incarnation: m.nextIncarnation(),      // 编号
		Node:        m.config.Name,            // 名称
		Addr:        addr,                     // ip地址
		Port:        uint16(port),             // 端口号
		Meta:        meta,                     // tag
		Vsn:         m.config.BuildVsnArray(), // 支持的版本号
	}
	m.aliveNode(&a, nil, true)

	return nil
}

// 获取advertise的ip port
func (m *Memberlist) getAdvertise() (net.IP, uint16) {
	m.advertiseLock.RLock()
	defer m.advertiseLock.RUnlock()
	return m.advertiseAddr, m.advertisePort
}

// 设置advertise地址以及端口号
func (m *Memberlist) setAdvertise(addr net.IP, port int) {
	m.advertiseLock.Lock()
	defer m.advertiseLock.Unlock()
	m.advertiseAddr = addr
	m.advertisePort = uint16(port)
}

// 校验或者获取advertise地址
func (m *Memberlist) refreshAdvertise() (net.IP, int, error) {
	addr, port, err := m.transport.FinalAdvertiseAddr(
		m.config.AdvertiseAddr, m.config.AdvertisePort)
	if err != nil {
		return nil, 0, fmt.Errorf("Failed to get final advertise address: %v", err)
	}
	m.setAdvertise(addr, port)
	return addr, port, nil
}

// LocalNode is used to return the local Node
// 在memberList层面获取当前的node信息
func (m *Memberlist) LocalNode() *Node {
	m.nodeLock.RLock()
	defer m.nodeLock.RUnlock()
	state := m.nodeMap[m.config.Name]
	return &state.Node
}

// UpdateNode is used to trigger re-advertising the local node. This is
// primarily used with a Delegate to support dynamic updates to the local
// meta data.  This will block until the update message is successfully
// broadcasted to a member of the cluster, if any exist or until a specified
// timeout is reached.
func (m *Memberlist) UpdateNode(timeout time.Duration) error {
	// Get the node meta data
	var meta []byte
	if m.config.Delegate != nil {
		meta = m.config.Delegate.NodeMeta(MetaMaxSize)
		if len(meta) > MetaMaxSize {
			panic("Node meta data provided is longer than the limit")
		}
	}

	// Get the existing node
	m.nodeLock.RLock()
	state := m.nodeMap[m.config.Name]
	m.nodeLock.RUnlock()

	// Format a new alive message
	a := alive{
		Incarnation: m.nextIncarnation(),
		Node:        m.config.Name,
		Addr:        state.Addr,
		Port:        state.Port,
		Meta:        meta,
		Vsn:         m.config.BuildVsnArray(),
	}
	notifyCh := make(chan struct{})
	m.aliveNode(&a, notifyCh, true)

	// Wait for the broadcast or a timeout
	if m.anyAlive() {
		var timeoutCh <-chan time.Time
		if timeout > 0 {
			timeoutCh = time.After(timeout)
		}
		select {
		case <-notifyCh:
		case <-timeoutCh:
			return fmt.Errorf("timeout waiting for update broadcast")
		}
	}
	return nil
}

// Deprecated: SendTo is deprecated in favor of SendBestEffort, which requires a node to
// target. If you don't have a node then use SendToAddress.
func (m *Memberlist) SendTo(to net.Addr, msg []byte) error {
	a := Address{Addr: to.String(), Name: ""}
	return m.SendToAddress(a, msg)
}

// serf层发送指定节点的udp消息  需要额外增加消息类型
func (m *Memberlist) SendToAddress(a Address, msg []byte) error {
	// Encode as a user message
	buf := make([]byte, 1, len(msg)+1)
	buf[0] = byte(userMsg)
	buf = append(buf, msg...)

	// Send the message
	return m.rawSendMsgPacket(a, nil, buf)
}

// Deprecated: SendToUDP is deprecated in favor of SendBestEffort.
func (m *Memberlist) SendToUDP(to *Node, msg []byte) error {
	return m.SendBestEffort(to, msg)
}

// Deprecated: SendToTCP is deprecated in favor of SendReliable.
func (m *Memberlist) SendToTCP(to *Node, msg []byte) error {
	return m.SendReliable(to, msg)
}

// SendBestEffort uses the unreliable packet-oriented interface of the transport
// to target a user message at the given node (this does not use the gossip
// mechanism). The maximum size of the message depends on the configured
// UDPBufferSize for this memberlist instance.
func (m *Memberlist) SendBestEffort(to *Node, msg []byte) error {
	// Encode as a user message
	buf := make([]byte, 1, len(msg)+1)
	buf[0] = byte(userMsg)
	buf = append(buf, msg...)

	// Send the message
	a := Address{Addr: to.Address(), Name: to.Name}
	return m.rawSendMsgPacket(a, to, buf)
}

// SendReliable uses the reliable stream-oriented interface of the transport to
// target a user message at the given node (this does not use the gossip
// mechanism). Delivery is guaranteed if no error is returned, and there is no
// limit on the size of the message.
func (m *Memberlist) SendReliable(to *Node, msg []byte) error {
	return m.sendUserMsg(to.FullAddress(), msg)
}

// Members returns a list of all known live nodes. The node structures
// returned must not be modified. If you wish to modify a Node, make a
// copy first.
func (m *Memberlist) Members() []*Node {
	m.nodeLock.RLock()
	defer m.nodeLock.RUnlock()

	nodes := make([]*Node, 0, len(m.nodes))
	for _, n := range m.nodes {
		if !n.DeadOrLeft() {
			nodes = append(nodes, &n.Node)
		}
	}

	return nodes
}

// NumMembers returns the number of alive nodes currently known. Between
// the time of calling this and calling Members, the number of alive nodes
// may have changed, so this shouldn't be used to determine how many
// members will be returned by Members.
// NumMembers返回当前已知的活动节点的数量。
// 在调用此函数和调用成员之间，活动节点的数量可能发生了变化，因此不应该使用此函数来确定成员将返回多少成员。
func (m *Memberlist) NumMembers() (alive int) {
	m.nodeLock.RLock()
	defer m.nodeLock.RUnlock()

	for _, n := range m.nodes {
		if !n.DeadOrLeft() {
			alive++
		}
	}

	return
}

// Leave will broadcast a leave message but will not shutdown the background
// listeners, meaning the node will continue participating in gossip and state
// updates.
//
// This will block until the leave message is successfully broadcasted to
// a member of the cluster, if any exist or until a specified timeout
// is reached.
//
// This method is safe to call multiple times, but must not be called
// after the cluster is already shut down.
// Leave将广播一个Leave消息，但不会关闭后台监听器，这意味着节点将继续参与八卦和状态更新。
// 这将阻塞，直到成功地将leave消息广播给集群的一个成员(如果存在的话)，或者直到达到指定的超时。
// 多次调用此方法是安全的，但不能在集群已经关闭后调用。
// serf层的退出 通知到memberList层
func (m *Memberlist) Leave(timeout time.Duration) error {
	m.leaveLock.Lock()
	defer m.leaveLock.Unlock()

	// 已经shutdown了
	if m.hasShutdown() {
		panic("leave after shutdown")
	}

	// 还没有left
	if !m.hasLeft() {
		// 标记leave
		atomic.StoreInt32(&m.leave, 1)

		m.nodeLock.Lock()
		state, ok := m.nodeMap[m.config.Name]
		m.nodeLock.Unlock()
		if !ok {
			m.logger.Printf("[WARN] memberlist: Leave but we're not in the node map.")
			return nil
		}

		// This dead message is special, because Node and From are the
		// same. This helps other nodes figure out that a node left
		// intentionally. When Node equals From, other nodes know for
		// sure this node is gone.
		// 这个dead消息很特别，因为Node和From是相同的。
		// 这有助于其他节点发现一个节点是故意留下的。
		// 当Node等于From时，其他节点肯定知道这个节点消失了。
		d := dead{
			Incarnation: state.Incarnation,
			Node:        state.Name,
			From:        state.Name,
		}
		m.deadNode(&d)

		// Block until the broadcast goes out
		// 阻塞到消息被广播出去
		if m.anyAlive() {
			var timeoutCh <-chan time.Time
			if timeout > 0 {
				timeoutCh = time.After(timeout)
			}
			select {
			case <-m.leaveBroadcast:
			case <-timeoutCh:
				return fmt.Errorf("timeout waiting for leave broadcast")
			}
		}
	}

	return nil
}

// Check for any other alive node.
func (m *Memberlist) anyAlive() bool {
	m.nodeLock.RLock()
	defer m.nodeLock.RUnlock()
	for _, n := range m.nodes {
		if !n.DeadOrLeft() && n.Name != m.config.Name {
			return true
		}
	}
	return false
}

// GetHealthScore gives this instance's idea of how well it is meeting the soft
// real-time requirements of the protocol. Lower numbers are better, and zero
// means "totally healthy".
func (m *Memberlist) GetHealthScore() int {
	return m.awareness.GetHealthScore()
}

// ProtocolVersion returns the protocol version currently in use by
// this memberlist.
// ProtocolVersion返回memberlist当前使用的协议版本。
func (m *Memberlist) ProtocolVersion() uint8 {
	// NOTE: This method exists so that in the future we can control
	// any locking if necessary, if we change the protocol version at
	// runtime, etc.
	// 注意:这个方法的存在是为了在将来我们可以在必要的时候控制任何锁，如果我们在运行时改变协议版本，等等。
	return m.config.ProtocolVersion
}

// Shutdown will stop any background maintenance of network activity
// for this memberlist, causing it to appear "dead". A leave message
// will not be broadcasted prior, so the cluster being left will have
// to detect this node's shutdown using probing. If you wish to more
// gracefully exit the cluster, call Leave prior to shutting down.
//
// This method is safe to call multiple times.
// 关机将停止任何后台维护网络活动的这个成员名单，导致它出现“死亡”。
// leave消息不会提前广播，因此被离开的集群必须使用探测来检测该节点的关闭。
// 如果您希望更优雅地退出集群，请在关闭集群之前调用Leave。
// 这个方法可以安全地多次调用。
func (m *Memberlist) Shutdown() error {
	m.shutdownLock.Lock()
	defer m.shutdownLock.Unlock()

	if m.hasShutdown() {
		return nil
	}

	// Shut down the transport first, which should block until it's
	// completely torn down. If we kill the memberlist-side handlers
	// those I/O handlers might get stuck.
	// 先关闭运输系统，它会一直阻塞直到链接全部关闭。如果我们终止成员列表端处理程序，这些I/O处理程序可能会卡住。
	if err := m.transport.Shutdown(); err != nil {
		m.logger.Printf("[ERR] Failed to shutdown transport: %v", err)
	}

	// Now tear down everything else.
	atomic.StoreInt32(&m.shutdown, 1) // 标记关闭
	close(m.shutdownCh)               // 终止相应的go协程
	m.deschedule()
	return nil
}

// 是否已关闭
func (m *Memberlist) hasShutdown() bool {
	return atomic.LoadInt32(&m.shutdown) == 1
}

// 是否已离开集群
func (m *Memberlist) hasLeft() bool {
	return atomic.LoadInt32(&m.leave) == 1
}

func (m *Memberlist) getNodeState(addr string) NodeStateType {
	m.nodeLock.RLock()
	defer m.nodeLock.RUnlock()

	n := m.nodeMap[addr]
	return n.State
}

func (m *Memberlist) getNodeStateChange(addr string) time.Time {
	m.nodeLock.RLock()
	defer m.nodeLock.RUnlock()

	n := m.nodeMap[addr]
	return n.StateChange
}

func (m *Memberlist) changeNode(addr string, f func(*nodeState)) {
	m.nodeLock.Lock()
	defer m.nodeLock.Unlock()

	n := m.nodeMap[addr]
	f(n)
}
