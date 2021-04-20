package memberlist

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"sync/atomic"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/go-msgpack/codec"
)

// This is the minimum and maximum protocol version that we can
// _understand_. We're allowed to speak at any version within this
// range. This range is inclusive.
const (
	ProtocolVersionMin uint8 = 1 // memberlist层支持的协议最小版本号

	// Version 3 added support for TCP pings but we kept the default
	// protocol version at 2 to ease transition to this new feature.
	// A memberlist speaking version 2 of the protocol will attempt
	// to TCP ping another memberlist who understands version 3 or
	// greater.
	//
	// Version 4 added support for nacks as part of indirect probes.
	// A memberlist speaking version 2 of the protocol will expect
	// nacks from another memberlist who understands version 4 or
	// greater, and likewise nacks will be sent to memberlists who
	// understand version 4 or greater.
	ProtocolVersion2Compatible = 2

	ProtocolVersionMax = 5 // memberlist层支持的协议最大版本号
)

// messageType is an integer ID of a type of message that can be received
// on network channels from other members.
type messageType uint8

// The list of available message types.
// 有效的消息类型
const (
	pingMsg         messageType = iota // ping请求
	indirectPingMsg                    // 间接ping
	ackRespMsg                         // ping的ack消息
	suspectMsg                         // 可疑的dead消息
	aliveMsg                           // alive消息
	deadMsg                            // dead消息
	pushPullMsg                        // tcp节点同步
	compoundMsg                        // 复合消息类型
	userMsg                            // serf层的消息标记 member层不处理 User mesg, not handled by us
	compressMsg                        // 压缩类型
	encryptMsg                         // 加密消息
	nackRespMsg                        // ping的nack消息
	hasCrcMsg                          // crc校验码的消息
	errMsg
)

// compressionType is used to specify the compression algorithm
type compressionType uint8

const (
	lzwAlgo compressionType = iota
)

const (
	MetaMaxSize            = 512                   // Maximum size for node meta data
	compoundHeaderOverhead = 2                     // Assumed header overhead  复合类型的消息头占用的字节长度 2个unit8(类型+长度)
	compoundOverhead       = 2                     // Assumed overhead per entry in compoundHeader
	userMsgOverhead        = 1                     // self层的消息进行打包发送时额外消耗的字节长度
	blockingWarning        = 10 * time.Millisecond // Warn if a UDP packet takes this long to process
	maxPushStateBytes      = 20 * 1024 * 1024
	maxPushPullRequests    = 128 // Maximum number of concurrent push/pull requests
)

// ping request sent directly to node
// ping的消息内容
type ping struct {
	SeqNo uint32 // 请求序列号

	// Node is sent so the target can verify they are
	// the intended recipient. This is to protect again an agent
	// restart with a new name.
	// 目标节点名称 用来校验目标节点是否丢弃消息 (目标节点改名)
	Node string

	// 发送方地址
	SourceAddr []byte `codec:",omitempty"` // Source address, used for a direct reply
	// 发送方端口
	SourcePort uint16 `codec:",omitempty"` // Source port, used for a direct reply
	// 发送方节点名称
	SourceNode string `codec:",omitempty"` // Source name, used for a direct reply
}

// indirect ping sent to an indirect node
// 间接ping的消息
type indirectPingReq struct {
	SeqNo  uint32 // 序列号
	Target []byte // 目标node地址   不是间接节点的信息
	Port   uint16 // 目标node端口号

	// Node is sent so the target can verify they are
	// the intended recipient. This is to protect against an agent
	// restart with a new name.
	Node string // 目标node名称

	Nack bool // true if we'd like a nack back

	SourceAddr []byte `codec:",omitempty"` // Source address, used for a direct reply
	SourcePort uint16 `codec:",omitempty"` // Source port, used for a direct reply
	SourceNode string `codec:",omitempty"` // Source name, used for a direct reply
}

// ack response is sent for a ping
// ping的相应消息
type ackResp struct {
	SeqNo   uint32 // 请求序列号
	Payload []byte // 内容
}

// nack response is sent for an indirect ping when the pinger doesn't hear from
// the ping-ee within the configured timeout. This lets the original node know
// that the indirect ping attempt happened but didn't succeed.
type nackResp struct {
	SeqNo uint32
}

// err response is sent to relay the error from the remote end
type errResp struct {
	Error string
}

// suspect is broadcast when we suspect a node is dead
// 可疑的消息
type suspect struct {
	Incarnation uint32 // 目的节点的incarnation
	Node        string // 目的节点的名称
	From        string // 来源节点的名称 Include who is suspecting
}

// alive is broadcast when we know a node is alive.
// Overloaded for nodes joining
// alive的节点信息
type alive struct {
	Incarnation uint32 // 编号 判断过期的alive消息
	Node        string // 名称
	Addr        []byte // Advertise ip地址
	Port        uint16 // Advertise 端口号
	Meta        []byte // tag

	// The versions of the protocol/delegate that are being spoken, order:
	// pmin, pmax, pcur, dmin, dmax, dcur
	Vsn []uint8 // 支持的版本号
}

// dead is broadcast when we confirm a node is dead
// Overloaded for nodes leaving
// 当我们确认一个节点因节点离开而超载时，将广播dead
type dead struct {
	Incarnation uint32 // dead节点的Incarnation
	Node        string // dead节点的nodeName
	From        string // 发送dead的nodeName 区分主动还是被动 Include who is suspecting
}

// pushPullHeader is used to inform the
// otherside how many states we are transferring
type pushPullHeader struct {
	Nodes        int
	UserStateLen int  // Encodes the byte lengh of user state
	Join         bool // Is this a join request or a anti-entropy run
}

// userMsgHeader is used to encapsulate a userMsg
type userMsgHeader struct {
	UserMsgLen int // Encodes the byte lengh of user state
}

