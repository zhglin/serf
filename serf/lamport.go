package serf

import (
	"sync/atomic"
)

// LamportClock is a thread safe implementation of a lamport clock. It
// uses efficient atomic operations for all of its functions, falling back
// to a heavy lock only if there are enough CAS failures.
type LamportClock struct {
	counter uint64
}

// LamportTime is the value of a LamportClock.
type LamportTime uint64

// Time is used to return the current value of the lamport clock
// 获取本地的lamport时钟
func (l *LamportClock) Time() LamportTime {
	return LamportTime(atomic.LoadUint64(&l.counter))
}

// Increment is used to increment and return the value of the lamport clock
// 本地lamport时钟+1 发送前+1
func (l *LamportClock) Increment() LamportTime {
	return LamportTime(atomic.AddUint64(&l.counter, 1))
}

// Witness is called to update our local clock if necessary after
// witnessing a clock value received from another process
// 在见证从另一个进程接收到的时钟值之后，如果有必要，调用Witness来更新本地时钟
func (l *LamportClock) Witness(v LamportTime) {
WITNESS:
	// If the other value is old, we do not need to do anything
	// 如果另一个值是旧的，我们不需要做任何事情
	cur := atomic.LoadUint64(&l.counter)
	other := uint64(v)
	if other < cur {
		return
	}

	// Ensure that our local clock is at least one ahead.
	// 确保我们当地的时钟至少快一点。
	if !atomic.CompareAndSwapUint64(&l.counter, cur, other+1) {
		// The CAS failed, so we just retry. Eventually our CAS should
		// succeed or a future witness will pass us by and our witness
		// will end.
		goto WITNESS
	}
}
