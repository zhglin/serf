package memberlist

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"net"
	"strings"
	"sync/atomic"
	"time"

	metrics "github.com/armon/go-metrics"
)

type NodeStateType int

const (
	StateAlive   NodeStateType = iota
	StateSuspect               // ping失败设置成可疑
	StateDead                  // 各种ping失败就从可疑设置成dead
	StateLeft                  // node的主动leave会设置成left, left的节点不参与gossip消息, 不会被ping。left不会变成dead，重新加入集群会变成alive
)

// Node represents a node in the cluster.
type Node struct {
	Name  string
	Addr  net.IP
	Port  uint16
	Meta  []byte        // Metadata from the delegate for this node.
	State NodeStateType // State of the node.
	PMin  uint8         // Minimum protocol version this understands
	PMax  uint8         // Maximum protocol version this understands
	PCur  uint8         // Current version node is speaking
	DMin  uint8         // Min protocol version for the delegate to understand
	DMax  uint8         // Max protocol version for the delegate to understand
	DCur  uint8         // Current version delegate is speaking
}

// Address returns the host:port form of a node's address, suitable for use
// with a transport.
func (n *Node) Address() string {
	return joinHostPort(n.Addr.String(), n.Port)
}

// FullAddress returns the node name and host:port form of a node's address,
// suitable for use with a transport.
func (n *Node) FullAddress() Address {
	return Address{
		Addr: joinHostPort(n.Addr.String(), n.Port),
		Name: n.Name,
	}
}

// String returns the node name
func (n *Node) String() string {
	return n.Name
}

// NodeState is used to manage our state view of another node
type nodeState struct {
	Node
	Incarnation uint32        // Last known incarnation number  只有alive,update,refute才会修改incarnation
	State       NodeStateType // Current state						当前状态
	StateChange time.Time     // Time last state change happened	状态变更时间
}

// Address returns the host:port form of a node's address, suitable for use
// with a transport.
func (n *nodeState) Address() string {
	return n.Node.Address()
}

// FullAddress returns the node name and host:port form of a node's address,
// suitable for use with a transport.
func (n *nodeState) FullAddress() Address {
	return n.Node.FullAddress()
}

// 是否已经是dead || left状态
func (n *nodeState) DeadOrLeft() bool {
	return n.State == StateDead || n.State == StateLeft
}

// ackHandler is used to register handlers for incoming acks and nacks.
// 异步对ack响应的处理handler
type ackHandler struct {
	ackFn  func([]byte, time.Time)
	nackFn func()
	timer  *time.Timer
}

// NoPingResponseError is used to indicate a 'ping' packet was
// successfully issued but no response was received
type NoPingResponseError struct {
	node string
}

func (f NoPingResponseError) Error() string {
	return fmt.Sprintf("No response from node %s", f.node)
}

// Schedule is used to ensure the Tick is performed periodically. This
// function is safe to call multiple times. If the memberlist is already
// scheduled, then it won't do anything.
// 用于确保滴答周期性地执行。这个函数可以安全地多次调用。如果成员列表已经被调度，那么它不会做任何事情。
func (m *Memberlist) schedule() {
	m.tickerLock.Lock()
	defer m.tickerLock.Unlock()

	// If we already have tickers, then don't do anything, since we're
	// scheduled
	// 如果我们已经准备好了，那就什么都不要做
	if len(m.tickers) > 0 {
		return
	}

	// Create the stop tick channel, a blocking channel. We close this
	// when we should stop the tickers.
	// 用来协调下面几个go协程的退出
	stopCh := make(chan struct{})

	// Create a new probeTicker
	// 定时对节点进行节点探测
	// 随机对alive以及未超时的dead节点进行ping (直接，间接，tcp)
	// 如果在超时未收到响应就标记并广播Suspect信息
	// 标记为Suspect的一段时间仍然没有收到信息，就降级为dead消息并进行广播，并传递到serf层
	if m.config.ProbeInterval > 0 {
		t := time.NewTicker(m.config.ProbeInterval)
		// 定时不断执行m.probe
		go m.triggerFunc(m.config.ProbeInterval, t.C, stopCh, m.probe)
		m.tickers = append(m.tickers, t)
	}

	// Create a push pull ticker if needed
	// 定时进行tcp节点信息交换
	if m.config.PushPullInterval > 0 {
		go m.pushPullTrigger(stopCh)
	}

	// Create a gossip ticker if needed
	// 如果需要，创建一个八卦消息
	if m.config.GossipInterval > 0 && m.config.GossipNodes > 0 {
		t := time.NewTicker(m.config.GossipInterval) // 定时执行m.gossip
		go m.triggerFunc(m.config.GossipInterval, t.C, stopCh, m.gossip)
		m.tickers = append(m.tickers, t)
	}

	// If we made any tickers, then record the stopTick channel for
	// later.
	// 如果我们做了任何定时器，那么就记录下定时器的通道，供以后使用。
	if len(m.tickers) > 0 {
		m.stopTick = stopCh
	}
}

// triggerFunc is used to trigger a function call each time a
// message is received until a stop tick arrives.
// triggerFunc用于在每次收到消息时触发一个函数调用，直到到达一个停止点。
func (m *Memberlist) triggerFunc(stagger time.Duration, C <-chan time.Time, stop <-chan struct{}, f func()) {
	// Use a random stagger to avoid syncronizing
	// 随机错开时间 防止各个节点都在同一个时间进行
	randStagger := time.Duration(uint64(rand.Int63()) % uint64(stagger))
	select {
	case <-time.After(randStagger):
	case <-stop:
		return
	}

	// 定时 执行f()
	for {
		select {
		case <-C:
			f()
		case <-stop:
			return
		}
	}
}

// pushPullTrigger is used to periodically trigger a push/pull until
// a stop tick arrives. We don't use triggerFunc since the push/pull
// timer is dynamically scaled based on cluster size to avoid network
// saturation
// pushPullTrigger用于定期触发推/拉，直到一个停止滴答到达。
// 我们不使用triggerFunc，因为推/拉计时器是根据集群大小动态缩放的，以避免网络饱和
func (m *Memberlist) pushPullTrigger(stop <-chan struct{}) {
	interval := m.config.PushPullInterval

	// Use a random stagger to avoid syncronizing
	// 使用随机错开来避免同步
	randStagger := time.Duration(uint64(rand.Int63()) % uint64(interval))
	select {
	case <-time.After(randStagger):
	case <-stop:
		return
	}

	// Tick using a dynamic timer
	for {
		// 每次重新计算时间间隔
		tickTime := pushPullScale(interval, m.estNumNodes())
		select {
		case <-time.After(tickTime):
			m.pushPull()
		case <-stop:
			return
		}
	}
}

