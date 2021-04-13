package memberlist

import (
	"github.com/hashicorp/serf/extpkg/btree"
	"math"
	"sync"
	//"github.com/google/btree"
)

// TransmitLimitedQueue is used to queue messages to broadcast to
// the cluster (via gossip) but limits the number of transmits per
// message. It also prioritizes messages with lower transmit counts
// (hence newer messages).
type TransmitLimitedQueue struct {
	// NumNodes returns the number of nodes in the cluster. This is
	// used to determine the retransmit count, which is calculated
	// based on the log of this.
	// 获取当前nodes中node的总数
	NumNodes func() int

	// RetransmitMult is the multiplier used to determine the maximum
	// number of retransmissions attempted.
	// 重传是用来确定试图重传的最大次数的乘法器。
	RetransmitMult int

	mu    sync.Mutex
	tq    *btree.BTree                 // stores *limitedBroadcast as btree.Item
	tm    map[string]*limitedBroadcast // NameBroadCast的去重
	idGen int64                        // 队列中消息的id
}

type limitedBroadcast struct {
	transmits int   // btree-key[0]: Number of transmissions attempted.
	msgLen    int64 // btree-key[1]: copied from len(b.Message())
	id        int64 // btree-key[2]: unique incrementing id stamped at submission time
	b         Broadcast

	name string // set if Broadcast is a NamedBroadcast
}

// Less tests whether the current item is less than the given argument.
//
// This must provide a strict weak ordering.
// If !a.Less(b) && !b.Less(a), we treat this to mean a == b (i.e. we can only
// hold one of either a or b in the tree).
//
// default ordering is
// - [transmits=0, ..., transmits=inf]
// - [transmits=0:len=999, ..., transmits=0:len=2, ...]
// - [transmits=0:len=999,id=999, ..., transmits=0:len=999:id=1, ...]
// 按照transmits升序 msgLen降序 id升序进行排序
func (b *limitedBroadcast) Less(than btree.Item) bool {
	o := than.(*limitedBroadcast)
	if b.transmits < o.transmits {
		return true
	} else if b.transmits > o.transmits {
		return false
	}
	if b.msgLen > o.msgLen {
		return true
	} else if b.msgLen < o.msgLen {
		return false
	}
	return b.id > o.id
}

// for testing; emits in transmit order if reverse=false
func (q *TransmitLimitedQueue) orderedView(reverse bool) []*limitedBroadcast {
	q.mu.Lock()
	defer q.mu.Unlock()

	out := make([]*limitedBroadcast, 0, q.lenLocked())
	q.walkReadOnlyLocked(reverse, func(cur *limitedBroadcast) bool {
		out = append(out, cur)
		return true
	})

	return out
}

// walkReadOnlyLocked calls f for each item in the queue traversing it in
// natural order (by Less) when reverse=false and the opposite when true. You
// must hold the mutex.
//
// This method panics if you attempt to mutate the item during traversal.  The
// underlying btree should also not be mutated during traversal.
func (q *TransmitLimitedQueue) walkReadOnlyLocked(reverse bool, f func(*limitedBroadcast) bool) {
	if q.lenLocked() == 0 {
		return
	}

	iter := func(item btree.Item) bool {
		cur := item.(*limitedBroadcast)

		prevTransmits := cur.transmits
		prevMsgLen := cur.msgLen
		prevID := cur.id

		keepGoing := f(cur)

		if prevTransmits != cur.transmits || prevMsgLen != cur.msgLen || prevID != cur.id {
			panic("edited queue while walking read only")
		}

		return keepGoing
	}

	if reverse {
		q.tq.Descend(iter) // end with transmit 0
	} else {
		q.tq.Ascend(iter) // start with transmit 0
	}
}

// Broadcast is something that can be broadcasted via gossip to
// the memberlist cluster.
// memberlist层的广播的消息需要实现的接口
type Broadcast interface {
	// Invalidates checks if enqueuing the current broadcast
	// invalidates a previous broadcast
	// 检查输入当前广播是否会使前一个广播失效 防止重复 最新的生效
	Invalidates(b Broadcast) bool

	// Returns a byte form of the message
	// 返回broadcast的消息内容
	Message() []byte

	// Finished is invoked when the message will no longer
	// be broadcast, either due to invalidation or to the
	// transmit limit being reached
	// (因重复被丢弃)将消息不再被广播时调用Finished，无论是由于无效还是达到传输极限
	Finished()
}

// NamedBroadcast is an optional extension of the Broadcast interface that
// gives each message a unique string name, and that is used to optimize
//
// You shoud ensure that Invalidates() checks the same uniqueness as the
// example below:
//
// func (b *foo) Invalidates(other Broadcast) bool {
// 	nb, ok := other.(NamedBroadcast)
// 	if !ok {
// 		return false
// 	}
// 	return b.Name() == nb.Name()
// }
//
// Invalidates() isn't currently used for NamedBroadcasts, but that may change
// in the future.
// 根据name方法校验是否重复的broadCast
type NamedBroadcast interface {
	Broadcast
	// The unique identity of this broadcast message.
	Name() string
}

