package memberlist

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"

	multierror "github.com/hashicorp/go-multierror"
)

type Config struct {
	// The name of this node. This must be unique in the cluster.
	// 当前节点名称
	Name string

	// Transport is a hook for providing custom code to communicate with
	// other nodes. If this is left nil, then memberlist will by default
	// make a NetTransport using BindAddr and BindPort from this structure.
	// 与其他节点进行交互的网络组件
	Transport Transport

	// Configuration related to what address to bind to and ports to
	// listen on. The port is used for both UDP and TCP gossip. It is
	// assumed other nodes are running on this port, but they do not need
	// to.
	// memberList监听的地址，端口号。会接受控制请求，"0,0,0,0"表示在所有网络接口上监听请求
	BindAddr string // 监听地址
	BindPort int    // 监听端口号

	// Configuration related to what address to advertise to other
	// cluster members. Used for nat traversal.
	// 节点间进行通信的地址，端口号，不设置的话会根据bindAddr的配置进行设置，
	// (bindAddr="0,0,0,0"会随机选择一个ip，否者就直接使用bindAddr
	AdvertiseAddr string
	AdvertisePort int

	// ProtocolVersion is the configured protocol version that we
	// will _speak_. This must be between ProtocolVersionMin and
	// ProtocolVersionMax.
	// ProtocolVersion是我们要讲的配置协议版本。必须在ProtocolVersionMin和ProtocolVersionMax之间。
	// memberList层的的协议版本号
	ProtocolVersion uint8

	// TCPTimeout is the timeout for establishing a stream connection with
	// a remote node for a full state sync, and for stream read and write
	// operations. This is a legacy name for backwards compatibility, but
	// should really be called StreamTimeout now that we have generalized
	// the transport.
	// TCPTimeout是与远程节点建立流连接以实现全状态同步的超时时间，以及流读写操作的超时时间。
	// 这是为了向后兼容性而使用的一个传统名称，但是现在我们已经将传输一般化了，所以应该真正地称为StreamTimeout。
	// tcp conn的读写超时时间
	TCPTimeout time.Duration

	// IndirectChecks is the number of nodes that will be asked to perform
	// an indirect probe of a node in the case a direct probe fails. Memberlist
	// waits for an ack from any single indirect node, so increasing this
	// number will increase the likelihood that an indirect probe will succeed
	// at the expense of bandwidth.
	// IndirectChecks是在直接探测失败的情况下，要求对节点执行间接探测的节点数量。
	// Memberlist等待来自任何一个间接节点的ack，因此增加这个数字将增加间接探测成功的可能性，但代价是带宽。
	IndirectChecks int

	// RetransmitMult is the multiplier for the number of retransmissions
	// that are attempted for messages broadcasted over gossip. The actual
	// count of retransmissions is calculated using the formula:
	//
	//   Retransmits = RetransmitMult * log(N+1)
	//
	// This allows the retransmits to scale properly with cluster size. The
	// higher the multiplier, the more likely a failed broadcast is to converge
	// at the expense of increased bandwidth.
	// udp消息的初始重试次数
	// RetransmitMult是试图对通过八卦广播的消息进行重传次数的乘数。实际的重传次数用公式计算:
	// Retransmits = RetransmitMult * log(N+1)
	// 这使得重传可以根据集群大小适当地扩展。乘数越高，失败的广播就越有可能以增加的带宽为代价收敛。
	RetransmitMult int

	// SuspicionMult is the multiplier for determining the time an
	// inaccessible node is considered suspect before declaring it dead.
	// The actual timeout is calculated using the formula:
	//
	//   SuspicionTimeout = SuspicionMult * log(N+1) * ProbeInterval
	//
	// This allows the timeout to scale properly with expected propagation
	// delay with a larger cluster size. The higher the multiplier, the longer
	// an inaccessible node is considered part of the cluster before declaring
	// it dead, giving that suspect node more time to refute if it is indeed
	// still alive.
	// SuspicionMult是一个乘数，用于确定一个无法访问的节点在宣布它死亡之前被认为是可疑的时间。
	// 实际的超时是通过以下公式计算的:SuspicionTimeout = SuspicionMult * log(N+1) * ProbeInterval(随机节点探测的间隔时间)，
	// 这允许在更大的集群大小下，超时以预期的传播延迟适当地扩展。
	// 乘数越高，无法访问的节点被认为是集群的一部分的时间就越长，然后才会宣布它已死亡，
	// 从而给可疑节点更多的时间来反驳它是否确实仍然是活的。
	SuspicionMult int

	// SuspicionMaxTimeoutMult is the multiplier applied to the
	// SuspicionTimeout used as an upper bound on detection time. This max
	// timeout is calculated using the formula:
	//
	// SuspicionMaxTimeout = SuspicionMaxTimeoutMult * SuspicionTimeout
	//
	// If everything is working properly, confirmations from other nodes will
	// accelerate suspicion timers in a manner which will cause the timeout
	// to reach the base SuspicionTimeout before that elapses, so this value
	// will typically only come into play if a node is experiencing issues
	// communicating with other nodes. It should be set to a something fairly
	// large so that a node having problems will have a lot of chances to
	// recover before falsely declaring other nodes as failed, but short
	// enough for a legitimately isolated node to still make progress marking
	// nodes failed in a reasonable amount of time.
	// SuspicionMaxTimeoutMult是应用于SuspicionTimeout的乘数，它被用作检测时间的上限。这个最大超时是通过以下公式计算的:
	// 		SuspicionMaxTimeout = SuspicionMaxTimeoutMult * SuspicionTimeout
	// 如果一切都正常工作，来自其他节点的确认将加速怀疑计时器的速度，从而导致超时在过期之前达到基本的怀疑超时(最小的超时时间)，
	// 所以这个值通常只在节点与其他节点的通信出现问题时起作用。它应该被设置为一个相当大的东西,
	// 以便出现问题的节点在错误地声明其他节点失败之前有很多机会恢复，
	// 但是足够短的时间，使得合法隔离的节点仍能取得进展标记节点在合理的时间内失败。
	SuspicionMaxTimeoutMult int

	// PushPullInterval is the interval between complete state syncs.
	// Complete state syncs are done with a single node over TCP and are
	// quite expensive relative to standard gossiped messages. Setting this
	// to zero will disable state push/pull syncs completely.
	//
	// Setting this interval lower (more frequent) will increase convergence
	// speeds across larger clusters at the expense of increased bandwidth
	// usage.
	// PushPullInterval是完全状态同步之间的间隔。
	// 完全的状态同步是通过TCP在一个节点上完成的，相对于标准的八卦消息来说，这是非常昂贵的。将此设置为0将完全禁用推/拉同步状态。
	// 将此间隔设置得更低(更频繁)将提高大型集群之间的收敛速度，但代价是增加带宽使用。
	PushPullInterval time.Duration

	// ProbeInterval and ProbeTimeout are used to configure probing
	// behavior for memberlist.
	//
	// ProbeInterval is the interval between random node probes. Setting
	// this lower (more frequent) will cause the memberlist cluster to detect
	// failed nodes more quickly at the expense of increased bandwidth usage.
	//
	// ProbeTimeout is the timeout to wait for an ack from a probed node
	// before assuming it is unhealthy. This should be set to 99-percentile
	// of RTT (round-trip time) on your network.
	//ProbeInterval和ProbeTimeout用于配置memberlist的探测行为。
	//ProbeInterval是随机节点探测的间隔时间。将此值设置得更低(更频繁)将导致memberlist集群更快地检测出故障节点，但代价是增加了带宽使用。
	//ProbeTimeout是在认为被探测节点不健康之前等待来自被探测节点的ack的超时。这应该设置为网络上RTT(往返时间)的99%。
	ProbeInterval time.Duration
	ProbeTimeout  time.Duration // 配置的网络超时时间

	// DisableTcpPings will turn off the fallback TCP pings that are attempted
	// if the direct UDP ping fails. These get pipelined along with the
	// indirect UDP pings.
	// DisableTcpPings将关闭在直接UDP ping失败时尝试的回退TCP ping。这些与间接的UDP ping一起被流水线化。
	DisableTcpPings bool

	// DisableTcpPingsForNode is like DisableTcpPings, but lets you control
	// whether to perform TCP pings on a node-by-node basis.
	// DisableTcpPingsForNode与DisableTcpPings类似，但允许您控制是否逐个节点执行TCP ping。
	DisableTcpPingsForNode func(nodeName string) bool

	// AwarenessMaxMultiplier will increase the probe interval if the node
	// becomes aware that it might be degraded and not meeting the soft real
	// time requirements to reliably probe other nodes.
	// AwarenessMaxMultiplier在节点意识到自己可能被降级，无法满足可靠探测其他节点的软实时性要求时，会增加探测间隔。
	// 超时时间最大能增加到多少倍
	AwarenessMaxMultiplier int

	// GossipInterval and GossipNodes are used to configure the gossip
	// behavior of memberlist.
	// GossipInterval,GossipNodes 用于配置成员列表的八卦行为。
	//
	// GossipInterval is the interval between sending messages that need
	// to be gossiped that haven't been able to piggyback on probing messages.
	// If this is set to zero, non-piggyback gossip is disabled. By lowering
	// this value (more frequent) gossip messages are propagated across
	// the cluster more quickly at the expense of increased bandwidth.
	//
	// GossipInterval是发送那些需要八卦但无法被探测到的消息之间的时间间隔。
	// 如果该值设置为零，则禁用非附带八卦。通过降低这个值(更频繁地)，八卦消息在集群中传播得更快，但代价是增加了带宽。
	//
	// GossipNodes is the number of random nodes to send gossip messages to
	// per GossipInterval. Increasing this number causes the gossip messages
	// to propagate across the cluster more quickly at the expense of
	// increased bandwidth.
	//
	// GossipNodes是每个八卦间隔发送八卦消息的随机节点的数量。
	// 增加这个数字会使流言消息在集群中传播得更快，但代价是增加了带宽。
	//
	// GossipToTheDeadTime is the interval after which a node has died that
	// we will still try to gossip to it. This gives it a chance to refute.
	GossipInterval time.Duration
	GossipNodes    int
	// 持续deadTime的时间,如果变更到deadTime的时间距当前时间大于GossipToTheDeadTime,就认为已经dead
	// 我们仍然会尝试向它闲谈。这给了它反驳的机会。
	GossipToTheDeadTime time.Duration

	// GossipVerifyIncoming controls whether to enforce encryption for incoming
	// gossip. It is used for upshifting from unencrypted to encrypted gossip on
	// a running cluster.
	GossipVerifyIncoming bool

	// GossipVerifyOutgoing controls whether to enforce encryption for outgoing
	// gossip. It is used for upshifting from unencrypted to encrypted gossip on
	// a running cluster.
	// 控制是否对传出的流言强制加密。它用于在运行的集群上从未加密的八卦升级到加密的八卦。
	GossipVerifyOutgoing bool

	// EnableCompression is used to control message compression. This can
	// be used to reduce bandwidth usage at the cost of slightly more CPU
	// utilization. This is only available starting at protocol version 1.
	// EnableCompression用于控制消息压缩。
	// 这可以用来减少带宽的使用，但代价是略微增加CPU的使用。这只能从协议版本1开始使用。
	EnableCompression bool

	// SecretKey is used to initialize the primary encryption key in a keyring.
	// The primary encryption key is the only key used to encrypt messages and
	// the first key used while attempting to decrypt messages. Providing a
	// value for this primary key will enable message-level encryption and
	// verification, and automatically install the key onto the keyring.
	// The value should be either 16, 24, or 32 bytes to select AES-128,
	// AES-192, or AES-256.
	SecretKey []byte

	// The keyring holds all of the encryption keys used internally. It is
	// automatically initialized using the SecretKey and SecretKeys values.
	Keyring *Keyring

	// Delegate and Events are delegates for receiving and providing
	// data to memberlist via callback mechanisms. For Delegate, see
	// the Delegate interface. For Events, see the EventDelegate interface.
	//
	// The DelegateProtocolMin/Max are used to guarantee protocol-compatibility
	// for any custom messages that the delegate might do (broadcasts,
	// local/remote state, etc.). If you don't set these, then the protocol
	// versions will just be zero, and version compliance won't be done.
	// memberlist的委托接口 处理gossip协议 serf层实现
	Delegate Delegate
	// serf层的协议版本号
	DelegateProtocolVersion uint8
	// serf层支持的最小的协议版本号
	DelegateProtocolMin uint8
	// serf层支持的最大的协议版本号
	DelegateProtocolMax uint8

	// serf层的事件处理回调
	Events   EventDelegate    // join,leave,update事件 conf.MemberlistConfig.Events = &eventDelegate{serf: serf}
	Conflict ConflictDelegate // node名称冲突的事件回调  conf.MemberlistConfig.Conflict = &conflictDelegate{serf: serf}
	Merge    MergeDelegate    // tcp进行节点同步的回调
	Ping     PingDelegate     // ping消息回调 用于计算节点坐标  conf.MemberlistConfig.Ping = &pingDelegate{serf: serf}
	Alive    AliveDelegate    // serf层设置的活跃节点的回调

	// DNSConfigPath points to the system's DNS config file, usually located
	// at /etc/resolv.conf. It can be overridden via config for easier testing.
	DNSConfigPath string

	// LogOutput is the writer where logs should be sent. If this is not
	// set, logging will go to stderr by default. You cannot specify both LogOutput
	// and Logger at the same time.
	LogOutput io.Writer

	// Logger is a custom logger which you provide. If Logger is set, it will use
	// this for the internal logger. If Logger is not set, it will fall back to the
	// behavior for using LogOutput. You cannot specify both LogOutput and Logger
	// at the same time.
	Logger *log.Logger

	// Size of Memberlist's internal channel which handles UDP messages. The
	// size of this determines the size of the queue which Memberlist will keep
	// while UDP messages are handled.
	// udp消息中suspectMsg,aliveMsg,deadMsg,userMsg类型的数据包队列大小 超过会被丢弃
	HandoffQueueDepth int

	// Maximum number of bytes that memberlist will put in a packet (this
	// will be for UDP packets by default with a NetTransport). A safe value
	// for this is typically 1400 bytes (which is the default). However,
	// depending on your network's MTU (Maximum Transmission Unit) you may
	// be able to increase this to get more content into each gossip packet.
	// This is a legacy name for backward compatibility but should really be
	// called PacketBufferSize now that we have generalized the transport.
	UDPBufferSize int

	// DeadNodeReclaimTime controls the time before a dead node's name can be
	// reclaimed by one with a different address or port. By default, this is 0,
	// meaning nodes cannot be reclaimed this way.
	// 节点修改addr,ip导致从dead状态变更成alive状态的合法时间间隔,超过此时间拒绝状态变更
	DeadNodeReclaimTime time.Duration

	// RequireNodeNames controls if the name of a node is required when sending
	// a message to that node.
	// 发送消息是否必须携带node名称
	RequireNodeNames bool
	// CIDRsAllowed If nil, allow any connection (default), otherwise specify all networks
	// allowed to connect (you must specify IPv6/IPv4 separately)
	// Using [] will block all connections.
	// CIDRsAllowed如果为nil，则允许任何连接(默认值)，否则指定所有允许连接的网络(必须单独指定IPv6/IPv4)使用[]将阻塞所有连接。
	// 限制特定的ip加入集群 不填就不限制
	CIDRsAllowed []net.IPNet
}