// Deschedule is used to stop the background maintenance. This is safe
// to call multiple times.
// desschedule命令用于停止schedule开启的go协程
func (m *Memberlist) deschedule() {
	m.tickerLock.Lock()
	defer m.tickerLock.Unlock()

	// If we have no tickers, then we aren't scheduled.
	// 没有定时任务
	if len(m.tickers) == 0 {
		return
	}

	// Close the stop channel so all the ticker listeners stop.
	// 关闭停止通道。
	close(m.stopTick)

	// Explicitly stop all the tickers themselves so they don't take
	// up any more resources, and get rid of the list.
	// 明确地停止所有的ticker，这样它们就不会占用更多的资源，并删除列表。
	for _, t := range m.tickers {
		t.Stop()
	}
	m.tickers = nil
}

// Tick is used to perform a single round of failure detection and gossip
// Tick用于执行单轮故障检测和八卦
func (m *Memberlist) probe() {
	// Track the number of indexes we've considered probing
	// 防止一直goto START 都是DeadOrLeft节点
	numCheck := 0
START:
	m.nodeLock.RLock()

	// Make sure we don't wrap around infinitely
	if numCheck >= len(m.nodes) {
		m.nodeLock.RUnlock()
		return
	}

	// Handle the wrap around case
	if m.probeIndex >= len(m.nodes) {
		m.nodeLock.RUnlock()
		m.resetNodes() // 重置nodes
		m.probeIndex = 0
		numCheck++
		goto START
	}

	// Determine if we should probe this node
	skip := false
	var node nodeState

	// 校验当前的probeIndex的node 跳过自身节点已经dead节点
	node = *m.nodes[m.probeIndex]
	if node.Name == m.config.Name {
		skip = true
	} else if node.DeadOrLeft() {
		skip = true
	}

	// Potentially skip
	m.nodeLock.RUnlock()
	m.probeIndex++
	if skip {
		numCheck++
		goto START
	}

	// Probe the specific node
	// 探测特定的节点
	m.probeNode(&node)
}

// probeNodeByAddr just safely calls probeNode given only the address of the node (for tests)
func (m *Memberlist) probeNodeByAddr(addr string) {
	m.nodeLock.RLock()
	n := m.nodeMap[addr]
	m.nodeLock.RUnlock()

	m.probeNode(n)
}

// failedRemote checks the error and decides if it indicates a failure on the
// other end.
// 检查错误并决定它是否指示了另一端的失败。
func failedRemote(err error) bool {
	switch t := err.(type) {
	case *net.OpError:
		if strings.HasPrefix(t.Net, "tcp") {
			switch t.Op {
			case "dial", "read", "write":
				return true
			}
		}
	}
	return false
}

