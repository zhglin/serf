package serf

import (
	"github.com/hashicorp/serf/extpkg/memberlist"
)

type eventDelegate struct {
	serf *Serf
}

// 用户层 添加节点
func (e *eventDelegate) NotifyJoin(n *memberlist.Node) {
	e.serf.handleNodeJoin(n)
}

// seft的leave消息回调
func (e *eventDelegate) NotifyLeave(n *memberlist.Node) {
	e.serf.handleNodeLeave(n)
}

func (e *eventDelegate) NotifyUpdate(n *memberlist.Node) {
	e.serf.handleNodeUpdate(n)
}