// pushNodeState is used for pushPullReq when we are
// transferring out node states
type pushNodeState struct {
	Name        string
	Addr        []byte
	Port        uint16
	Meta        []byte
	Incarnation uint32
	State       NodeStateType
	Vsn         []uint8 // Protocol versions
}

// compress is used to wrap an underlying payload
// using a specified compression algorithm
type compress struct {
	Algo compressionType
	Buf  []byte
}

// msgHandoff is used to transfer a message between goroutines
type msgHandoff struct {
	msgType messageType
	buf     []byte
	from    net.Addr
}

// encryptionVersion returns the encryption version to use
func (m *Memberlist) encryptionVersion() encryptionVersion {
	switch m.ProtocolVersion() {
	case 1:
		return 0
	default:
		return 1
	}
}

// streamListen is a long running goroutine that pulls incoming streams from the
// transport and hands them off for processing.
// streamListen是一个长时间运行的goroutine，它将传入的流从传输中提取出来，并将它们交给处理。
// 处理已建立的tcp链接
func (m *Memberlist) streamListen() {
	for {
		select {
		case conn := <-m.transport.StreamCh():
			go m.handleConn(conn)

		case <-m.shutdownCh: // memberlist关闭
			return
		}
	}
}

// handleConn handles a single incoming stream connection from the transport.
// 处理conn链接 tcp链接
func (m *Memberlist) handleConn(conn net.Conn) {
	defer conn.Close() // 数据接收完就关闭 conn
	m.logger.Printf("[DEBUG] memberlist: Stream connection %s", LogConn(conn))

	metrics.IncrCounter([]string{"memberlist", "tcp", "accept"}, 1)

	// 设置conn的读写超时时间
	conn.SetDeadline(time.Now().Add(m.config.TCPTimeout))
	msgType, bufConn, dec, err := m.readStream(conn)
	if err != nil {
		if err != io.EOF { // err==io.EOF说明连接已经被关闭了，不需要进行响应
			m.logger.Printf("[ERR] memberlist: failed to receive: %s %s", err, LogConn(conn))

			// 构造响应报文
			resp := errResp{err.Error()}
			out, err := encode(errMsg, &resp)
			if err != nil {
				m.logger.Printf("[ERR] memberlist: Failed to encode error response: %s", err)
				return
			}

			// 发送数据
			err = m.rawSendMsgStream(conn, out.Bytes())
			if err != nil {
				m.logger.Printf("[ERR] memberlist: Failed to send error: %s %s", err, LogConn(conn))
				return
			}
		}
		return
	}

	switch msgType {
	case userMsg:
		if err := m.readUserMsg(bufConn, dec); err != nil {
			m.logger.Printf("[ERR] memberlist: Failed to receive user message: %s %s", err, LogConn(conn))
		}
	case pushPullMsg:
		// 收到pushPull消息后，校验同时处理pushPull的数量
		// 符合条件就直接回复自身的nodes节点
		// 合并的逻辑在回复之后
		// Increment counter of pending push/pulls
		numConcurrent := atomic.AddUint32(&m.pushPullReq, 1)
		defer atomic.AddUint32(&m.pushPullReq, ^uint32(0)) // 让x减1，调用AddUint32(&x, ^uint32(0))

		// Check if we have too many open push/pull requests
		// 限制同时进行pushPUll的请求数
		if numConcurrent >= maxPushPullRequests {
			m.logger.Printf("[ERR] memberlist: Too many pending push/pull requests")
			return
		}

		// 解析数据
		join, remoteNodes, userState, err := m.readRemoteState(bufConn, dec)
		if err != nil {
			m.logger.Printf("[ERR] memberlist: Failed to read remote state: %s %s", err, LogConn(conn))
			return
		}

		// 发送自身的nodes数据给对方
		if err := m.sendLocalState(conn, join); err != nil {
			m.logger.Printf("[ERR] memberlist: Failed to push local state: %s %s", err, LogConn(conn))
			return
		}

		// 合并对方的nodes数据
		if err := m.mergeRemoteState(join, remoteNodes, userState); err != nil {
			m.logger.Printf("[ERR] memberlist: Failed push/pull merge: %s %s", err, LogConn(conn))
			return
		}
	case pingMsg:
		var p ping
		if err := dec.Decode(&p); err != nil {
			m.logger.Printf("[ERR] memberlist: Failed to decode ping: %s %s", err, LogConn(conn))
			return
		}

		if p.Node != "" && p.Node != m.config.Name {
			m.logger.Printf("[WARN] memberlist: Got ping for unexpected node %s %s", p.Node, LogConn(conn))
			return
		}

		ack := ackResp{p.SeqNo, nil}
		out, err := encode(ackRespMsg, &ack)
		if err != nil {
			m.logger.Printf("[ERR] memberlist: Failed to encode ack: %s", err)
			return
		}

		err = m.rawSendMsgStream(conn, out.Bytes())
		if err != nil {
			m.logger.Printf("[ERR] memberlist: Failed to send ack: %s %s", err, LogConn(conn))
			return
		}
	default:
		m.logger.Printf("[ERR] memberlist: Received invalid msgType (%d) %s", msgType, LogConn(conn))
	}
}

// packetListen is a long running goroutine that pulls packets out of the
// transport and hands them off for processing.
// 处理udp请求数据
func (m *Memberlist) packetListen() {
	for {
		select {
		case packet := <-m.transport.PacketCh():
			m.ingestPacket(packet.Buf, packet.From, packet.Timestamp)

		case <-m.shutdownCh:
			return
		}
	}
}