// probeNode handles a single round of failure checking on a node.
// 发送ping消息进行节点探测
func (m *Memberlist) probeNode(node *nodeState) {
	defer metrics.MeasureSince([]string{"memberlist", "probeNode"}, time.Now())

	// We use our health awareness to scale the overall probe interval, so we
	// slow down if we detect problems. The ticker that calls us can handle
	// us running over the base interval, and will skip missed ticks.
	// 我们使用运行状况感知来扩展整个探测间隔，因此在检测到问题时，我们会放慢速度。
	// 调用我们的计时器可以处理我们在基本间隔上运行的情况，并跳过错过的节拍。
	// 计算个超时时间
	probeInterval := m.awareness.ScaleTimeout(m.config.ProbeInterval)
	if probeInterval > m.config.ProbeInterval {
		metrics.IncrCounter([]string{"memberlist", "degraded", "probe"}, 1)
	}

	// Prepare a ping message and setup an ack handler.
	// 构建ping的请求体已经 异步ack的channels
	selfAddr, selfPort := m.getAdvertise()
	ping := ping{
		SeqNo:      m.nextSeqNo(),
		Node:       node.Name,
		SourceAddr: selfAddr,
		SourcePort: selfPort,
		SourceNode: m.config.Name,
	}

	// 直接ping+间接ping的节点数量
	// probeInterval是整个的周期时间，m.config.ProbeTimeout会提前结束阻塞，
	// 剩余的时间进行间接ping，ack依然可以通过achCh获取
	ackCh := make(chan ackMessage, m.config.IndirectChecks+1) // 对ping进行相应的通道
	nackCh := make(chan struct{}, m.config.IndirectChecks+1)
	m.setProbeChannels(ping.SeqNo, ackCh, nackCh, probeInterval)

	// Mark the sent time here, which should be after any pre-processing but
	// before system calls to do the actual send. This probably over-reports
	// a bit, but it's the best we can do. We had originally put this right
	// after the I/O, but that would sometimes give negative RTT measurements
	// which was not desirable.
	// 在这里标记发送时间，它应该在任何预处理之后，但在进行实际发送的系统调用之前。
	// 这可能有点夸大其词，但这是我们能做的最好的了。
	// 我们最初把它放在I/O之后，但这有时会给负RTT测量，这是不可取的。
	sent := time.Now()

	// Send a ping to the node. If this node looks like it's suspect or dead,
	// also tack on a suspect message so that it has a chance to refute as
	// soon as possible.
	// 发送一个ping到节点。如果该节点看起来是可疑的或已死亡的，也要添加可疑消息，以便它有机会尽快反驳。
	deadline := sent.Add(probeInterval)
	addr := node.Address()

	// Arrange for our self-awareness to get updated.
	var awarenessDelta int
	defer func() {
		m.awareness.ApplyDelta(awarenessDelta)
	}()

	// 对alive的节点以及可疑的dead节点发送ping信息
	// alive节点直接发送ping信息
	// 可疑的dead节点发送ping信息以及suspect信息
	// 如果是网络失败就直接执行HANDLE_REMOTE_FAILURE
	if node.State == StateAlive {
		if err := m.encodeAndSendMsg(node.FullAddress(), pingMsg, &ping); err != nil {
			m.logger.Printf("[ERR] memberlist: Failed to send ping: %s", err)
			if failedRemote(err) {
				goto HANDLE_REMOTE_FAILURE
			} else {
				return
			}
		}
	} else {
		// 可疑的dead节点
		var msgs [][]byte
		if buf, err := encode(pingMsg, &ping); err != nil {
			m.logger.Printf("[ERR] memberlist: Failed to encode ping message: %s", err)
			return
		} else {
			msgs = append(msgs, buf.Bytes())
		}

		// 直接向对方发送suspect消息,对方收到后如果状态是alive会进行广播反驳
		s := suspect{Incarnation: node.Incarnation, Node: node.Name, From: m.config.Name}
		if buf, err := encode(suspectMsg, &s); err != nil {
			m.logger.Printf("[ERR] memberlist: Failed to encode suspect message: %s", err)
			return
		} else {
			msgs = append(msgs, buf.Bytes())
		}

		compound := makeCompoundMessage(msgs)
		if err := m.rawSendMsgPacket(node.FullAddress(), &node.Node, compound.Bytes()); err != nil {
			m.logger.Printf("[ERR] memberlist: Failed to send compound ping and suspect message to %s: %s", addr, err)
			if failedRemote(err) {
				goto HANDLE_REMOTE_FAILURE
			} else {
				return
			}
		}
	}

	// Arrange for our self-awareness to get updated. At this point we've
	// sent the ping, so any return statement means the probe succeeded
	// which will improve our health until we get to the failure scenarios
	// at the end of this function, which will alter this delta variable
	// accordingly.
	// 此时，我们已经发送了ping，因此任何return语句都意味着探测成功，这将改善我们的运行状况，
	// 直到我们到达这个函数末尾的失败场景，这将相应地改变这个delta变量。
	awarenessDelta = -1

	// Wait for response or round-trip-time.
	// 等待响应或往返时间 不处理可疑节点信息
	select {
	case v := <-ackCh: // 在探测周期内响应
		if v.Complete == true {
			if m.config.Ping != nil { // serf层计算坐标
				rtt := v.Timestamp.Sub(sent)
				m.config.Ping.NotifyPingComplete(&node.Node, rtt, v.Payload)
			}
			return // 正常响应直接退出
		}

		// As an edge case, if we get a timeout, we need to re-enqueue it
		// here to break out of the select below.
		// 超时未响应
		if v.Complete == false {
			ackCh <- v
		}
	case <-time.After(m.config.ProbeTimeout):
		// Note that we don't scale this timeout based on awareness and
		// the health score. That's because we don't really expect waiting
		// longer to help get UDP through. Since health does extend the
		// probe interval it will give the TCP fallback more time, which
		// is more active in dealing with lost packets, and it gives more
		// time to wait for indirect acks/nacks.
		// 配置的网络超时时间，要小于探测的周期时间，尽快失败
		// 周期内剩余的时间进行间接ping已经tcp ping
		m.logger.Printf("[DEBUG] memberlist: Failed ping: %s (timeout reached)", node.Name)
	}

	// 直接ping未响应
HANDLE_REMOTE_FAILURE:
	// Get some random live nodes.
	// 随机选出m.config.IndirectChecks个alive的node
	m.nodeLock.RLock()
	kNodes := kRandomNodes(m.config.IndirectChecks, m.nodes, func(n *nodeState) bool {
		return n.Name == m.config.Name ||
			n.Name == node.Name ||
			n.State != StateAlive
	})
	m.nodeLock.RUnlock()

	// Attempt an indirect ping.
	// 构建并发送indirectPingMsg
	expectedNacks := 0 // 需要进行nack的数量
	selfAddr, selfPort = m.getAdvertise()
	ind := indirectPingReq{
		SeqNo:      ping.SeqNo,
		Target:     node.Addr,
		Port:       node.Port,
		Node:       node.Name,
		SourceAddr: selfAddr,
		SourcePort: selfPort,
		SourceNode: m.config.Name,
	}
	for _, peer := range kNodes {
		// We only expect nack to be sent from peers who understand
		// version 4 of the protocol.
		if ind.Nack = peer.PMax >= 4; ind.Nack {
			expectedNacks++
		}

		if err := m.encodeAndSendMsg(peer.FullAddress(), indirectPingMsg, &ind); err != nil {
			m.logger.Printf("[ERR] memberlist: Failed to send indirect ping: %s", err)
		}
	}

	// Also make an attempt to contact the node directly over TCP. This
	// helps prevent confused clients who get isolated from UDP traffic
	// but can still speak TCP (which also means they can possibly report
	// misinformation to other nodes via anti-entropy), avoiding flapping in
	// the cluster.
	// 还可以尝试通过TCP直接联系节点。
	// 这有助于防止混淆的客户端与UDP通信隔离，但仍然可以使用TCP(这也意味着他们可能通过反熵向其他节点报告错误信息)，
	// 避免在集群中震荡。
	//
	// This is a little unusual because we will attempt a TCP ping to any
	// member who understands version 3 of the protocol, regardless of
	// which protocol version we are speaking. That's why we've included a
	// config option to turn this off if desired.
	// 这有点不寻常，因为我们将尝试对任何理解协议版本3的成员进行TCP ping，而不管我们说的是哪个协议版本。
	// 这就是为什么我们包含了一个配置选项，可以在需要时关闭此功能。
	fallbackCh := make(chan bool, 1)

	// 是否禁用tcpPing
	disableTcpPings := m.config.DisableTcpPings ||
		(m.config.DisableTcpPingsForNode != nil && m.config.DisableTcpPingsForNode(node.Name))
	if (!disableTcpPings) && (node.PMax >= 3) {
		go func() {
			defer close(fallbackCh)
			didContact, err := m.sendPingAndWaitForAck(node.FullAddress(), ping, deadline)
			if err != nil {
				m.logger.Printf("[ERR] memberlist: Failed fallback ping: %s", err)
			} else {
				fallbackCh <- didContact
			}
		}()
	} else {
		close(fallbackCh)
	}

	// Wait for the acks or timeout. Note that we don't check the fallback
	// channel here because we want to issue a warning below if that's the
	// *only* way we hear back from the peer, so we have to let this time
	// out first to allow the normal UDP-based acks to come in.
	// 等待ack或超时。请注意，我们在这里没有检查后备通道，因为如果这是我们从对等端收到反馈的唯一方式，
	// 我们想在下面发出警告，所以我们必须先释放这段时间，以允许正常的基于udp的ack进入。
	// 阻塞到超时结束
	select {
	case v := <-ackCh:
		if v.Complete == true { // 多个节点进行间接ping，有一个ack响应就意味着正常，直接退出
			return
		}
	}

	// Finally, poll the fallback channel. The timeouts are set such that
	// the channel will have something or be closed without having to wait
	// any additional time here.
	// 最后，调查后备通道。通过设置超时，通道将有一些东西或被关闭，而无需在这里等待任何额外的时间。
	// tcp的ping响应 tcp的超时时间是周期时间
	for didContact := range fallbackCh {
		if didContact { // tcp的ping请求成功
			m.logger.Printf("[WARN] memberlist: Was able to connect to %s but other probes failed, network may be misconfigured", node.Name)
			return
		}
	}

	// Update our self-awareness based on the results of this failed probe.
	// If we don't have peers who will send nacks then we penalize for any
	// failed probe as a simple health metric. If we do have peers to nack
	// verify, then we can use that as a more sophisticated measure of self-
	// health because we assume them to be working, and they can help us
	// decide if the probed node was really dead or if it was something wrong
	// with ourselves.
	// 根据这次失败的调查结果更新我们的自我意识。
	// 如果我们没有发送nack的对等体，那么我们将对任何失败的探测进行惩罚，作为一个简单的运行状况度量。
	// 如果我们确实有一些节点需要nack来验证，那么我们可以将其作为一个更复杂的自我健康度量，
	// 因为我们假设它们在工作，它们可以帮助我们判断被探测节点是否真的死亡，或者是否我们自己出了问题。
	awarenessDelta = 0
	if expectedNacks > 0 {
		if nackCount := len(nackCh); nackCount < expectedNacks {
			awarenessDelta += (expectedNacks - nackCount)
		}
	} else {
		awarenessDelta += 1
	}

	// No acks received from target, suspect it as failed.
	// 没有收到任何ack，可能是对方节点的网络问题，把对方节点设置成suspect 不确定是不是节点不存在了
	m.logger.Printf("[INFO] memberlist: Suspect %s has failed, no acks received", node.Name)
	s := suspect{Incarnation: node.Incarnation, Node: node.Name, From: m.config.Name}
	m.suspectNode(&s) // 这里会广播suspectMsg的消息
}

