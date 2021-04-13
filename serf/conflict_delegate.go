package serf

import (
	"github.com/hashicorp/serf/extpkg/memberlist"
)

// 用户层 node名称冲突的回调实现
type conflictDelegate struct {
	serf *Serf
}

func (c *conflictDelegate) NotifyConflict(existing, other *memberlist.Node) {
	c.serf.handleNodeConflict(existing, other)
}