// 接受并处理udp数据包
func (m *Memberlist) ingestPacket(buf []byte, from net.Addr, timestamp time.Time) {
	// Check if encryption is enabled
	if m.config.EncryptionEnabled() { // 加密
		// Decrypt the payload
		plain, err := decryptPayload(m.config.Keyring.GetKeys(), buf, nil)
		if err != nil {
			if !m.config.GossipVerifyIncoming {
				// Treat the message as plaintext
				plain = buf
			} else {
				m.logger.Printf("[ERR] memberlist: Decrypt packet failed: %v %s", err, LogAddress(from))
				return
			}
		}

		// Continue processing the plaintext buffer
		buf = plain
	}

	// See if there's a checksum included to verify the contents of the message
	if len(buf) >= 5 && messageType(buf[0]) == hasCrcMsg {
		crc := crc32.ChecksumIEEE(buf[5:])
		expected := binary.BigEndian.Uint32(buf[1:5])
		if crc != expected {
			m.logger.Printf("[WARN] memberlist: Got invalid checksum for UDP packet: %x, %x", crc, expected)
			return
		}
		m.handleCommand(buf[5:], from, timestamp)
	} else {
		m.handleCommand(buf, from, timestamp)
	}
}

// 处理upd数据包
func (m *Memberlist) handleCommand(buf []byte, from net.Addr, timestamp time.Time) {
	// Decode the message type
	msgType := messageType(buf[0])
	buf = buf[1:]

	// Switch on the msgType
	switch msgType {
	case compoundMsg:
		m.handleCompound(buf, from, timestamp)
	case compressMsg:
		m.handleCompressed(buf, from, timestamp)

	case pingMsg:
		m.handlePing(buf, from)
	case indirectPingMsg:
		m.handleIndirectPing(buf, from)
	case ackRespMsg:
		m.handleAck(buf, from, timestamp)
	case nackRespMsg:
		m.handleNack(buf, from)

	case suspectMsg:
		fallthrough
	case aliveMsg:
		fallthrough
	case deadMsg:
		fallthrough
	case userMsg:
		// Determine the message queue, prioritize alive
		queue := m.lowPriorityMsgQueue // 低优先级消息
		if msgType == aliveMsg {       // 高优先级消息
			queue = m.highPriorityMsgQueue
		}

		// Check for overflow and append if not full
		// 校验队列长度是否大于配置的限制
		m.msgQueueLock.Lock()
		if queue.Len() >= m.config.HandoffQueueDepth {
			m.logger.Printf("[WARN] memberlist: handler queue full, dropping message (%d) %s", msgType, LogAddress(from))
		} else {
			queue.PushBack(msgHandoff{msgType, buf, from})
		}
		m.msgQueueLock.Unlock()

		// Notify of pending message
		// queue满了也会进行通知
		select {
		case m.handoffCh <- struct{}{}:
		default:
		}

	default:
		m.logger.Printf("[ERR] memberlist: msg type (%d) not supported %s", msgType, LogAddress(from))
	}
}

// getNextMessage returns the next message to process in priority order, using LIFO
// 获取队列中的udp报文 优先处理高优先级任务
func (m *Memberlist) getNextMessage() (msgHandoff, bool) {
	m.msgQueueLock.Lock()
	defer m.msgQueueLock.Unlock()

	if el := m.highPriorityMsgQueue.Back(); el != nil {
		m.highPriorityMsgQueue.Remove(el)
		msg := el.Value.(msgHandoff)
		return msg, true
	} else if el := m.lowPriorityMsgQueue.Back(); el != nil {
		m.lowPriorityMsgQueue.Remove(el)
		msg := el.Value.(msgHandoff)
		return msg, true
	}
	return msgHandoff{}, false
}

// packetHandler is a long running goroutine that processes messages received
// over the packet interface, but is decoupled from the listener to avoid
// blocking the listener which may cause ping/ack messages to be delayed.
// udp请求的消息类型处理
func (m *Memberlist) packetHandler() {
	for {
		select {
		case <-m.handoffCh:
			for {
				msg, ok := m.getNextMessage()
				if !ok {
					break
				}
				msgType := msg.msgType
				buf := msg.buf
				from := msg.from

				switch msgType {
				case suspectMsg:
					m.handleSuspect(buf, from)
				case aliveMsg:
					m.handleAlive(buf, from)
				case deadMsg:
					m.handleDead(buf, from)
				case userMsg:
					m.handleUser(buf, from)
				default:
					m.logger.Printf("[ERR] memberlist: Message type (%d) not supported %s (packet handler)", msgType, LogAddress(from))
				}
			}

		case <-m.shutdownCh:
			return
		}
	}
}

func (m *Memberlist) handleCompound(buf []byte, from net.Addr, timestamp time.Time) {
	// Decode the parts
	trunc, parts, err := decodeCompoundMessage(buf)
	if err != nil {
		m.logger.Printf("[ERR] memberlist: Failed to decode compound request: %s %s", err, LogAddress(from))
		return
	}

	// Log any truncation
	if trunc > 0 {
		m.logger.Printf("[WARN] memberlist: Compound request had %d truncated messages %s", trunc, LogAddress(from))
	}

	// Handle each message
	for _, part := range parts {
		m.handleCommand(part, from, timestamp)
	}
}

// 响应ping消息
func (m *Memberlist) handlePing(buf []byte, from net.Addr) {
	var p ping
	if err := decode(buf, &p); err != nil {
		m.logger.Printf("[ERR] memberlist: Failed to decode ping request: %s %s", err, LogAddress(from))
		return
	}
	// If node is provided, verify that it is for us
	// 名称不匹配 直接丢弃
	if p.Node != "" && p.Node != m.config.Name {
		m.logger.Printf("[WARN] memberlist: Got ping for unexpected node '%s' %s", p.Node, LogAddress(from))
		return
	}

	// 构建响应内容
	var ack ackResp
	ack.SeqNo = p.SeqNo
	if m.config.Ping != nil {
		ack.Payload = m.config.Ping.AckPayload()
	}

	addr := ""
	if len(p.SourceAddr) > 0 && p.SourcePort > 0 {
		addr = joinHostPort(net.IP(p.SourceAddr).String(), p.SourcePort)
	} else {
		addr = from.String()
	}

	a := Address{
		Addr: addr,
		Name: p.SourceNode,
	}
	if err := m.encodeAndSendMsg(a, ackRespMsg, &ack); err != nil {
		m.logger.Printf("[ERR] memberlist: Failed to send ack: %s %s", err, LogAddress(from))
	}
}