// Ping initiates a ping to the node with the specified name.
func (m *Memberlist) Ping(node string, addr net.Addr) (time.Duration, error) {
	// Prepare a ping message and setup an ack handler.
	selfAddr, selfPort := m.getAdvertise()
	ping := ping{
		SeqNo:      m.nextSeqNo(),
		Node:       node,
		SourceAddr: selfAddr,
		SourcePort: selfPort,
		SourceNode: m.config.Name,
	}
	ackCh := make(chan ackMessage, m.config.IndirectChecks+1)
	m.setProbeChannels(ping.SeqNo, ackCh, nil, m.config.ProbeInterval)

	a := Address{Addr: addr.String(), Name: node}

	// Send a ping to the node.
	if err := m.encodeAndSendMsg(a, pingMsg, &ping); err != nil {
		return 0, err
	}

	// Mark the sent time here, which should be after any pre-processing and
	// system calls to do the actual send. This probably under-reports a bit,
	// but it's the best we can do.
	sent := time.Now()

	// Wait for response or timeout.
	select {
	case v := <-ackCh:
		if v.Complete == true {
			return v.Timestamp.Sub(sent), nil
		}
	case <-time.After(m.config.ProbeTimeout):
		// Timeout, return an error below.
	}

	m.logger.Printf("[DEBUG] memberlist: Failed UDP ping: %v (timeout reached)", node)
	return 0, NoPingResponseError{ping.Node}
}

// resetNodes is used when the tick wraps around. It will reap the
// dead nodes and shuffle the node list.
// 清理掉dead的node 并对剩下的node进行洗牌
func (m *Memberlist) resetNodes() {
	m.nodeLock.Lock()
	defer m.nodeLock.Unlock()

	// Move dead nodes, but respect gossip to the dead interval
	// 重置dead节点
	deadIdx := moveDeadNodes(m.nodes, m.config.GossipToTheDeadTime)

	// Deregister the dead nodes
	// 删除dead节点
	for i := deadIdx; i < len(m.nodes); i++ {
		delete(m.nodeMap, m.nodes[i].Name)
		m.nodes[i] = nil
	}

	// Trim the nodes to exclude the dead nodes
	// 重置nodes大小
	m.nodes = m.nodes[0:deadIdx]

	// Update numNodes after we've trimmed the dead nodes
	// 更新最新的非dead节点数量
	atomic.StoreUint32(&m.numNodes, uint32(deadIdx))

	// Shuffle live nodes
	shuffleNodes(m.nodes)
}

// gossip is invoked every GossipInterval period to broadcast our gossip
// messages to a few random nodes.
// 每隔一段时间就会调用流言蜚语，向一些随机节点广播我们的流言消息。
func (m *Memberlist) gossip() {
	defer metrics.MeasureSince([]string{"memberlist", "gossip"}, time.Now())

	// Get some random live, suspect, or recently dead nodes
	// 随机选择m.config.GossipNodes个节点
	m.nodeLock.RLock()
	kNodes := kRandomNodes(m.config.GossipNodes, m.nodes, func(n *nodeState) bool {
		if n.Name == m.config.Name { // 去除自身节点
			return true
		}

		switch n.State {
		case StateAlive, StateSuspect: // 可疑的node也可以
			return false

		case StateDead:
			return time.Since(n.StateChange) > m.config.GossipToTheDeadTime

		default:
			return true
		}
	})
	m.nodeLock.RUnlock()

	// Compute the bytes available
	// compound报文
	bytesAvail := m.config.UDPBufferSize - compoundHeaderOverhead
	if m.config.EncryptionEnabled() {
		bytesAvail -= encryptOverhead(m.encryptionVersion()) // 加密需要的额外空间
	}

	// 每个节点发送的消息不同
	for _, node := range kNodes {
		// Get any pending broadcasts
		// 获取需要广播的消息
		msgs := m.getBroadcasts(compoundOverhead, bytesAvail)
		if len(msgs) == 0 {
			return
		}

		addr := node.Address()
		if len(msgs) == 1 {
			// Send single message as is
			if err := m.rawSendMsgPacket(node.FullAddress(), &node.Node, msgs[0]); err != nil {
				m.logger.Printf("[ERR] memberlist: Failed to send gossip to %s: %s", addr, err)
			}
		} else {
			// Otherwise create and send a compound message
			compound := makeCompoundMessage(msgs)
			if err := m.rawSendMsgPacket(node.FullAddress(), &node.Node, compound.Bytes()); err != nil {
				m.logger.Printf("[ERR] memberlist: Failed to send gossip to %s: %s", addr, err)
			}
		}
	}
}

// pushPull is invoked periodically to randomly perform a complete state
// exchange. Used to ensure a high level of convergence, but is also
// reasonably expensive as the entire state of this node is exchanged
// with the other node.
// 周期性地调用pushPull来随机地执行一个完整的状态交换。
// 用于确保高水平的收敛，但由于这个节点的整个状态要与其他节点交换，因此成本也相当高。
func (m *Memberlist) pushPull() {
	// Get a random live node
	// 随机获取一个alive节点
	m.nodeLock.RLock()
	nodes := kRandomNodes(1, m.nodes, func(n *nodeState) bool {
		return n.Name == m.config.Name ||
			n.State != StateAlive
	})
	m.nodeLock.RUnlock()

	// If no nodes, bail
	// 没有随机到
	if len(nodes) == 0 {
		return
	}
	node := nodes[0]

	// Attempt a push pull
	// 尝试推拉
	if err := m.pushPullNode(node.FullAddress(), false); err != nil {
		m.logger.Printf("[ERR] memberlist: Push/Pull with %s failed: %s", node.Name, err)
	}
}

// pushPullNode does a complete state exchange with a specific node.
// 与特定节点进行完整的状态交换。
func (m *Memberlist) pushPullNode(a Address, join bool) error {
	defer metrics.MeasureSince([]string{"memberlist", "pushPullNode"}, time.Now())

	// Attempt to send and receive with the node
	// 尝试用节点发送和接收
	remote, userState, err := m.sendAndReceiveState(a, join)
	if err != nil {
		return err
	}

	if err := m.mergeRemoteState(join, remote, userState); err != nil {
		return err
	}
	return nil
}

