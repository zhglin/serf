package serf

import (
	"fmt"
	"net"

	"github.com/hashicorp/serf/extpkg/memberlist"
)

type MergeDelegate interface {
	NotifyMerge([]*Member) error
}

type mergeDelegate struct {
	serf *Serf
}

func (m *mergeDelegate) NotifyMerge(nodes []*memberlist.Node) error {
	members := make([]*Member, len(nodes))
	for idx, n := range nodes {
		var err error
		members[idx], err = m.nodeToMember(n)
		if err != nil {
			return err
		}
	}
	return m.serf.config.Merge.NotifyMerge(members)
}

func (m *mergeDelegate) NotifyAlive(peer *memberlist.Node) error {
	member, err := m.nodeToMember(peer)
	if err != nil {
		return err
	}
	return m.serf.config.Merge.NotifyMerge([]*Member{member})
}

// node类型转换成member类型  处在不同的层次
func (m *mergeDelegate) nodeToMember(n *memberlist.Node) (*Member, error) {
	status := StatusNone
	if n.State == memberlist.StateLeft {
		status = StatusLeft
	}
	if err := m.validateMemberInfo(n); err != nil {
		return nil, err
	}
	return &Member{
		Name:        n.Name,
		Addr:        net.IP(n.Addr),
		Port:        n.Port,
		Tags:        m.serf.decodeTags(n.Meta),
		Status:      status,
		ProtocolMin: n.PMin,
		ProtocolMax: n.PMax,
		ProtocolCur: n.PCur,
		DelegateMin: n.DMin,
		DelegateMax: n.DMax,
		DelegateCur: n.DCur,
	}, nil
}

// validateMemberInfo checks that the data we are sending is valid
// 校验member是否合法
func (m *mergeDelegate) validateMemberInfo(n *memberlist.Node) error {
	// 校验名称
	if err := m.serf.validateNodeName(n.Name); err != nil {
		return err
	}

	// addr
	if len(n.Addr) != 4 && len(n.Addr) != 16 {
		return fmt.Errorf("IP byte length is invalid: %d bytes is not either 4 or 16", len(n.Addr))
	}

	// meta编码后的长度
	if len(n.Meta) > memberlist.MetaMaxSize {
		return fmt.Errorf("Encoded length of tags exceeds limit of %d bytes",
			memberlist.MetaMaxSize)
	}
	return nil
}