// 间接ping
func (m *Memberlist) handleIndirectPing(buf []byte, from net.Addr) {
	var ind indirectPingReq
	if err := decode(buf, &ind); err != nil {
		m.logger.Printf("[ERR] memberlist: Failed to decode indirect ping request: %s %s", err, LogAddress(from))
		return
	}

	// For proto versions < 2, there is no port provided. Mask old
	// behavior by using the configured port.
	if m.ProtocolVersion() < 2 || ind.Port == 0 {
		ind.Port = uint16(m.config.BindPort)
	}

	// Send a ping to the correct host.
	localSeqNo := m.nextSeqNo()
	selfAddr, selfPort := m.getAdvertise()
	ping := ping{
		SeqNo: localSeqNo,
		Node:  ind.Node, // 目标名
		// The outbound message is addressed FROM us.
		SourceAddr: selfAddr,
		SourcePort: selfPort,
		SourceNode: m.config.Name,
	}

	// Forward the ack back to the requestor. If the request encodes an origin
	// use that otherwise assume that the other end of the UDP socket is
	// usable.
	indAddr := ""
	if len(ind.SourceAddr) > 0 && ind.SourcePort > 0 {
		indAddr = joinHostPort(net.IP(ind.SourceAddr).String(), ind.SourcePort)
	} else {
		indAddr = from.String()
	}

	// Setup a response handler to relay the ack
	// 注册ack的异步处理结果
	cancelCh := make(chan struct{})
	respHandler := func(payload []byte, timestamp time.Time) {
		// Try to prevent the nack if we've caught it in time.
		close(cancelCh)

		// 如果对方正常响应ping结果  把ack发送回原来的node
		ack := ackResp{ind.SeqNo, nil}
		a := Address{
			Addr: indAddr,
			Name: ind.SourceNode,
		}
		if err := m.encodeAndSendMsg(a, ackRespMsg, &ack); err != nil {
			m.logger.Printf("[ERR] memberlist: Failed to forward ack: %s %s", err, LogStringAddress(indAddr))
		}
	}
	// 超时时间不同
	m.setAckHandler(localSeqNo, respHandler, m.config.ProbeTimeout)

	// Send the ping.
	// 向对方发送ping信息
	addr := joinHostPort(net.IP(ind.Target).String(), ind.Port)
	a := Address{
		Addr: addr,
		Name: ind.Node,
	}
	if err := m.encodeAndSendMsg(a, pingMsg, &ping); err != nil {
		m.logger.Printf("[ERR] memberlist: Failed to send indirect ping: %s %s", err, LogStringAddress(indAddr))
	}

	// Setup a timer to fire off a nack if no ack is seen in time.
	// 对方节点未响应 并且需要进行nack响应
	if ind.Nack {
		go func() {
			select {
			case <-cancelCh: // 正常响应ack了直接退出
				return
			case <-time.After(m.config.ProbeTimeout): // 向来源节点响应nack信息
				nack := nackResp{ind.SeqNo}
				a := Address{
					Addr: indAddr,
					Name: ind.SourceNode,
				}
				if err := m.encodeAndSendMsg(a, nackRespMsg, &nack); err != nil {
					m.logger.Printf("[ERR] memberlist: Failed to send nack: %s %s", err, LogStringAddress(indAddr))
				}
			}
		}()
	}
}

// ping消息的ack响应
func (m *Memberlist) handleAck(buf []byte, from net.Addr, timestamp time.Time) {
	var ack ackResp
	if err := decode(buf, &ack); err != nil {
		m.logger.Printf("[ERR] memberlist: Failed to decode ack response: %s %s", err, LogAddress(from))
		return
	}
	m.invokeAckHandler(ack, timestamp)
}

// ping的nack响应
func (m *Memberlist) handleNack(buf []byte, from net.Addr) {
	var nack nackResp
	if err := decode(buf, &nack); err != nil {
		m.logger.Printf("[ERR] memberlist: Failed to decode nack response: %s %s", err, LogAddress(from))
		return
	}
	m.invokeNackHandler(nack)
}

// 可疑消息
func (m *Memberlist) handleSuspect(buf []byte, from net.Addr) {
	var sus suspect
	if err := decode(buf, &sus); err != nil {
		m.logger.Printf("[ERR] memberlist: Failed to decode suspect message: %s %s", err, LogAddress(from))
		return
	}
	m.suspectNode(&sus)
}

// ensureCanConnect return the IP from a RemoteAddress
// return error if this client must not connect
func (m *Memberlist) ensureCanConnect(from net.Addr) error {
	if !m.config.IPMustBeChecked() {
		return nil
	}
	source := from.String()
	if source == "pipe" {
		return nil
	}
	host, _, err := net.SplitHostPort(source)
	if err != nil {
		return err
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("Cannot parse IP from %s", host)
	}
	return m.config.IPAllowed(ip)
}