// UniqueBroadcast is an optional interface that indicates that each message is
// intrinsically unique and there is no need to scan the broadcast queue for
// duplicates.
//
// You should ensure that Invalidates() always returns false if implementing
// this interface. Invalidates() isn't currently used for UniqueBroadcasts, but
// that may change in the future.
// 每个BroadCast都是唯一的 不需要进行过滤去重
type UniqueBroadcast interface {
	Broadcast
	// UniqueBroadcast is just a marker method for this interface.
	// UniqueBroadcast只是这个接口的一个标记方法。
	UniqueBroadcast()
}

// QueueBroadcast is used to enqueue a broadcast
// 添加Broadcast到queue
func (q *TransmitLimitedQueue) QueueBroadcast(b Broadcast) {
	q.queueBroadcast(b, 0)
}

// lazyInit initializes internal data structures the first time they are
// needed.  You must already hold the mutex.
// 延迟初始化
func (q *TransmitLimitedQueue) lazyInit() {
	if q.tq == nil {
		q.tq = btree.New(32)
	}
	if q.tm == nil {
		q.tm = make(map[string]*limitedBroadcast)
	}
}

// queueBroadcast is like QueueBroadcast but you can use a nonzero value for
// the initial transmit tier assigned to the message. This is meant to be used
// for unit testing.
// queueBroadcast与queueBroadcast类似，但您可以为分配给消息的初始传输层使用非零值。这是用于单元测试的。
// 非零值表示已经重传的次数
// 添加Broadcast
func (q *TransmitLimitedQueue) queueBroadcast(b Broadcast, initialTransmits int) {
	q.mu.Lock()
	defer q.mu.Unlock()

	// 初始化tq tm
	q.lazyInit()

	// 生成broadCast的id
	if q.idGen == math.MaxInt64 {
		// it's super duper unlikely to wrap around within the retransmit limit
		q.idGen = 1
	} else {
		q.idGen++
	}
	id := q.idGen

	// 包装下
	lb := &limitedBroadcast{
		transmits: initialTransmits,
		msgLen:    int64(len(b.Message())), // broadCast中的消息长度
		id:        id,                      // id序号
		b:         b,                       // 原始的BroadCast
	}
	unique := false // 是否是唯一类型的UniqueBroadCast
	if nb, ok := b.(NamedBroadcast); ok {
		lb.name = nb.Name()
	} else if _, ok := b.(UniqueBroadcast); ok {
		unique = true
	}

	// Check if this message invalidates another.
	// NameBroadCast的去重
	if lb.name != "" {
		if old, ok := q.tm[lb.name]; ok {
			old.b.Finished()
			q.deleteItem(old)
		}
	} else if !unique { // 唯一类型的不需要去重
		// Slow path, hopefully nothing hot hits this.
		var remove []*limitedBroadcast
		q.tq.Ascend(func(item btree.Item) bool { // 从btree里过滤
			cur := item.(*limitedBroadcast)

			// Special Broadcasts can only invalidate each other.
			switch cur.b.(type) {
			case NamedBroadcast:
				// noop
			case UniqueBroadcast:
				// noop
			default: // 其他BroadCast类型
				if b.Invalidates(cur.b) {
					cur.b.Finished()
					remove = append(remove, cur)
				}
			}
			return true
		})

		// 删除重复的broadCast
		for _, cur := range remove {
			q.deleteItem(cur)
		}
	}

	// Append to the relevant queue.
	// 添加
	q.addItem(lb)
}

// deleteItem removes the given item from the overall datastructure. You
// must already hold the mutex.
// 删除指定的limitedBroadcast
func (q *TransmitLimitedQueue) deleteItem(cur *limitedBroadcast) {
	_ = q.tq.Delete(cur)
	if cur.name != "" {
		delete(q.tm, cur.name)
	}

	// 空闲时重置id生成
	if q.tq.Len() == 0 {
		// At idle there's no reason to let the id generator keep going
		// indefinitely.
		q.idGen = 0
	}
}

// addItem adds the given item into the overall datastructure. You must already
// hold the mutex.
// 添加limitedBroadCast
func (q *TransmitLimitedQueue) addItem(cur *limitedBroadcast) {
	_ = q.tq.ReplaceOrInsert(cur)
	if cur.name != "" {
		q.tm[cur.name] = cur
	}
}

// getTransmitRange returns a pair of min/max values for transmit values
// represented by the current queue contents. Both values represent actual
// transmit values on the interval [0, len). You must already hold the mutex.
// 获取所有limitedBroadcast中的最小以及最大的transmits
func (q *TransmitLimitedQueue) getTransmitRange() (minTransmit, maxTransmit int) {
	if q.lenLocked() == 0 {
		return 0, 0
	}
	minItem, maxItem := q.tq.Min(), q.tq.Max()
	if minItem == nil || maxItem == nil {
		return 0, 0
	}

	min := minItem.(*limitedBroadcast).transmits
	max := maxItem.(*limitedBroadcast).transmits

	return min, max
}