// verifyProtocol verifies that all the remote nodes can speak with our
// nodes and vice versa on both the core protocol as well as the
// delegate protocol level.
//
// The verification works by finding the maximum minimum and
// minimum maximum understood protocol and delegate versions. In other words,
// it finds the common denominator of protocol and delegate version ranges
// for the entire cluster.
//
// After this, it goes through the entire cluster (local and remote) and
// verifies that everyone's speaking protocol versions satisfy this range.
// If this passes, it means that every node can understand each other.
// verifyProtocol验证所有远程节点可以与我们的节点通信，反之亦然，在核心协议以及委托协议级别上。
// 验证工作通过找到最大最小值和最小最大可理解的协议和委托版本。
// 换句话说，它找到了整个集群的协议和委托版本范围的共同点。
// 在此之后，它将遍历整个集群(本地和远程)，并验证每个人的会话协议版本是否满足此范围。
// 如果通过，就意味着每个节点都可以相互理解。
func (m *Memberlist) verifyProtocol(remote []pushNodeState) error {
	m.nodeLock.RLock()
	defer m.nodeLock.RUnlock()

	// Maximum minimum understood and minimum maximum understood for both
	// the protocol and delegate versions. We use this to verify everyone
	// can be understood.
	// 协议和委托版本的最大最小理解和最小最大理解。我们用这个来验证每个人都能被理解。
	var maxpmin, minpmax uint8
	var maxdmin, mindmax uint8
	minpmax = math.MaxUint8
	mindmax = math.MaxUint8

	// 远端传来的nodes的最大 最小范围
	for _, rn := range remote {
		// If the node isn't alive, then skip it
		// 如果节点非alive，则跳过它
		if rn.State != StateAlive {
			continue
		}

		// Skip nodes that don't have versions set, it just means
		// their version is zero.
		// 跳过没有设置版本的节点，这意味着它们的版本为0。
		if len(rn.Vsn) == 0 {
			continue
		}

		// []uint8{ n.PMin, n.PMax, n.PCur, n.DMin, n.DMax, n.DCur}

		// 所有nodes里最大的 PMin
		if rn.Vsn[0] > maxpmin {
			maxpmin = rn.Vsn[0]
		}

		// 所有nodes里最小的 PMax
		if rn.Vsn[1] < minpmax {
			minpmax = rn.Vsn[1]
		}

		// 所有nodes里最大的 DMin
		if rn.Vsn[3] > maxdmin {
			maxdmin = rn.Vsn[3]
		}

		// 所有nodes里最小的 DMax
		if rn.Vsn[4] < mindmax {
			mindmax = rn.Vsn[4]
		}
	}

	// 本地nodes的最大 最小范围
	for _, n := range m.nodes {
		// Ignore non-alive nodes
		if n.State != StateAlive {
			continue
		}

		if n.PMin > maxpmin {
			maxpmin = n.PMin
		}

		if n.PMax < minpmax {
			minpmax = n.PMax
		}

		if n.DMin > maxdmin {
			maxdmin = n.DMin
		}

		if n.DMax < mindmax {
			mindmax = n.DMax
		}
	}

	// Now that we definitively know the minimum and maximum understood
	// version that satisfies the whole cluster, we verify that every
	// node in the cluster satisifies this.
	// 现在我们明确知道满足整个集群的最小和最大可理解版本，我们验证集群中的每个节点都满足这一点。
	for _, n := range remote {
		var nPCur, nDCur uint8
		if len(n.Vsn) > 0 {
			nPCur = n.Vsn[2]
			nDCur = n.Vsn[5]
		}

		if nPCur < maxpmin || nPCur > minpmax {
			return fmt.Errorf(
				"Node '%s' protocol version (%d) is incompatible: [%d, %d]",
				n.Name, nPCur, maxpmin, minpmax)
		}

		if nDCur < maxdmin || nDCur > mindmax {
			return fmt.Errorf(
				"Node '%s' delegate protocol version (%d) is incompatible: [%d, %d]",
				n.Name, nDCur, maxdmin, mindmax)
		}
	}

	for _, n := range m.nodes {
		nPCur := n.PCur
		nDCur := n.DCur

		if nPCur < maxpmin || nPCur > minpmax {
			return fmt.Errorf(
				"Node '%s' protocol version (%d) is incompatible: [%d, %d]",
				n.Name, nPCur, maxpmin, minpmax)
		}

		if nDCur < maxdmin || nDCur > mindmax {
			return fmt.Errorf(
				"Node '%s' delegate protocol version (%d) is incompatible: [%d, %d]",
				n.Name, nDCur, maxdmin, mindmax)
		}
	}

	return nil
}

// nextSeqNo returns a usable sequence number in a thread safe way
func (m *Memberlist) nextSeqNo() uint32 {
	return atomic.AddUint32(&m.sequenceNum, 1)
}

// nextIncarnation returns the next incarnation number in a thread safe way
// 获取node标识号
func (m *Memberlist) nextIncarnation() uint32 {
	return atomic.AddUint32(&m.incarnation, 1)
}

// skipIncarnation adds the positive offset to the incarnation number.
// 获取node标识号 保持递增
func (m *Memberlist) skipIncarnation(offset uint32) uint32 {
	return atomic.AddUint32(&m.incarnation, offset)
}

// estNumNodes is used to get the current estimate of the number of nodes
// estNumNodes用于获取当前时间的节点数量
func (m *Memberlist) estNumNodes() int {
	return int(atomic.LoadUint32(&m.numNodes))
}

// ping的ack消息内容
type ackMessage struct {
	Complete  bool // 如果超时设置为false
	Payload   []byte
	Timestamp time.Time
}

// setProbeChannels is used to attach the ackCh to receive a message when an ack
// with a given sequence number is received. The `complete` field of the message
// will be false on timeout. Any nack messages will cause an empty struct to be
// passed to the nackCh, which can be nil if not needed.
// 节点探测的响应异步ack处理
func (m *Memberlist) setProbeChannels(seqNo uint32, ackCh chan ackMessage, nackCh chan struct{}, timeout time.Duration) {
	// Create handler functions for acks and nacks
	ackFn := func(payload []byte, timestamp time.Time) {
		select {
		case ackCh <- ackMessage{true, payload, timestamp}:
		default:
		}
	}
	nackFn := func() {
		select {
		case nackCh <- struct{}{}:
		default:
		}
	}

	// Add the handlers
	ah := &ackHandler{ackFn, nackFn, nil}
	m.ackLock.Lock()
	m.ackHandlers[seqNo] = ah
	m.ackLock.Unlock()

	// Setup a reaping routing
	// 超时的定时器
	ah.timer = time.AfterFunc(timeout, func() {
		m.ackLock.Lock()
		delete(m.ackHandlers, seqNo)
		m.ackLock.Unlock()
		select {
		case ackCh <- ackMessage{false, nil, time.Now()}:
		default:
		}
	})
}

