package memberlist

// ConflictDelegate is a used to inform a client that
// a node has attempted to join which would result in a
// name conflict. This happens if two clients are configured
// with the same name but different addresses.
// node同名 不同address,port的冲突回调  existing已存在的 other新增加的
type ConflictDelegate interface {
	// NotifyConflict is invoked when a name conflict is detected
	NotifyConflict(existing, other *Node)
}
