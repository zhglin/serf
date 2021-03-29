package memberlist

// MergeDelegate is used to involve a client in
// a potential cluster merge operation. Namely, when
// a node does a TCP push/pull (as part of a join),
// the delegate is involved and allowed to cancel the join
// based on custom logic. The merge delegate is NOT invoked
// as part of the push-pull anti-entropy.
// merge delegate用于在潜在的集群合并操作中涉及客户机。
// 也就是说，当节点执行TCP push/pull(作为连接的一部分)时，委托会参与进来，并允许基于自定义逻辑取消连接。
// 合并委托不作为推拉反熵的一部分调用。
type MergeDelegate interface {
	// NotifyMerge is invoked when a merge could take place.
	// Provides a list of the nodes known by the peer. If
	// the return value is non-nil, the merge is canceled.
	// 当合并发生时，会调用NotifyMerge。提供对等体已知的节点列表。如果返回值是非nil，则取消合并。
	NotifyMerge(peers []*Node) error
}
