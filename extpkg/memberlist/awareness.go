package memberlist

import (
	"sync"
	"time"

	"github.com/armon/go-metrics"
)

// awareness manages a simple metric for tracking the estimated health of the
// local node. Health is primary the node's ability to respond in the soft
// real-time manner required for correct health checking of other nodes in the
// cluster.
// 用于跟踪估计的本地节点的运行状况。“健康”是节点以软实时方式响应集群中其他节点正确健康检查所需的主要能力。
// 动态调整超时时间  节点关闭或者网络问题都可能导致ping的结果失败 通过调整超时时间来调整网络运行状况
type awareness struct {
	sync.RWMutex

	// max is the upper threshold for the timeout scale (the score will be
	// constrained to be from 0 <= score < max).
	// max是超时尺度的上限阈值(分数将被限制为0 <= score < max)。
	max int

	// score is the current awareness score. Lower values are healthier and
	// zero is the minimum value.
	// score是当前的意识评分。较低的值更健康，零是最小值。
	score int
}

// newAwareness returns a new awareness object.
func newAwareness(max int) *awareness {
	return &awareness{
		max:   max,
		score: 0,
	}
}

// ApplyDelta takes the given delta and applies it to the score in a thread-safe
// manner. It also enforces a floor of zero and a max of max, so deltas may not
// change the overall score if it's railed at one of the extremes.
// ApplyDelta获取给定的增量值，并以线程安全的方式将其应用到分数上。
// 它还强制下限为0，最大值为max，因此如果被限制在一个极端，delta可能不会改变总体分数。
func (a *awareness) ApplyDelta(delta int) {
	a.Lock()
	initial := a.score
	a.score += delta
	if a.score < 0 {
		a.score = 0
	} else if a.score > (a.max - 1) {
		a.score = (a.max - 1)
	}
	final := a.score
	a.Unlock()

	if initial != final {
		metrics.SetGauge([]string{"memberlist", "health", "score"}, float32(final))
	}
}

// GetHealthScore returns the raw health score.
func (a *awareness) GetHealthScore() int {
	a.RLock()
	score := a.score
	a.RUnlock()
	return score
}

// ScaleTimeout takes the given duration and scales it based on the current
// score. Less healthyness will lead to longer timeouts.
// ScaleTimeout获取给定的持续时间，并根据当前得分对其进行缩放。健康状况不佳会导致超时时间较长。
func (a *awareness) ScaleTimeout(timeout time.Duration) time.Duration {
	a.RLock()
	score := a.score
	a.RUnlock()
	return timeout * (time.Duration(score) + 1)
}
