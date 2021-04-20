package serf

import (
	"io"
	"log"
	"os"
	"time"

	"github.com/hashicorp/serf/extpkg/memberlist"
)

// ProtocolVersionMap is the mapping of Serf delegate protocol versions
// to memberlist protocol versions. We mask the memberlist protocols using
// our own protocol version.
var ProtocolVersionMap map[uint8]uint8

// serf层的协议版本号 对应的的memberlist层的协议版本号
func init() {
	ProtocolVersionMap = map[uint8]uint8{
		5: 2,
		4: 2,
		3: 2,
		2: 2,
	}
}

// Config is the configuration for creating a Serf instance.
type Config struct {
	// The name of this node. This must be unique in the cluster. If this
	// is not set, Serf will set it to the hostname of the running machine.
	// 此节点的名称。这在集群中必须是唯一的。如果没有设置，Serf将把它设置为运行机器的主机名。
	NodeName string

	// The tags for this role, if any. This is used to provide arbitrary
	// key/value metadata per-node. For example, a "role" tag may be used to
	// differentiate "load-balancer" from a "web" role as parts of the same cluster.
	// Tags are deprecating 'Role', and instead it acts as a special key in this
	// map.
	// 这个角色的标记(如果有的话)。这用于为每个节点提供任意的键/值元数据。
	// 例如，“role”标签可以用来区分“load-balancer”和“web”角色，它们是同一个集群的一部分。
	// 标签正在弃用“Role”，取而代之的是它在这个映射中充当一个特殊的键。
	Tags map[string]string

	// EventCh is a channel that receives all the Serf events. The events
	// are sent on this channel in proper ordering. Care must be taken that
	// this channel doesn't block, either by processing the events quick
	// enough or buffering the channel, otherwise it can block state updates
	// within Serf itself. If no EventCh is specified, no events will be fired,
	// but point-in-time snapshots of members can still be retrieved by
	// calling Members on Serf.
	// Create(conf *Config)
	// EventCh是一个接收所有Serf事件的通道。事件在这个通道上以正确的顺序发送。
	// 必须注意这个通道不会阻塞，无论是通过足够快的处理事件还是缓冲通道，否则它会阻塞Serf自身的状态更新。
	// 如果没有指定EventCh，则不会触发任何事件，但是仍然可以通过在Serf上调用成员来检索成员的时间点快照。
	// func Create(agentConf *Config, conf *serf.Config, logOutput io.Writer)
	EventCh chan<- Event

	// ProtocolVersion is the protocol version to speak. This must be between
	// ProtocolVersionMin and ProtocolVersionMax.
	// serf层当前使用的版本号
	ProtocolVersion uint8

	// BroadcastTimeout is the amount of time to wait for a broadcast
	// message to be sent to the cluster. Broadcast messages are used for
	// things like leave messages and force remove messages. If this is not
	// set, a timeout of 5 seconds will be set.
	// BroadcastTimeout是等待广播消息发送到集群的时间。
	// 广播消息用于leave消息和force remove删除消息。
	// 如果没有设置此参数，将设置一个5秒的超时。
	BroadcastTimeout time.Duration

	// LeavePropagateDelay is for our leave (node dead) message to propagate
	// through the cluster. In particular, we want to stay up long enough to
	// service any probes from other nodes before they learn about us
	// leaving and stop probing. Otherwise, we risk getting node failures as
	// we leave.
	// LeavePropagateDelay是为了让我们的leave(节点已死)消息在集群中传播。
	// 特别是，我们想要保持足够长的时间，以便在其他节点知道我们离开并停止探测之前为它们提供服务。
	// 否则，当我们离开时，我们将面临节点故障的风险。
	LeavePropagateDelay time.Duration

	// The settings below relate to Serf's event coalescence feature. Serf
	// is able to coalesce multiple events into single events in order to
	// reduce the amount of noise that is sent along the EventCh. For example
	// if five nodes quickly join, the EventCh will be sent one EventMemberJoin
	// containing the five nodes rather than five individual EventMemberJoin
	// events. Coalescence can mitigate potential flapping behavior.
	//
	// Coalescence is disabled by default and can be enabled by setting
	// CoalescePeriod.
	//
	// CoalescePeriod specifies the time duration to coalesce events.
	// For example, if this is set to 5 seconds, then all events received
	// within 5 seconds that can be coalesced will be.
	//
	// QuiescentPeriod specifies the duration of time where if no events
	// are received, coalescence immediately happens. For example, if
	// CoalscePeriod is set to 10 seconds but QuiscentPeriod is set to 2
	// seconds, then the events will be coalesced and dispatched if no
	// new events are received within 2 seconds of the last event. Otherwise,
	// every event will always be delayed by at least 10 seconds.
	CoalescePeriod  time.Duration
	QuiescentPeriod time.Duration

	// The settings below relate to Serf's user event coalescing feature.
	// The settings operate like above but only affect user messages and
	// not the Member* messages that Serf generates.
	UserCoalescePeriod  time.Duration
	UserQuiescentPeriod time.Duration

	// The settings below relate to Serf keeping track of recently
	// failed/left nodes and attempting reconnects.
	//
	// ReapInterval is the interval when the reaper runs. If this is not
	// set (it is zero), it will be set to a reasonable default.
	//
	// ReconnectInterval is the interval when we attempt to reconnect
	// to failed nodes. If this is not set (it is zero), it will be set
	// to a reasonable default.
	//
	// ReconnectTimeout is the amount of time to attempt to reconnect to
	// a failed node before giving up and considering it completely gone.
	//
	// TombstoneTimeout is the amount of time to keep around nodes
	// that gracefully left as tombstones for syncing state with other
	// Serf nodes.
	// 下面的设置与Serf跟踪最近failed/left的节点并试图重新连接有关。
	// ReapInterval是定时清理运行时的间隔。如果没有设置这个值(它是零)，它将被设置为一个合理的默认值。
	// ReconnectInterval是尝试重新连接failed节点的时间间隔。如果没有设置这个值(它是零)，它将被设置为一个合理的默认值。
	// ReconnectTimeout是尝试重新连接failed节点的时间，在放弃并认为它完全消失之前。
	// TombstoneTimeout是保持作为left的节点与其他节点同步状态所需的时间。
	ReapInterval      time.Duration
	ReconnectInterval time.Duration
	ReconnectTimeout  time.Duration
	TombstoneTimeout  time.Duration

	// FlapTimeout is the amount of time less than which we consider a node
	// being failed and rejoining looks like a flap for telemetry purposes.
	// This should be set less than a typical reboot time, but large enough
	// to see actual events, given our expected detection times for a failed
	// node.
	// FlapTimeout是指小于我们认为节点发生故障的时间，从遥测的角度来看，重新加入节点的时间看起来像一个皮瓣。
	// 这一设置应该小于典型的重启时间，但是要大到可以看到实际事件，这是我们对失败节点的预期检测时间。
	FlapTimeout time.Duration

	// QueueCheckInterval is the interval at which we check the message
	// queue to apply the warning and max depth.
	QueueCheckInterval time.Duration

	// QueueDepthWarning is used to generate warning message if the
	// number of queued messages to broadcast exceeds this number. This
	// is to provide the user feedback if events are being triggered
	// faster than they can be disseminated
	// QueueDepthWarning用于在要广播的队列消息数量超过此数量时生成警告消息。
	// 这是为了在事件触发速度快于传播速度时提供用户反馈
	QueueDepthWarning int

	// MaxQueueDepth is used to start dropping messages if the number
	// of queued messages to broadcast exceeds this number. This is to
	// prevent an unbounded growth of memory utilization
	// MaxQueueDepth用于在要广播的队列消息数量超过这个数量时开始丢弃消息。这是为了防止内存使用的无限制增长
	MaxQueueDepth int

	// MinQueueDepth, if >0 will enforce a lower limit for dropping messages
	// and then the max will be max(MinQueueDepth, 2*SizeOfCluster). This
	// defaults to 0 which disables this dynamic sizing feature. If this is
	// >0 then MaxQueueDepth will be ignored.
	// MinQueueDepth，如果>0将强制删除消息的下限，那么最大值将为max(MinQueueDepth, 2*SizeOfCluster)。
	// 默认值为0，这将禁用这种动态大小调整特性。如果这是>0，那么MaxQueueDepth将被忽略。
	MinQueueDepth int

	// RecentIntentTimeout is used to determine how long we store recent
	// join and leave intents. This is used to guard against the case where
	// Serf broadcasts an intent that arrives before the Memberlist event.
	// It is important that this not be too short to avoid continuous
	// rebroadcasting of dead events.
	// RecentIntentTimeout用于确定最近join和leave意图的存储时间。
	// 这是用来防止Serf在Memberlist事件之前广播一个意图。
	// 重要的是，不要太短，以避免连续重播dead事件。
	// handleNodeLeaveIntent中如果不存在intent事件会记录并广播leave
	RecentIntentTimeout time.Duration

	// EventBuffer is used to control how many events are buffered.
	// This is used to prevent re-delivery of events to a client. The buffer
	// must be large enough to handle all "recent" events, since Serf will
	// not deliver messages that are older than the oldest entry in the buffer.
	// Thus if a client is generating too many events, it's possible that the
	// buffer gets overrun and messages are not delivered.
	EventBuffer int

	// QueryBuffer is used to control how many queries are buffered.
	// This is used to prevent re-delivery of queries to a client. The buffer
	// must be large enough to handle all "recent" events, since Serf will not
	// deliver queries older than the oldest entry in the buffer.
	// Thus if a client is generating too many queries, it's possible that the
	// buffer gets overrun and messages are not delivered.
	// QueryBuffer用于控制缓冲多少查询。这用于防止将查询重新交付给客户机。
	// 缓冲区必须足够大，以处理所有“最近”事件，因为Serf不会交付比缓冲区中最老的条目更老的查询。
	// 因此，如果客户机生成了太多的查询，可能会导致缓冲区溢出，从而无法传递消息。
	QueryBuffer int

	// QueryTimeoutMult configures the default timeout multipler for a query to run if no
	// specific value is provided. Queries are real-time by nature, where the
	// reply is time sensitive. As a result, results are collected in an async
	// fashion, however the query must have a bounded duration. We want the timeout
	// to be long enough that all nodes have time to receive the message, run a handler,
	// and generate a reply. Once the timeout is exceeded, any further replies are ignored.
	// The default value is
	//
	// Timeout = GossipInterval * QueryTimeoutMult * log(N+1)
	//
	// QueryTimeoutMult配置默认超时乘数，以便在没有提供特定值的情况下运行查询。
	// 查询本质上是实时的，其中的答复是时间敏感的。因此，结果以异步方式收集，但是查询必须有一个有限的持续时间。
	// 我们希望超时时间足够长，以便所有节点都有时间接收消息、运行处理程序并生成应答。
	// 一旦超过了超时，任何进一步的响应都将被忽略。默认值为
	// Timeout = GossipInterval * QueryTimeoutMult * log(N+1)
	QueryTimeoutMult int

	// QueryResponseSizeLimit and QuerySizeLimit limit the inbound and
	// outbound payload sizes for queries, respectively. These must fit
	// in a UDP packet with some additional overhead, so tuning these
	// past the default values of 1024 will depend on your network
	// configuration.
	// QueryResponseSizeLimit和QuerySizeLimit分别限制查询的入站和出站有效负载大小。
	// 这些参数必须包含在带有额外开销的UDP数据包中，因此将这些参数调优为超过默认值1024将取决于您的网络配置。
	QueryResponseSizeLimit int
	// query消息的报文长度限制
	QuerySizeLimit int

	// MemberlistConfig is the memberlist configuration that Serf will
	// use to do the underlying membership management and gossip. Some
	// fields in the MemberlistConfig will be overwritten by Serf no
	// matter what:
	//
	//   * Name - This will always be set to the same as the NodeName
	//     in this configuration.
	//
	//   * Events - Serf uses a custom event delegate.
	//
	//   * Delegate - Serf uses a custom delegate.
	//
	MemberlistConfig *memberlist.Config

	// LogOutput is the location to write logs to. If this is not set,
	// logs will go to stderr.
	LogOutput io.Writer

	// Logger is a custom logger which you provide. If Logger is set, it will use
	// this for the internal logger. If Logger is not set, it will fall back to the
	// behavior for using LogOutput. You cannot specify both LogOutput and Logger
	// at the same time.
	Logger *log.Logger

	// SnapshotPath if provided is used to snapshot live nodes as well
	// as lamport clock values. When Serf is started with a snapshot,
	// it will attempt to join all the previously known nodes until one
	// succeeds and will also avoid replaying old user events.
	SnapshotPath string

	// RejoinAfterLeave controls our interaction with the snapshot file.
	// When set to false (default), a leave causes a Serf to not rejoin
	// the cluster until an explicit join is received. If this is set to
	// true, we ignore the leave, and rejoin the cluster on start.
	RejoinAfterLeave bool

	// EnableNameConflictResolution controls if Serf will actively attempt
	// to resolve a name conflict. Since each Serf member must have a unique
	// name, a cluster can run into issues if multiple nodes claim the same
	// name. Without automatic resolution, Serf merely logs some warnings, but
	// otherwise does not take any action. Automatic resolution detects the
	// conflict and issues a special query which asks the cluster for the
	// Name -> IP:Port mapping. If there is a simple majority of votes, that
	// node stays while the other node will leave the cluster and exit.
	// EnableNameConflictResolution控件，如果Serf将积极尝试解决名称冲突。
	// 由于每个Serf成员必须有一个唯一的名称，如果多个节点声明相同的名称，集群可能会遇到问题。
	// 在没有自动解析的情况下，Serf只是记录一些警告，而不采取任何行动。
	// 自动解析检测冲突并发出一个特殊的查询，该查询要求集群提供Name -> IP:Port映射。
	// 如果投票结果为简单多数，则该节点保留，而另一个节点将离开集群并退出。
	EnableNameConflictResolution bool

	// DisableCoordinates controls if Serf will maintain an estimate of this
	// node's network coordinate internally. A network coordinate is useful
	// for estimating the network distance (i.e. round trip time) between
	// two nodes. Enabling this option adds some overhead to ping messages.
	// 如果Serf将在内部维持该节点的网络坐标的估计值，则禁用该控件。
	// 网络坐标对于估计两个节点之间的网络距离(即往返时间)是有用的。
	// 启用这个选项会给ping消息增加一些开销。
	DisableCoordinates bool

	// KeyringFile provides the location of a writable file where Serf can
	// persist changes to the encryption keyring.
	KeyringFile string

	// Merge can be optionally provided to intercept a cluster merge
	// and conditionally abort the merge.
	// memberList的回调
	Merge MergeDelegate

	// UserEventSizeLimit is maximum byte size limit of user event `name` + `payload` in bytes.
	// It's optimal to be relatively small, since it's going to be gossiped through the cluster.
	UserEventSizeLimit int

	// messageDropper is a callback used for selectively ignoring inbound
	// gossip messages. This should only be used in unit tests needing careful
	// control over sequencing of gossip arrival
	//
	// WARNING: this should ONLY be used in tests
	// 回复消息的回调 判断是否丢弃
	// messageDropper是一个回调函数，用于选择性地忽略入站八卦消息。这应该只在需要仔细控制消息到达顺序的单元测试中使用
	messageDropper func(typ messageType) bool

	// ReconnectTimeoutOverride is an optional interface which when present allows
	// the application to cause reaping of a node to happen when it otherwise wouldn't
	// ReconnectTimeoutOverride是一个可选的接口，如果有这个接口，应用程序就可以允许覆盖各个成员的重新连接超时
	ReconnectTimeoutOverride ReconnectTimeoutOverrider

	// ValidateNodeNames controls whether nodenames only
	// contain alphanumeric, dashes and '.'characters
	// and sets maximum length to 128 characters
	// 是否校验member的名称
	ValidateNodeNames bool
}