// GetBroadcasts is used to get a number of broadcasts, up to a byte limit
// and applying a per-message overhead as provided.
// 获取一批broadCast组合在一起，打包成复合报文进行发送
// overhead是每个报文组合在一起需要额外消耗的长度(2个字节的报文长度)
// limit是一个udp报文的所能承载的最大长度(udp的mtu)
func (q *TransmitLimitedQueue) GetBroadcasts(overhead, limit int) [][]byte {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Fast path the default case
	// 队列中没有数据
	if q.lenLocked() == 0 {
		return nil
	}

	// 报文的重试次数上限
	transmitLimit := retransmitLimit(q.RetransmitMult, q.NumNodes())

	var (
		bytesUsed int
		toSend    [][]byte
		reinsert  []*limitedBroadcast
	)

	// Visit fresher items first, but only look at stuff that will fit.
	// We'll go tier by tier, grabbing the largest items first.
	// 每个transmits取一个，优先最低transmits，最大body的，满足free条件的情况下再取transmits+1的
	minTr, maxTr := q.getTransmitRange()
	for transmits := minTr; transmits <= maxTr; /*do not advance automatically*/ {
		// 没有空闲空间了
		free := int64(limit - bytesUsed - overhead)
		if free <= 0 {
			break // bail out early
		}

		// Search for the least element on a given tier (by transmit count) as
		// defined in the limitedBroadcast.Less function that will fit into our
		// remaining space.
		// 大于等于greaterOrEqual
		greaterOrEqual := &limitedBroadcast{
			transmits: transmits,
			msgLen:    free,
			id:        math.MaxInt64,
		}
		// 小于等于lessThan
		lessThan := &limitedBroadcast{
			transmits: transmits + 1, // 这里需要transmits+1，所以for循环不能自动加
			msgLen:    math.MaxInt64,
			id:        math.MaxInt64,
		}
		// 在btree查找在greaterOrEqual-lessThan范围内的第一条数据
		var keep *limitedBroadcast
		q.tq.AscendRange(greaterOrEqual, lessThan, func(item btree.Item) bool {
			cur := item.(*limitedBroadcast)
			// Check if this is within our limits
			if int64(len(cur.b.Message())) > free {
				// If this happens it's a bug in the datastructure or
				// surrounding use doing something like having len(Message())
				// change over time. There's enough going on here that it's
				// probably sane to just skip it and move on for now.
				// 数据包放进btree之后，长度被修改了
				return true
			}

			// 取出当前节点 btree搜索
			keep = cur
			return false
		})

		// 没有符合条件的数据
		if keep == nil {
			// No more items of an appropriate size in the tier.
			transmits++
			continue
		}

		msg := keep.b.Message()

		// Add to slice to send
		// 计算已取出来的长度，添加到返回值
		bytesUsed += overhead + len(msg)
		toSend = append(toSend, msg)

		// Check if we should stop transmission
		// 校验keep的transmits是否超过限制，没超过就重新放入btree进行重试，用户层不做重试逻辑
		// 对方收到重复报文根据LamportTime进行去重
		q.deleteItem(keep)
		if keep.transmits+1 >= transmitLimit {
			keep.b.Finished()
		} else {
			// We need to bump this item down to another transmit tier, but
			// because it would be in the same direction that we're walking the
			// tiers, we will have to delay the reinsertion until we are
			// finished our search. Otherwise we'll possibly re-add the message
			// when we ascend to the next tier.
			keep.transmits++ // 增加transmits值
			reinsert = append(reinsert, keep)
		}
	}

	// 重新添加
	for _, cur := range reinsert {
		q.addItem(cur)
	}

	return toSend
}

// NumQueued returns the number of queued messages
func (q *TransmitLimitedQueue) NumQueued() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.lenLocked()
}

// lenLocked returns the length of the overall queue datastructure. You must
// hold the mutex.
func (q *TransmitLimitedQueue) lenLocked() int {
	if q.tq == nil {
		return 0
	}
	return q.tq.Len()
}

// Reset clears all the queued messages. Should only be used for tests.
func (q *TransmitLimitedQueue) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.walkReadOnlyLocked(false, func(cur *limitedBroadcast) bool {
		cur.b.Finished()
		return true
	})

	q.tq = nil
	q.tm = nil
	q.idGen = 0
}

// Prune will retain the maxRetain latest messages, and the rest
// will be discarded. This can be used to prevent unbounded queue sizes
func (q *TransmitLimitedQueue) Prune(maxRetain int) {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Do nothing if queue size is less than the limit
	for q.tq.Len() > maxRetain {
		item := q.tq.Max()
		if item == nil {
			break
		}
		cur := item.(*limitedBroadcast)
		cur.b.Finished()
		q.deleteItem(cur)
	}
}
