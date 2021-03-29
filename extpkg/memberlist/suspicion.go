package memberlist

import (
	"math"
	"sync/atomic"
	"time"
)

// suspicion manages the suspect timer for a node and provides an interface
// to accelerate the timeout as we get more independent confirmations that
// a node is suspect.
// suspicion管理节点的可疑计时器，并提供一个接口，在我们获得更多节点可疑的独立确认时加速超时。
type suspicion struct {
	// n is the number of independent confirmations we've seen. This must
	// be updated using atomic instructions to prevent contention with the
	// timer callback.
	// n是独立确认的次数。这必须使用原子指令进行更新，以防止与计时器回调发生争用。
	n int32

	// k is the number of independent confirmations we'd like to see in
	// order to drive the timer to its minimum value.
	// k是我们希望看到的独立确认数，以便将计时器驱动到其最小值。
	k int32

	// min is the minimum timer value.
	// min为最小定时器值。
	min time.Duration

	// max is the maximum timer value.
	// max为最大定时器值。
	max time.Duration

	// start captures the timestamp when we began the timer. This is used
	// so we can calculate durations to feed the timer during updates in
	// a way the achieves the overall time we'd like.
	// start捕获我们开始计时器时的时间戳。这样我们就可以计算在更新期间以我们想要的方式提供给计时器的持续时间。
	start time.Time

	// timer is the underlying timer that implements the timeout.
	// timer是实现超时的底层计时器。
	timer *time.Timer

	// f is the function to call when the timer expires. We hold on to this
	// because there are cases where we call it directly.
	// f是定时器到期时调用的函数。我们使用这个是因为有些情况下我们直接调用它。
	timeoutFn func()

	// confirmations is a map of "from" nodes that have confirmed a given
	// node is suspect. This prevents double counting.
	// confirmations是“来自”节点的映射，这些节点已经确认了给定节点是可疑的。这可以防止重复计算。
	confirmations map[string]struct{}
}

// newSuspicion returns a timer started with the max time, and that will drive
// to the min time after seeing k or more confirmations. The from node will be
// excluded from confirmations since we might get our own suspicion message
// gossiped back to us. The minimum time will be used if no confirmations are
// called for (k <= 0).
func newSuspicion(from string, k int, min time.Duration, max time.Duration, fn func(int)) *suspicion {
	s := &suspicion{
		k:             int32(k),
		min:           min,
		max:           max,
		confirmations: make(map[string]struct{}),
	}

	// Exclude the from node from any confirmations.
	// 从任何确认中排除from节点。
	s.confirmations[from] = struct{}{}

	// Pass the number of confirmations into the timeout function for
	// easy telemetry.
	// 将确认数传递给超时函数，以便于遥测。
	s.timeoutFn = func() {
		fn(int(atomic.LoadInt32(&s.n)))
	}

	// If there aren't any confirmations to be made then take the min
	// time from the start.
	// 如果没有任何确认需要进行，那么从一开始就花最少的时间。
	timeout := max
	if k < 1 {
		timeout = min
	}

	// 定时器执行超时函数
	s.timer = time.AfterFunc(timeout, s.timeoutFn)

	// Capture the start time right after starting the timer above so
	// we should always err on the side of a little longer timeout if
	// there's any preemption that separates this and the step above.
	// 在上面的计时器启动后立即捕获开始时间，因此如果有任何抢占将此步骤与上面的步骤分开，我们应该总是错误地设置稍长的超时。
	s.start = time.Now()
	return s
}

// remainingSuspicionTime takes the state variables of the suspicion timer and
// calculates the remaining time to wait before considering a node dead. The
// return value can be negative, so be prepared to fire the timer immediately in
// that case.
// 获取怀疑计时器的状态变量，并计算节点死亡前的剩余等待时间。返回值可以是负数，因此在这种情况下准备立即触发计时器。
func remainingSuspicionTime(n, k int32, elapsed time.Duration, min, max time.Duration) time.Duration {
	// n/k  已确认数在总数里的占比
	frac := math.Log(float64(n)+1.0) / math.Log(float64(k)+1.0)
	// min不变  max-n个确认节点在max-min中占的比例
	raw := max.Seconds() - frac*(max.Seconds()-min.Seconds())
	timeout := time.Duration(math.Floor(1000.0*raw)) * time.Millisecond
	if timeout < min {
		timeout = min
	}

	// We have to take into account the amount of time that has passed so
	// far, so we get the right overall timeout.
	// 我们必须考虑到目前为止所经过的时间，以便获得正确的总体超时。
	return timeout - elapsed
}

// Confirm registers that a possibly new peer has also determined the given
// node is suspect. This returns true if this was new information, and false
// if it was a duplicate confirmation, or if we've got enough confirmations to
// hit the minimum.
// Confirm注册了一个可能的新对等节点也确定了给定节点是可疑的。
// 如果这是新信息，则返回true;
// 如果这是重复的确认，或者如果我们有足够的确认达到最小值，则返回false。
func (s *suspicion) Confirm(from string) bool {
	// If we've got enough confirmations then stop accepting them.
	// 如果我们得到了足够的确认信息，那么就停止接受它们。
	if atomic.LoadInt32(&s.n) >= s.k {
		return false
	}

	// Only allow one confirmation from each possible peer.
	// 每个可能的对等节点只允许一次确认。
	if _, ok := s.confirmations[from]; ok {
		return false
	}

	// 记录确认的节点
	s.confirmations[from] = struct{}{}

	// Compute the new timeout given the current number of confirmations and
	// adjust the timer. If the timeout becomes negative *and* we can cleanly
	// stop the timer then we will call the timeout function directly from
	// here.
	// 计算给定当前确认数的新超时，并调整计时器。如果超时变成负数，我们可以停止计时器，然后我们将直接从这里调用timeout函数。
	n := atomic.AddInt32(&s.n, 1)                                      // 增加确认次数
	elapsed := time.Since(s.start)                                     // n个确认经过的时长
	remaining := remainingSuspicionTime(n, s.k, elapsed, s.min, s.max) // 重新计算超时时间
	if s.timer.Stop() {                                                // 停止计时器
		if remaining > 0 {
			s.timer.Reset(remaining) // 重新开启计时器
		} else {
			go s.timeoutFn() // 直接执行超时函数
		}
	}
	return true
}