// Init allocates the subdata structures
func (c *Config) Init() {
	if c.Tags == nil {
		c.Tags = make(map[string]string)
	}
	if c.messageDropper == nil {
		c.messageDropper = func(typ messageType) bool {
			return false
		}
	}
}

// DefaultConfig returns a Config struct that contains reasonable defaults
// for most of the configurations.
func DefaultConfig() *Config {
	hostname, err := os.Hostname()
	if err != nil {
		panic(err)
	}

	return &Config{
		NodeName:                     hostname,
		BroadcastTimeout:             5 * time.Second,
		LeavePropagateDelay:          1 * time.Second,
		EventBuffer:                  512,
		QueryBuffer:                  512,
		LogOutput:                    os.Stderr,
		ProtocolVersion:              4,
		ReapInterval:                 15 * time.Second,
		RecentIntentTimeout:          5 * time.Minute,
		ReconnectInterval:            30 * time.Second,
		ReconnectTimeout:             24 * time.Hour,
		QueueCheckInterval:           30 * time.Second,
		QueueDepthWarning:            128,
		MaxQueueDepth:                4096,
		TombstoneTimeout:             24 * time.Hour,
		FlapTimeout:                  60 * time.Second,
		MemberlistConfig:             memberlist.DefaultLANConfig(),
		QueryTimeoutMult:             16,
		QueryResponseSizeLimit:       1024,
		QuerySizeLimit:               1024,
		EnableNameConflictResolution: true,
		DisableCoordinates:           false,
		ValidateNodeNames:            false,
		UserEventSizeLimit:           512,
	}
}