// ParseCIDRs return a possible empty list of all Network that have been parsed
// In case of error, it returns succesfully parsed CIDRs and the last error found
func ParseCIDRs(v []string) ([]net.IPNet, error) {
	nets := make([]net.IPNet, 0)
	if v == nil {
		return nets, nil
	}
	var errs error
	hasErrors := false
	for _, p := range v {
		_, net, err := net.ParseCIDR(strings.TrimSpace(p))
		if err != nil {
			err = fmt.Errorf("invalid cidr: %s", p)
			errs = multierror.Append(errs, err)
			hasErrors = true
		} else {
			nets = append(nets, *net)
		}
	}
	if !hasErrors {
		errs = nil
	}
	return nets, errs
}

// DefaultLANConfig returns a sane set of configurations for Memberlist.
// It uses the hostname as the node name, and otherwise sets very conservative
// values that are sane for most LAN environments. The default configuration
// errs on the side of caution, choosing values that are optimized
// for higher convergence at the cost of higher bandwidth usage. Regardless,
// these values are a good starting point when getting started with memberlist.
func DefaultLANConfig() *Config {
	hostname, _ := os.Hostname()
	return &Config{
		Name:                    hostname,
		BindAddr:                "0.0.0.0",
		BindPort:                7946,
		AdvertiseAddr:           "",
		AdvertisePort:           7946,
		ProtocolVersion:         ProtocolVersion2Compatible,
		TCPTimeout:              10 * time.Second,       // Timeout after 10 seconds
		IndirectChecks:          3,                      // Use 3 nodes for the indirect ping
		RetransmitMult:          4,                      // Retransmit a message 4 * log(N+1) nodes
		SuspicionMult:           4,                      // Suspect a node for 4 * log(N+1) * Interval
		SuspicionMaxTimeoutMult: 6,                      // For 10k nodes this will give a max timeout of 120 seconds
		PushPullInterval:        30 * time.Second,       // Low frequency
		ProbeTimeout:            500 * time.Millisecond, // Reasonable RTT time for LAN
		ProbeInterval:           1 * time.Second,        // Failure check every second
		DisableTcpPings:         false,                  // TCP pings are safe, even with mixed versions
		AwarenessMaxMultiplier:  8,                      // Probe interval backs off to 8 seconds

		GossipNodes:          3,                      // Gossip to 3 nodes
		GossipInterval:       200 * time.Millisecond, // Gossip more rapidly
		GossipToTheDeadTime:  30 * time.Second,       // Same as push/pull
		GossipVerifyIncoming: true,
		GossipVerifyOutgoing: true,

		EnableCompression: true, // Enable compression by default

		SecretKey: nil,
		Keyring:   nil,

		DNSConfigPath: "/etc/resolv.conf",

		HandoffQueueDepth: 1024,
		UDPBufferSize:     1400,
		CIDRsAllowed:      nil, // same as allow all
	}
}