// setAckHandler is used to attach a handler to be invoked when an ack with a
// given sequence number is received. If a timeout is reached, the handler is
// deleted. This is used for indirect pings so does not configure a function
// for nacks.
// 间接ping的使用
func (m *Memberlist) setAckHandler(seqNo uint32, ackFn func([]byte, time.Time), timeout time.Duration) {
	// Add the handler
	ah := &ackHandler{ackFn, nil, nil}
	m.ackLock.Lock()
	m.ackHandlers[seqNo] = ah
	m.ackLock.Unlock()

	// Setup a reaping routing
	ah.timer = time.AfterFunc(timeout, func() {
		m.ackLock.Lock()
		delete(m.ackHandlers, seqNo)
		m.ackLock.Unlock()
	})
}

// Invokes an ack handler if any is associated, and reaps the handler immediately
// 执行对ack的处理 timestamp=>收到响应的时间
func (m *Memberlist) invokeAckHandler(ack ackResp, timestamp time.Time) {
	m.ackLock.Lock()
	ah, ok := m.ackHandlers[ack.SeqNo]
	delete(m.ackHandlers, ack.SeqNo)
	m.ackLock.Unlock()
	if !ok {
		return
	}
	ah.timer.Stop()                  // 停止超时定时器
	ah.ackFn(ack.Payload, timestamp) // 执行ack函数
}

// Invokes nack handler if any is associated.
// ping的nack
func (m *Memberlist) invokeNackHandler(nack nackResp) {
	m.ackLock.Lock()
	ah, ok := m.ackHandlers[nack.SeqNo]
	m.ackLock.Unlock()
	if !ok || ah.nackFn == nil {
		return
	}
	ah.nackFn()
}

// refute gossips an alive message in response to incoming information that we
// are suspect or dead. It will make sure the incarnation number beats the given
// accusedInc value, or you can supply 0 to just get the next incarnation number.
// This alters the node state that's passed in so this MUST be called while the
// nodeLock is held.
// 收到关于自身节点的异常消息时，进行反驳并广播更新
func (m *Memberlist) refute(me *nodeState, accusedInc uint32) {
	// Make sure the incarnation number beats the accusation.
	// 保证incarnation递增
	inc := m.nextIncarnation()
	if accusedInc >= inc {
		inc = m.skipIncarnation(accusedInc - inc + 1)
	}
	me.Incarnation = inc

	// Decrease our health because we are being asked to refute a problem.
	// 因为我们被要求反驳一个问题而降低我们的健康。 可能是网络问题导致的
	m.awareness.ApplyDelta(1)

	// Format and broadcast an alive message.
	// 对外广播最新的当前节点alive信息
	a := alive{
		Incarnation: inc,
		Node:        me.Name,
		Addr:        me.Addr,
		Port:        me.Port,
		Meta:        me.Meta,
		Vsn: []uint8{
			me.PMin, me.PMax, me.PCur,
			me.DMin, me.DMax, me.DCur,
		},
	}
	m.encodeAndBroadcast(me.Addr.String(), aliveMsg, a)
}