// alive消息处理
func (m *Memberlist) handleAlive(buf []byte, from net.Addr) {
	if err := m.ensureCanConnect(from); err != nil {
		m.logger.Printf("[DEBUG] memberlist: Blocked alive message: %s %s", err, LogAddress(from))
		return
	}
	var live alive
	if err := decode(buf, &live); err != nil {
		m.logger.Printf("[ERR] memberlist: Failed to decode alive message: %s %s", err, LogAddress(from))
		return
	}
	if m.config.IPMustBeChecked() {
		innerIP := net.IP(live.Addr)
		if innerIP != nil {
			if err := m.config.IPAllowed(innerIP); err != nil {
				m.logger.Printf("[DEBUG] memberlist: Blocked alive.Addr=%s message from: %s %s", innerIP.String(), err, LogAddress(from))
				return
			}
		}
	}

	// For proto versions < 2, there is no port provided. Mask old
	// behavior by using the configured port
	// 对于原始版本< 2，没有提供端口。使用配置的端口屏蔽旧的行为
	if m.ProtocolVersion() < 2 || live.Port == 0 {
		live.Port = uint16(m.config.BindPort)
	}

	m.aliveNode(&live, nil, false)
}

func (m *Memberlist) handleDead(buf []byte, from net.Addr) {
	var d dead
	if err := decode(buf, &d); err != nil {
		m.logger.Printf("[ERR] memberlist: Failed to decode dead message: %s %s", err, LogAddress(from))
		return
	}
	m.deadNode(&d)
}

// handleUser is used to notify channels of incoming user data
// serf层消息
func (m *Memberlist) handleUser(buf []byte, from net.Addr) {
	d := m.config.Delegate
	if d != nil {
		d.NotifyMsg(buf)
	}
}

// handleCompressed is used to unpack a compressed message
// handleCompressed用于解压缩消息
func (m *Memberlist) handleCompressed(buf []byte, from net.Addr, timestamp time.Time) {
	// Try to decode the payload
	payload, err := decompressPayload(buf)
	if err != nil {
		m.logger.Printf("[ERR] memberlist: Failed to decompress payload: %v %s", err, LogAddress(from))
		return
	}

	// Recursively handle the payload
	m.handleCommand(payload, from, timestamp)
}

// encodeAndSendMsg is used to combine the encoding and sending steps
// encodeAndSendMsg用于组合编码和发送步骤
func (m *Memberlist) encodeAndSendMsg(a Address, msgType messageType, msg interface{}) error {
	out, err := encode(msgType, msg)
	if err != nil {
		return err
	}
	if err := m.sendMsg(a, out.Bytes()); err != nil {
		return err
	}
	return nil
}

// sendMsg is used to send a message via packet to another host. It will
// opportunistically create a compoundMsg and piggy back other broadcasts.
// sendMsg用于通过数据包向另一个主机发送消息。它将有机会创建一个composdmsg并将其他广播连接起来。
func (m *Memberlist) sendMsg(a Address, msg []byte) error {
	// Check if we can piggy back any messages
	// 计算消息剩余长度  获取其他报文
	bytesAvail := m.config.UDPBufferSize - len(msg) - compoundHeaderOverhead
	if m.config.EncryptionEnabled() && m.config.GossipVerifyOutgoing {
		bytesAvail -= encryptOverhead(m.encryptionVersion())
	}
	extra := m.getBroadcasts(compoundOverhead, bytesAvail)

	// Fast path if nothing to piggypack
	// 没有获取到 就只发一条消息
	if len(extra) == 0 {
		return m.rawSendMsgPacket(a, nil, msg)
	}

	// Join all the messages
	// 合并新老消息
	msgs := make([][]byte, 0, 1+len(extra))
	msgs = append(msgs, msg)
	msgs = append(msgs, extra...)

	// Create a compound message
	// 打包成compound消息
	compound := makeCompoundMessage(msgs)

	// Send the message
	return m.rawSendMsgPacket(a, nil, compound.Bytes())
}

// rawSendMsgPacket is used to send message via packet to another host without
// modification, other than compression or encryption if enabled.
// rawSendMsgPacket用于通过数据包发送消息到另一个主机，不需要修改，如果启用了压缩或加密。
// udp
func (m *Memberlist) rawSendMsgPacket(a Address, node *Node, msg []byte) error {
	if a.Name == "" && m.config.RequireNodeNames {
		return errNodeNamesAreRequired
	}

	// Check if we have compression enabled
	// 开启压缩
	if m.config.EnableCompression {
		buf, err := compressPayload(msg)
		if err != nil {
			m.logger.Printf("[WARN] memberlist: Failed to compress payload: %v", err)
		} else {
			// Only use compression if it reduced the size
			// 只有当压缩缩小了尺寸时才使用压缩
			if buf.Len() < len(msg) {
				msg = buf.Bytes()
			}
		}
	}

	// Try to look up the destination node. Note this will only work if the
	// bare ip address is used as the node name, which is not guaranteed.
	// 尝试查找目标节点。说明仅当使用裸ip地址作为节点名称时有效，不作保证。
	if node == nil {
		toAddr, _, err := net.SplitHostPort(a.Addr)
		if err != nil {
			m.logger.Printf("[ERR] memberlist: Failed to parse address %q: %v", a.Addr, err)
			return err
		}
		m.nodeLock.RLock()
		nodeState, ok := m.nodeMap[toAddr]
		m.nodeLock.RUnlock()
		if ok {
			node = &nodeState.Node
		}
	}

	// Add a CRC to the end of the payload if the recipient understands
	// ProtocolVersion >= 5
	// 如果接收方理解协议版本>= 5，则在有效载荷的末尾添加CRC
	if node != nil && node.PMax >= 5 {
		crc := crc32.ChecksumIEEE(msg)
		header := make([]byte, 5, 5+len(msg))       // 初始5个长度
		header[0] = byte(hasCrcMsg)                 // 0写入类型
		binary.BigEndian.PutUint32(header[1:], crc) // 剩下四个写校验码
		msg = append(header, msg...)
	}

	// Check if we have encryption enabled
	// 检查我们是否启用了加密  第一个字节没有type标识
	if m.config.EncryptionEnabled() && m.config.GossipVerifyOutgoing {
		// Encrypt the payload
		var buf bytes.Buffer
		primaryKey := m.config.Keyring.GetPrimaryKey()
		err := encryptPayload(m.encryptionVersion(), primaryKey, msg, nil, &buf)
		if err != nil {
			m.logger.Printf("[ERR] memberlist: Encryption of message failed: %v", err)
			return err
		}
		msg = buf.Bytes()
	}

	metrics.IncrCounter([]string{"memberlist", "udp", "sent"}, float32(len(msg)))
	_, err := m.transport.WriteToAddress(msg, a)
	return err
}

