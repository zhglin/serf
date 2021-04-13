package memberlist

/*
The broadcast mechanism works by maintaining a sorted list of messages to be
sent out. When a message is to be broadcast, the retransmit count
is set to zero and appended to the queue. The retransmit count serves
as the "priority", ensuring that newer messages get sent first. Once
a message hits the retransmit limit, it is removed from the queue.

Additionally, older entries can be invalidated by new messages that
are contradictory. For example, if we send "{suspect M1 inc: 1},
then a following {alive M1 inc: 2} will invalidate that message
*/

// 需要去重的broadcast
type memberlistBroadcast struct {
	node   string
	msg    []byte
	notify chan struct{}
}

func (b *memberlistBroadcast) Invalidates(other Broadcast) bool {
	// Check if that broadcast is a memberlist type
	mb, ok := other.(*memberlistBroadcast)
	if !ok {
		return false
	}

	// Invalidates any message about the same node
	return b.node == mb.node
}

// memberlist.NamedBroadcast optional interface
func (b *memberlistBroadcast) Name() string {
	return b.node
}

func (b *memberlistBroadcast) Message() []byte {
	return b.msg
}

func (b *memberlistBroadcast) Finished() {
	select {
	case b.notify <- struct{}{}:
	default:
	}
}

// encodeAndBroadcast encodes a message and enqueues it for broadcast. Fails
// silently if there is an encoding error.
// encodeAndBroadcast对消息进行编码并将其排队以进行广播。如果存在编码错误，则静默失败。
func (m *Memberlist) encodeAndBroadcast(node string, msgType messageType, msg interface{}) {
	m.encodeBroadcastNotify(node, msgType, msg, nil)
}

// encodeBroadcastNotify encodes a message and enqueues it for broadcast
// and notifies the given channel when transmission is finished. Fails
// silently if there is an encoding error.
// encodeBroadcastNotify对消息进行编码，并将其排队广播，并在传输完成时通知给定的通道。如果存在编码错误，则静默失败。
func (m *Memberlist) encodeBroadcastNotify(node string, msgType messageType, msg interface{}, notify chan struct{}) {
	buf, err := encode(msgType, msg)
	if err != nil {
		m.logger.Printf("[ERR] memberlist: Failed to encode message for broadcast: %s", err)
	} else {
		m.queueBroadcast(node, buf.Bytes(), notify)
	}
}

// queueBroadcast is used to start dissemination of a message. It will be
// sent up to a configured number of times. The message could potentially
// be invalidated by a future message about the same node
// queueBroadcast用于开始发布消息。
// 它将被发送到一个配置的次数。该消息可能会被关于同一节点的未来消息作废
func (m *Memberlist) queueBroadcast(node string, msg []byte, notify chan struct{}) {
	b := &memberlistBroadcast{node, msg, notify}
	m.broadcasts.QueueBroadcast(b)
}

// getBroadcasts is used to return a slice of broadcasts to send up to
// a maximum byte size, while imposing a per-broadcast overhead. This is used
// to fill a UDP packet with piggybacked data
// getBroadcasts用于返回要发送的最大字节大小的广播片，同时强制每个广播的开销。这是用来填充一个UDP包与附带的数据
func (m *Memberlist) getBroadcasts(overhead, limit int) [][]byte {
	// Get memberlist messages first
	// 获取成员列表消息
	toSend := m.broadcasts.GetBroadcasts(overhead, limit)

	// Check if the user has anything to broadcast
	// 检查用户是否有任何东西要广播
	d := m.config.Delegate
	if d != nil {
		// Determine the bytes used already
		// 确定已经使用的字节数
		bytesUsed := 0
		for _, msg := range toSend {
			bytesUsed += len(msg) + overhead
		}

		// Check space remaining for user messages
		// 检查用户消息的剩余空间
		// 需要额外消耗2个字节的长度+1个字节的类型
		avail := limit - bytesUsed
		if avail > overhead+userMsgOverhead {
			userMsgs := d.GetBroadcasts(overhead+userMsgOverhead, avail)

			// Frame each user message
			// 将每个用户信息帧起来
			for _, msg := range userMsgs {
				buf := make([]byte, 1, len(msg)+1)
				buf[0] = byte(userMsg) // 增加userMsg标记
				buf = append(buf, msg...)
				toSend = append(toSend, buf)
			}
		}
	}
	return toSend
}