// aliveNode is invoked by the network layer when we get a message about a
// live node.
// 当我们得到关于活动节点的消息时，网络层调用aliveNode。 packetHandler()
func (m *Memberlist) aliveNode(a *alive, notify chan struct{}, bootstrap bool) {
	m.nodeLock.Lock()
	defer m.nodeLock.Unlock()
	state, ok := m.nodeMap[a.Node]

	// It is possible that during a Leave(), there is already an aliveMsg
	// in-queue to be processed but blocked by the locks above. If we let
	// that aliveMsg process, it'll cause us to re-join the cluster. This
	// ensures that we don't.
	// 在Leave()期间，可能已经有一个aliveMsg在队列中等待处理，但被上面的锁阻塞了。
	// 如果我们让这个aliveMsg生效，就会导致我们重新加入集群。这确保了我们不会。
	// 如果是当前节点 并且已leave 忽略
	if m.hasLeft() && a.Node == m.config.Name {
		return
	}

	// 校验memberlist的协议版本号
	if len(a.Vsn) >= 3 {
		pMin := a.Vsn[0]
		pMax := a.Vsn[1]
		pCur := a.Vsn[2]
		if pMin == 0 || pMax == 0 || pMin > pMax {
			m.logger.Printf("[WARN] memberlist: Ignoring an alive message for '%s' (%v:%d) because protocol version(s) are wrong: %d <= %d <= %d should be >0", a.Node, net.IP(a.Addr), a.Port, pMin, pCur, pMax)
			return
		}
	}

	// Invoke the Alive delegate if any. This can be used to filter out
	// alive messages based on custom logic. For example, using a cluster name.
	// Using a merge delegate is not enough, as it is possible for passive
	// cluster merging to still occur.
	// todo 调用活动委托(如果有的话)。这可用于根据自定义逻辑过滤掉活动消息。例如，使用集群名称。使用合并委托是不够的，因为仍然可能发生被动集群合并。
	// 活跃节点的回调接口 可以提供过滤的逻辑
	if m.config.Alive != nil {
		if len(a.Vsn) < 6 {
			m.logger.Printf("[WARN] memberlist: ignoring alive message for '%s' (%v:%d) because Vsn is not present",
				a.Node, net.IP(a.Addr), a.Port)
			return
		}
		node := &Node{
			Name: a.Node,
			Addr: a.Addr,
			Port: a.Port,
			Meta: a.Meta,
			PMin: a.Vsn[0],
			PMax: a.Vsn[1],
			PCur: a.Vsn[2],
			DMin: a.Vsn[3],
			DMax: a.Vsn[4],
			DCur: a.Vsn[5],
		}
		if err := m.config.Alive.NotifyAlive(node); err != nil {
			m.logger.Printf("[WARN] memberlist: ignoring alive message for '%s': %s",
				a.Node, err)
			return
		}
	}

	// Check if we've never seen this node before, and if not, then
	// store this node in our node map.
	var updatesNode bool
	// 活跃节点不存在
	if !ok {
		// 是否允许加入集群
		errCon := m.config.IPAllowed(a.Addr)
		if errCon != nil {
			m.logger.Printf("[WARN] memberlist: Rejected node %s (%v): %s", a.Node, net.IP(a.Addr), errCon)
			return
		}

		// 不存在就设置state的值
		state = &nodeState{
			Node: Node{
				Name: a.Node,
				Addr: a.Addr,
				Port: a.Port,
				Meta: a.Meta,
			},
			State: StateDead, // 下面会更新成alive
		}
		if len(a.Vsn) > 5 {
			state.PMin = a.Vsn[0]
			state.PMax = a.Vsn[1]
			state.PCur = a.Vsn[2]
			state.DMin = a.Vsn[3]
			state.DMax = a.Vsn[4]
			state.DCur = a.Vsn[5]
		}

		// Add to map
		// 添加到map
		m.nodeMap[a.Node] = state

		// Get a random offset. This is important to ensure
		// the failure detection bound is low on average. If all
		// nodes did an append, failure detection bound would be
		// very high.
		// 获取一个随机偏移量。这对于确保故障检测范围平均较低是很重要的。
		// 如果所有节点都执行了append操作，那么故障检测范围将非常大。
		// 随机插入 进行ping
		n := len(m.nodes)
		offset := randomOffset(n)

		// Add at the end and swap with the node at the offset
		m.nodes = append(m.nodes, state)
		m.nodes[offset], m.nodes[n] = m.nodes[n], m.nodes[offset]

		// Update numNodes after we've added a new node
		// 增加计数
		atomic.AddUint32(&m.numNodes, 1)
	} else {
		// Check if this address is different than the existing node unless the old node is dead.
		// 活跃节点已存在 是否ip || port变更
		if !bytes.Equal([]byte(state.Addr), a.Addr) || state.Port != a.Port {
			// 校验addr是否允许加入
			errCon := m.config.IPAllowed(a.Addr)
			if errCon != nil {
				m.logger.Printf("[WARN] memberlist: Rejected IP update from %v to %v for node %s: %s", a.Node, state.Addr, net.IP(a.Addr), errCon)
				return
			}
			// If DeadNodeReclaimTime is configured, check if enough time has elapsed since the node died.
			// dead状态所能持续的最大时间
			canReclaim := (m.config.DeadNodeReclaimTime > 0 &&
				time.Since(state.StateChange) > m.config.DeadNodeReclaimTime)

			// Allow the address to be updated if a dead node is being replaced.
			// 当前状态不是left || dead 不允许进行变更
			if state.State == StateLeft || (state.State == StateDead && canReclaim) {
				m.logger.Printf("[INFO] memberlist: Updating address for left or failed node %s from %v:%d to %v:%d",
					state.Name, state.Addr, state.Port, net.IP(a.Addr), a.Port)
				updatesNode = true
			} else {
				m.logger.Printf("[ERR] memberlist: Conflicting address for %s. Mine: %v:%d Theirs: %v:%d Old state: %v",
					state.Name, state.Addr, state.Port, net.IP(a.Addr), a.Port, state.State)

				// Inform the conflict delegate if provided
				// ip，port不同 name相同 && state不合法导致的冲突事件  新加入节点名称冲突
				if m.config.Conflict != nil {
					other := Node{
						Name: a.Node,
						Addr: a.Addr,
						Port: a.Port,
						Meta: a.Meta,
					}
					m.config.Conflict.NotifyConflict(&state.Node, &other)
				}
				return
			}
		}
	}

	// Bail if the incarnation number is older, and this is not about us
	isLocalNode := state.Name == m.config.Name
	//已存在 未改变 不是当前node 并且不是节点变更 incarnation小 说明过期消息
	if a.Incarnation <= state.Incarnation && !isLocalNode && !updatesNode {
		return
	}

	// Bail if strictly less and this is about us
	// 当前节点的 过期消息
	if a.Incarnation < state.Incarnation && isLocalNode {
		return
	}

	// 非过期的消息处理

	// Clear out any suspicion timer that may be in effect.
	// 清除任何可能起作用的可疑计时器。
	delete(m.nodeTimers, a.Node)

	// Store the old state and meta data
	oldState := state.State
	oldMeta := state.Meta

	// If this is us we need to refute, otherwise re-broadcast
	// 当前节点(名称相同) 并且非启动阶段 别的节点同步过来的
	if !bootstrap && isLocalNode {
		// Compute the version vector
		versions := []uint8{
			state.PMin, state.PMax, state.PCur,
			state.DMin, state.DMax, state.DCur,
		}

		// If the Incarnation is the same, we need special handling, since it
		// possible for the following situation to happen:
		// 1) Start with configuration C, join cluster
		// 2) Hard fail / Kill / Shutdown
		// 3) Restart with configuration C', join cluster
		//
		// In this case, other nodes and the local node see the same incarnation,
		// but the values may not be the same. For this reason, we always
		// need to do an equality check for this Incarnation. In most cases,
		// we just ignore, but we may need to refute.
		//
		// 相同配置重启之后 其他节点进行数据同步 如果确实相同就忽略
		if a.Incarnation == state.Incarnation &&
			bytes.Equal(a.Meta, state.Meta) &&
			bytes.Equal(a.Vsn, versions) {
			return
		}

		// 当前节点信息有变更
		m.refute(state, a.Incarnation)
		m.logger.Printf("[WARN] memberlist: Refuting an alive message for '%s' (%v:%d) meta:(%v VS %v), vsn:(%v VS %v)", a.Node, net.IP(a.Addr), a.Port, a.Meta, state.Meta, a.Vsn, versions)
	} else {
		// 不是当前节点 需要进行广播
		m.encodeBroadcastNotify(a.Node, aliveMsg, a, notify)

		// Update protocol versions if it arrived
		if len(a.Vsn) > 0 {
			state.PMin = a.Vsn[0]
			state.PMax = a.Vsn[1]
			state.PCur = a.Vsn[2]
			state.DMin = a.Vsn[3]
			state.DMax = a.Vsn[4]
			state.DCur = a.Vsn[5]
		}

		// Update the state and incarnation number
		// 更新成alive状态
		state.Incarnation = a.Incarnation
		state.Meta = a.Meta
		state.Addr = a.Addr
		state.Port = a.Port
		if state.State != StateAlive {
			state.State = StateAlive
			state.StateChange = time.Now()
		}
	}

	// Update metrics
	metrics.IncrCounter([]string{"memberlist", "msg", "alive"}, 1)

	// Notify the delegate of any relevant updates
	// serf层的事件回调
	if m.config.Events != nil {
		if oldState == StateDead || oldState == StateLeft {
			// if Dead/Left -> Alive, notify of join
			m.config.Events.NotifyJoin(&state.Node)

		} else if !bytes.Equal(oldMeta, state.Meta) {
			// if Meta changed, trigger an update notification
			m.config.Events.NotifyUpdate(&state.Node)
		}
	}
}