// rawSendMsgStream is used to stream a message to another host without
// modification, other than applying compression and encryption if enabled.
// 发送数据 对加密 压缩的消息类型单独处理
// rawSendMsgStream用于在不修改的情况下将消息流发送到另一个主机，如果启用，则不应用压缩和加密。
// tcp
func (m *Memberlist) rawSendMsgStream(conn net.Conn, sendBuf []byte) error {
	// Check if compression is enabled
	if m.config.EnableCompression { // 压缩
		compBuf, err := compressPayload(sendBuf)
		if err != nil {
			m.logger.Printf("[ERROR] memberlist: Failed to compress payload: %v", err)
		} else {
			sendBuf = compBuf.Bytes()
		}
	}

	// Check if encryption is enabled
	// 加密
	if m.config.EncryptionEnabled() && m.config.GossipVerifyOutgoing {
		crypt, err := m.encryptLocalState(sendBuf)
		if err != nil {
			m.logger.Printf("[ERROR] memberlist: Failed to encrypt local state: %v", err)
			return err
		}
		sendBuf = crypt
	}

	// Write out the entire send buffer
	metrics.IncrCounter([]string{"memberlist", "tcp", "sent"}, float32(len(sendBuf)))

	// 写入请求数据
	if n, err := conn.Write(sendBuf); err != nil {
		return err
	} else if n != len(sendBuf) {
		return fmt.Errorf("only %d of %d bytes written", n, len(sendBuf))
	}

	return nil
}