// DefaultWANConfig works like DefaultConfig, however it returns a configuration
// that is optimized for most WAN environments. The default configuration is
// still very conservative and errs on the side of caution.
func DefaultWANConfig() *Config {
	conf := DefaultLANConfig()
	conf.TCPTimeout = 30 * time.Second
	conf.SuspicionMult = 6
	conf.PushPullInterval = 60 * time.Second
	conf.ProbeTimeout = 3 * time.Second
	conf.ProbeInterval = 5 * time.Second
	conf.GossipNodes = 4 // Gossip less frequently, but to an additional node
	conf.GossipInterval = 500 * time.Millisecond
	conf.GossipToTheDeadTime = 60 * time.Second
	return conf
}

// IPMustBeChecked return true if IPAllowed must be called
// 是否限制ip
func (c *Config) IPMustBeChecked() bool {
	return len(c.CIDRsAllowed) > 0
}

// IPAllowed return an error if access to memberlist is denied
// 是否允许加入集群
func (c *Config) IPAllowed(ip net.IP) error {
	if !c.IPMustBeChecked() {
		return nil
	}

	// 校验ip是否在allowed配置里面
	for _, n := range c.CIDRsAllowed {
		if n.Contains(ip) {
			return nil
		}
	}
	return fmt.Errorf("%s is not allowed", ip)
}

// DefaultLocalConfig works like DefaultConfig, however it returns a configuration
// that is optimized for a local loopback environments. The default configuration is
// still very conservative and errs on the side of caution.
func DefaultLocalConfig() *Config {
	conf := DefaultLANConfig()
	conf.TCPTimeout = time.Second
	conf.IndirectChecks = 1
	conf.RetransmitMult = 2
	conf.SuspicionMult = 3
	conf.PushPullInterval = 15 * time.Second
	conf.ProbeTimeout = 200 * time.Millisecond
	conf.ProbeInterval = time.Second
	conf.GossipInterval = 100 * time.Millisecond
	conf.GossipToTheDeadTime = 15 * time.Second
	return conf
}

// Returns whether or not encryption is enabled
// 是否开启加密数据传输
func (c *Config) EncryptionEnabled() bool {
	return c.Keyring != nil && len(c.Keyring.GetKeys()) > 0
}