// suspectNode is invoked by the network layer when we get a message
// about a suspect node
// 当我们获得关于可疑节点的消息时，网络层将调用susectnode
func (m *Memberlist) suspectNode(s *suspect) {
	m.nodeLock.Lock()
	defer m.nodeLock.Unlock()
	state, ok := m.nodeMap[s.Node]

	// If we've never heard about this node before, ignore it
	// 当前节点不存在此node
	if !ok {
		return
	}

	// Ignore old incarnation numbers
	// 过期的node信息
	if s.Incarnation < state.Incarnation {
		return
	}

	// See if there's a suspicion timer we can confirm. If the info is new
	// to us we will go ahead and re-gossip it. This allows for multiple
	// independent confirmations to flow even when a node probes a node
	// that's already suspect.
	// 看看有没有可以确认的嫌疑计时器。如果这个消息对我们来说是新消息，我们会继续传播它。
	// 这允许多个独立的确认流即使当一个节点探测一个已经可疑的节点。
	if timer, ok := m.nodeTimers[s.Node]; ok {
		if timer.Confirm(s.From) {
			// 收到的已标记suspect信息 直接进行广播
			m.encodeAndBroadcast(s.Node, suspectMsg, s)
		}
		return
	}

	// Ignore non-alive nodes
	// 已经被标记为非alive状态 忽略
	// ping 非alive的节点就不管了
	// ping alive节点但是没有收到ack
	if state.State != StateAlive {
		return
	}

	// If this is us we need to refute, otherwise re-broadcast
	// 自身节点收到suspect并且state=alive就进行反驳
	if state.Name == m.config.Name {
		m.refute(state, s.Incarnation)
		m.logger.Printf("[WARN] memberlist: Refuting a suspect message (from: %s)", s.From)
		return // Do not mark ourself suspect
	} else {
		// 其他节点进行广播
		m.encodeAndBroadcast(s.Node, suspectMsg, s)
	}

	// Update metrics
	metrics.IncrCounter([]string{"memberlist", "msg", "suspect"}, 1)

	// Update the state
	// 在当前节点下把此节点标记为suspect
	state.Incarnation = s.Incarnation
	state.State = StateSuspect
	changeTime := time.Now()
	state.StateChange = changeTime

	// Setup a suspicion timer. Given that we don't have any known phase
	// relationship with our peers, we set up k such that we hit the nominal
	// timeout two probe intervals short of what we expect given the suspicion
	// multiplier.
	// 设置一个怀疑计时器。
	// 假设我们与我们的同伴没有任何已知的相位关系，
	// 我们设置k，使我们达到的超时时间比我们已知的怀疑乘数的预期时间间隔短2个探测间隔。
	// 期望获取确认的节点数
	k := m.config.SuspicionMult - 2 // (减去自身以及被探测的节点)

	// If there aren't enough nodes to give the expected confirmations, just
	// set k to 0 to say that we don't expect any. Note we subtract 2 from n
	// here to take out ourselves and the node being probed.
	// 如果没有足够的节点给出预期的确认，只需将k(期望获取确认的节点数)设为0，表示不需要任何确认。
	// 注意这里我们从n中减去2来取出我们自己和被探测的节点。
	n := m.estNumNodes()
	if n-2 < k {
		k = 0
	}

	// Compute the timeouts based on the size of the cluster.
	// 根据集群的大小计算超时。 最小以及最大的周期数
	min := suspicionTimeout(m.config.SuspicionMult, n, m.config.ProbeInterval)
	max := time.Duration(m.config.SuspicionMaxTimeoutMult) * min
	// 超时函数 numConfirmations:收到的确认节点数
	fn := func(numConfirmations int) {
		m.nodeLock.Lock()
		state, ok := m.nodeMap[s.Node]
		// 标记为suspect后 changeTime没有变 状态没有被变更过
		timeout := ok && state.State == StateSuspect && state.StateChange == changeTime
		m.nodeLock.Unlock()

		// 不管收到的确认数够不够 超时之后都会把节点进行降级
		if timeout {
			// 这个k(需要确认的节点数) 并不是有k个节点同步此node为suspect才更新成suspect
			// 不管有没有达到k个确认，最后都会变成成dead
			// k只是会影响变更成dead的时间，如果达到k个节点，那么经过min的时间就会变更成dead
			if k > 0 && numConfirmations < k {
				metrics.IncrCounter([]string{"memberlist", "degraded", "timeout"}, 1)
			}

			m.logger.Printf("[INFO] memberlist: Marking %s as failed, suspect timeout reached (%d peer confirmations)",
				state.Name, numConfirmations)
			d := dead{Incarnation: state.Incarnation, Node: state.Name, From: m.config.Name}
			m.deadNode(&d) // 降级成dead
		}
	}
	m.nodeTimers[s.Node] = newSuspicion(s.From, k, min, max, fn)
}

// deadNode is invoked by the network layer when we get a message
// about a dead node
// 当我们得到关于死节点的消息时，网络层会调用deadNode
func (m *Memberlist) deadNode(d *dead) {
	m.nodeLock.Lock()
	defer m.nodeLock.Unlock()
	state, ok := m.nodeMap[d.Node]

	// If we've never heard about this node before, ignore it
	// 如果我们以前从未听说过这个节点，请忽略它
	if !ok {
		return
	}

	// Ignore old incarnation numbers
	// 过期的消息
	if d.Incarnation < state.Incarnation {
		return
	}

	// Clear out any suspicion timer that may be in effect.
	// 明确需要进行降级了 就删除suspicion信息
	delete(m.nodeTimers, d.Node)

	// Ignore if node is already dead
	// 已经dead || left了
	if state.DeadOrLeft() {
		return
	}

	// Check if this is us
	// 自身节点收到被动的dead信息
	if state.Name == m.config.Name {
		// If we are not leaving we need to refute
		// 如果自身节点不是dead || left状态就进行refute
		if !m.hasLeft() {
			m.refute(state, d.Incarnation)
			m.logger.Printf("[WARN] memberlist: Refuting a dead message (from: %s)", d.From)
			return // Do not mark ourself dead
		}

		// If we are leaving, we broadcast and wait
		// 如果已经leaving，就广播然后等待  等待消息已经被网络发出
		m.encodeBroadcastNotify(d.Node, deadMsg, d, m.leaveBroadcast)
	} else {
		// 非自身节点直接广播
		m.encodeAndBroadcast(d.Node, deadMsg, d)
	}

	// Update metrics
	metrics.IncrCounter([]string{"memberlist", "msg", "dead"}, 1)

	// Update the state
	// 最新的Incarnation
	state.Incarnation = d.Incarnation

	// If the dead message was send by the node itself, mark it is left
	// instead of dead.
	// 如果dead消息是由节点本身发送的，则将其标记为left而不是dead。
	if d.Node == d.From {
		state.State = StateLeft
	} else { // 别的节点通过ping广播的dead
		state.State = StateDead
	}
	state.StateChange = time.Now()

	// Notify of death
	// 通知serf层节点dead
	if m.config.Events != nil {
		m.config.Events.NotifyLeave(&state.Node)
	}
}

// mergeState is invoked by the network layer when we get a Push/Pull
// state transfer
// 当我们获得Push/Pull状态传输时，网络层会调用mergeState
func (m *Memberlist) mergeState(remote []pushNodeState) {
	for _, r := range remote {
		switch r.State {
		case StateAlive: // 设置成alive
			a := alive{
				Incarnation: r.Incarnation,
				Node:        r.Name,
				Addr:        r.Addr,
				Port:        r.Port,
				Meta:        r.Meta,
				Vsn:         r.Vsn,
			}
			m.aliveNode(&a, nil, false)

		case StateLeft: // 设置成left
			d := dead{Incarnation: r.Incarnation, Node: r.Name, From: r.Name}
			m.deadNode(&d)
		case StateDead: // 设置成suspect
			// If the remote node believes a node is dead, we prefer to
			// suspect that node instead of declaring it dead instantly
			// 如果远程节点认为某个节点已死，我们宁愿怀疑该节点，而不是立即声明它已死
			fallthrough
		case StateSuspect:
			s := suspect{Incarnation: r.Incarnation, Node: r.Name, From: m.config.Name}
			m.suspectNode(&s)
		}
	}
}