// sendUserMsg is used to stream a user message to another host.
func (m *Memberlist) sendUserMsg(a Address, sendBuf []byte) error {
	if a.Name == "" && m.config.RequireNodeNames {
		return errNodeNamesAreRequired
	}

	conn, err := m.transport.DialAddressTimeout(a, m.config.TCPTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()

	bufConn := bytes.NewBuffer(nil)
	if err := bufConn.WriteByte(byte(userMsg)); err != nil {
		return err
	}

	header := userMsgHeader{UserMsgLen: len(sendBuf)}
	hd := codec.MsgpackHandle{}
	enc := codec.NewEncoder(bufConn, &hd)
	if err := enc.Encode(&header); err != nil {
		return err
	}
	if _, err := bufConn.Write(sendBuf); err != nil {
		return err
	}
	return m.rawSendMsgStream(conn, bufConn.Bytes())
}

// sendAndReceiveState is used to initiate a push/pull over a stream with a
// remote host.
// sendAndReceiveState用于在远程主机上发起推/拉流。
func (m *Memberlist) sendAndReceiveState(a Address, join bool) ([]pushNodeState, []byte, error) {
	// 是否需要带上nodeName
	if a.Name == "" && m.config.RequireNodeNames {
		return nil, nil, errNodeNamesAreRequired
	}

	// Attempt to connect
	// 建立链接
	conn, err := m.transport.DialAddressTimeout(a, m.config.TCPTimeout)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	m.logger.Printf("[DEBUG] memberlist: Initiating push/pull sync with: %s %s", a.Name, conn.RemoteAddr())
	metrics.IncrCounter([]string{"memberlist", "tcp", "connect"}, 1)

	// Send our state
	// 发送当前节点下的nodes
	if err := m.sendLocalState(conn, join); err != nil {
		return nil, nil, err
	}

	conn.SetDeadline(time.Now().Add(m.config.TCPTimeout))
	msgType, bufConn, dec, err := m.readStream(conn)
	if err != nil {
		return nil, nil, err
	}

	if msgType == errMsg {
		var resp errResp
		if err := dec.Decode(&resp); err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("remote error: %v", resp.Error)
	}

	// Quit if not push/pull
	if msgType != pushPullMsg {
		err := fmt.Errorf("received invalid msgType (%d), expected pushPullMsg (%d) %s", msgType, pushPullMsg, LogConn(conn))
		return nil, nil, err
	}

	// Read remote state
	_, remoteNodes, userState, err := m.readRemoteState(bufConn, dec)
	return remoteNodes, userState, err
}

// sendLocalState is invoked to send our local state over a stream connection.
// 调用sendLocalState通过流连接发送我们的本地状态。
func (m *Memberlist) sendLocalState(conn net.Conn, join bool) error {
	// Setup a deadline
	// 设置一个期限
	conn.SetDeadline(time.Now().Add(m.config.TCPTimeout))

	// Prepare the local node state
	// 复制本节点所有的节点信息
	m.nodeLock.RLock()
	localNodes := make([]pushNodeState, len(m.nodes))
	for idx, n := range m.nodes {
		localNodes[idx].Name = n.Name
		localNodes[idx].Addr = n.Addr
		localNodes[idx].Port = n.Port
		localNodes[idx].Incarnation = n.Incarnation
		localNodes[idx].State = n.State
		localNodes[idx].Meta = n.Meta
		localNodes[idx].Vsn = []uint8{
			n.PMin, n.PMax, n.PCur,
			n.DMin, n.DMax, n.DCur,
		}
	}
	m.nodeLock.RUnlock()

	// Get the delegate state
	// serf层的节点信息
	var userData []byte
	if m.config.Delegate != nil {
		userData = m.config.Delegate.LocalState(join)
	}

	// Create a bytes buffer writer
	bufConn := bytes.NewBuffer(nil)

	// Send our node state
	header := pushPullHeader{Nodes: len(localNodes), UserStateLen: len(userData), Join: join}
	hd := codec.MsgpackHandle{}
	enc := codec.NewEncoder(bufConn, &hd)

	// Begin state push
	// 写入消息类型 pushPullMsg
	if _, err := bufConn.Write([]byte{byte(pushPullMsg)}); err != nil {
		return err
	}

	// 写入 header
	if err := enc.Encode(&header); err != nil {
		return err
	}

	// 依次写入node信息
	for i := 0; i < header.Nodes; i++ {
		if err := enc.Encode(&localNodes[i]); err != nil {
			return err
		}
	}

	// Write the user state as well
	// 写入serf层的信息
	if userData != nil {
		if _, err := bufConn.Write(userData); err != nil {
			return err
		}
	}

	// Get the send buffer
	// 获取发送缓冲区
	return m.rawSendMsgStream(conn, bufConn.Bytes())
}

// encryptLocalState is used to help encrypt local state before sending
// 对数据进行加密
func (m *Memberlist) encryptLocalState(sendBuf []byte) ([]byte, error) {
	var buf bytes.Buffer

	// Write the encryptMsg byte
	buf.WriteByte(byte(encryptMsg)) // 第一个字节 类型

	// Write the size of the message
	sizeBuf := make([]byte, 4)
	encVsn := m.encryptionVersion()
	encLen := encryptedLength(encVsn, len(sendBuf))
	binary.BigEndian.PutUint32(sizeBuf, uint32(encLen))
	buf.Write(sizeBuf)

	// Write the encrypted cipher text to the buffer
	key := m.config.Keyring.GetPrimaryKey()
	err := encryptPayload(encVsn, key, sendBuf, buf.Bytes()[:5], &buf)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decryptRemoteState is used to help decrypt the remote state
// 对加密的conn进行数据读取以及解密 返回已解密的数据
func (m *Memberlist) decryptRemoteState(bufConn io.Reader) ([]byte, error) {
	// Read in enough to determine message length
	cipherText := bytes.NewBuffer(nil)         // 用来接收conn数据
	cipherText.WriteByte(byte(encryptMsg))     // 写入第一个字节 表示类型
	_, err := io.CopyN(cipherText, bufConn, 4) // 从conn读入4个字节 表示长度
	if err != nil {
		return nil, err
	}

	// Ensure we aren't asked to download too much. This is to guard against
	// an attack vector where a huge amount of state is sent
	// [1:5]个字节代表整个请求数据的长度
	moreBytes := binary.BigEndian.Uint32(cipherText.Bytes()[1:5])
	if moreBytes > maxPushStateBytes {
		return nil, fmt.Errorf("Remote node state is larger than limit (%d)", moreBytes)
	}

	// Read in the rest of the payload
	// 剩下的数据读入cipherText
	_, err = io.CopyN(cipherText, bufConn, int64(moreBytes))
	if err != nil {
		return nil, err
	}

	// Decrypt the cipherText
	dataBytes := cipherText.Bytes()[:5]
	cipherBytes := cipherText.Bytes()[5:]

	// Decrypt the payload
	// 解密
	keys := m.config.Keyring.GetKeys()
	return decryptPayload(keys, cipherBytes, dataBytes)
}

// readStream is used to read from a stream connection, decrypting and
// decompressing the stream if necessary.
// 解析tcp conn请求数据  [类型][value]
func (m *Memberlist) readStream(conn net.Conn) (messageType, io.Reader, *codec.Decoder, error) {
	// Created a buffered reader
	var bufConn io.Reader = bufio.NewReader(conn)

	// Read the message type
	// 从bufConn中读取一个字节 校验类型
	buf := [1]byte{0}
	if _, err := bufConn.Read(buf[:]); err != nil {
		return 0, nil, nil, err
	}
	msgType := messageType(buf[0])

	// Check if the message is encrypted
	if msgType == encryptMsg {
		if !m.config.EncryptionEnabled() {
			return 0, nil, nil,
				fmt.Errorf("Remote state is encrypted and encryption is not configured")
		}

		// 解密数据
		plain, err := m.decryptRemoteState(bufConn)
		if err != nil {
			return 0, nil, nil, err
		}

		// Reset message type and bufConn
		msgType = messageType(plain[0]) // 变更消息类型
		bufConn = bytes.NewReader(plain[1:])
	} else if m.config.EncryptionEnabled() && m.config.GossipVerifyIncoming {
		return 0, nil, nil,
			fmt.Errorf("Encryption is configured but remote state is not encrypted")
	}

	// Get the msgPack decoders
	hd := codec.MsgpackHandle{}
	dec := codec.NewDecoder(bufConn, &hd)

	// Check if we have a compressed message
	// 压缩类型
	if msgType == compressMsg {
		var c compress
		if err := dec.Decode(&c); err != nil {
			return 0, nil, nil, err
		}
		decomp, err := decompressBuffer(&c)
		if err != nil {
			return 0, nil, nil, err
		}

		// Reset the message type
		msgType = messageType(decomp[0]) // 变更消息类型

		// Create a new bufConn
		bufConn = bytes.NewReader(decomp[1:])

		// Create a new decoder
		dec = codec.NewDecoder(bufConn, &hd)
	}

	return msgType, bufConn, dec, nil
}

// readRemoteState is used to read the remote state from a connection
// readRemoteState用于从连接中读取远程状态
func (m *Memberlist) readRemoteState(bufConn io.Reader, dec *codec.Decoder) (bool, []pushNodeState, []byte, error) {
	// Read the push/pull header
	// 读取header
	var header pushPullHeader
	if err := dec.Decode(&header); err != nil {
		return false, nil, nil, err
	}

	// Allocate space for the transfer
	// nodes信息
	remoteNodes := make([]pushNodeState, header.Nodes)

	// Try to decode all the states
	for i := 0; i < header.Nodes; i++ {
		if err := dec.Decode(&remoteNodes[i]); err != nil {
			return false, nil, nil, err
		}
	}

	// Read the remote user state into a buffer
	// 读取serf层的节点信息
	var userBuf []byte
	if header.UserStateLen > 0 {
		userBuf = make([]byte, header.UserStateLen)
		bytes, err := io.ReadAtLeast(bufConn, userBuf, header.UserStateLen)
		if err == nil && bytes != header.UserStateLen {
			err = fmt.Errorf(
				"Failed to read full user state (%d / %d)",
				bytes, header.UserStateLen)
		}
		if err != nil {
			return false, nil, nil, err
		}
	}

	// For proto versions < 2, there is no port provided. Mask old
	// behavior by using the configured port
	// 对于原始版本< 2，没有提供端口。使用配置的端口屏蔽旧的行为
	for idx := range remoteNodes {
		if m.ProtocolVersion() < 2 || remoteNodes[idx].Port == 0 {
			remoteNodes[idx].Port = uint16(m.config.BindPort)
		}
	}

	return header.Join, remoteNodes, userBuf, nil
}

// mergeRemoteState is used to merge the remote state with our local state
// mergeRemoteState用于将远程状态与本地状态合并
func (m *Memberlist) mergeRemoteState(join bool, remoteNodes []pushNodeState, userBuf []byte) error {
	// 版本校验
	if err := m.verifyProtocol(remoteNodes); err != nil {
		return err
	}

	// Invoke the merge delegate if any
	// 是否通知到serf层 可以控制是否取消合并
	if join && m.config.Merge != nil {
		nodes := make([]*Node, len(remoteNodes))
		for idx, n := range remoteNodes {
			nodes[idx] = &Node{
				Name:  n.Name,
				Addr:  n.Addr,
				Port:  n.Port,
				Meta:  n.Meta,
				State: n.State,
				PMin:  n.Vsn[0],
				PMax:  n.Vsn[1],
				PCur:  n.Vsn[2],
				DMin:  n.Vsn[3],
				DMax:  n.Vsn[4],
				DCur:  n.Vsn[5],
			}
		}
		if err := m.config.Merge.NotifyMerge(nodes); err != nil {
			return err
		}
	}

	// Merge the membership state
	// membership层合并nodes
	m.mergeState(remoteNodes)

	// Invoke the delegate for user state
	// serf层合并nodes
	if userBuf != nil && m.config.Delegate != nil {
		m.config.Delegate.MergeRemoteState(userBuf, join)
	}
	return nil
}

// readUserMsg is used to decode a userMsg from a stream.
func (m *Memberlist) readUserMsg(bufConn io.Reader, dec *codec.Decoder) error {
	// Read the user message header
	var header userMsgHeader
	if err := dec.Decode(&header); err != nil {
		return err
	}

	// Read the user message into a buffer
	var userBuf []byte
	if header.UserMsgLen > 0 {
		userBuf = make([]byte, header.UserMsgLen)
		bytes, err := io.ReadAtLeast(bufConn, userBuf, header.UserMsgLen)
		if err == nil && bytes != header.UserMsgLen {
			err = fmt.Errorf(
				"Failed to read full user message (%d / %d)",
				bytes, header.UserMsgLen)
		}
		if err != nil {
			return err
		}

		d := m.config.Delegate
		if d != nil {
			d.NotifyMsg(userBuf)
		}
	}

	return nil
}

// sendPingAndWaitForAck makes a stream connection to the given address, sends
// a ping, and waits for an ack. All of this is done as a series of blocking
// operations, given the deadline. The bool return parameter is true if we
// we able to round trip a ping to the other node.
// sendPingAndWaitForAck建立到给定地址的tcp连接，发送ping，并等待ack。
// 所有这些都是通过一系列的阻塞操作完成的，给定了最后期限。
// 如果我们能够往返ping到另一个节点，bool返回参数为真。
func (m *Memberlist) sendPingAndWaitForAck(a Address, ping ping, deadline time.Time) (bool, error) {
	if a.Name == "" && m.config.RequireNodeNames {
		return false, errNodeNamesAreRequired
	}

	// timeout是相对的超时时间
	conn, err := m.transport.DialAddressTimeout(a, deadline.Sub(time.Now()))
	if err != nil {
		// If the node is actually dead we expect this to fail, so we
		// shouldn't spam the logs with it. After this point, errors
		// with the connection are real, unexpected errors and should
		// get propagated up.
		return false, nil
	}
	defer conn.Close()
	conn.SetDeadline(deadline) // 绝对的超时时间

	out, err := encode(pingMsg, &ping)
	if err != nil {
		return false, err
	}

	if err = m.rawSendMsgStream(conn, out.Bytes()); err != nil {
		return false, err
	}

	msgType, _, dec, err := m.readStream(conn)
	if err != nil {
		return false, err
	}

	if msgType != ackRespMsg {
		return false, fmt.Errorf("Unexpected msgType (%d) from ping %s", msgType, LogConn(conn))
	}

	var ack ackResp
	if err = dec.Decode(&ack); err != nil {
		return false, err
	}

	if ack.SeqNo != ping.SeqNo {
		return false, fmt.Errorf("Sequence number from ack (%d) doesn't match ping (%d)", ack.SeqNo, ping.SeqNo)
	}

	return true, nil
}
