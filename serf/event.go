package serf

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/serf/extpkg/memberlist"
)

// EventType are all the types of events that may occur and be sent
// along the Serf channel.
type EventType int

const (
	EventMemberJoin EventType = iota
	EventMemberLeave
	EventMemberFailed
	EventMemberUpdate
	EventMemberReap
	EventUser
	EventQuery
)

func (t EventType) String() string {
	switch t {
	case EventMemberJoin:
		return "member-join"
	case EventMemberLeave:
		return "member-leave"
	case EventMemberFailed:
		return "member-failed"
	case EventMemberUpdate:
		return "member-update"
	case EventMemberReap:
		return "member-reap"
	case EventUser:
		return "user"
	case EventQuery:
		return "query"
	default:
		panic(fmt.Sprintf("unknown event type: %d", t))
	}
}

// Event is a generic interface for exposing Serf events
// Clients will usually need to use a type switches to get
// to a more useful type
type Event interface {
	EventType() EventType
	String() string
}

// MemberEvent is the struct used for member related events
// Because Serf coalesces events, an event may contain multiple members.
// MemberEvent是用于成员相关事件的结构体，因为Serf合并事件，一个事件可能包含多个成员。
type MemberEvent struct {
	Type    EventType // 事件类型  供上游系统选择自己关心的事件处理
	Members []Member
}

func (m MemberEvent) EventType() EventType {
	return m.Type
}

func (m MemberEvent) String() string {
	switch m.Type {
	case EventMemberJoin:
		return "member-join"
	case EventMemberLeave:
		return "member-leave"
	case EventMemberFailed:
		return "member-failed"
	case EventMemberUpdate:
		return "member-update"
	case EventMemberReap:
		return "member-reap"
	default:
		panic(fmt.Sprintf("unknown event type: %d", m.Type))
	}
}

// UserEvent is the struct used for events that are triggered
// by the user and are not related to members
type UserEvent struct {
	LTime    LamportTime
	Name     string
	Payload  []byte
	Coalesce bool
}

func (u UserEvent) EventType() EventType {
	return EventUser
}

func (u UserEvent) String() string {
	return fmt.Sprintf("user-event: %s", u.Name)
}

// Query is the struct used by EventQuery type events
// Query是EventQuery类型事件使用的结构
type Query struct {
	LTime   LamportTime
	Name    string
	Payload []byte

	serf        *Serf
	id          uint32    // ID is not exported, since it may change
	addr        []byte    // Address to respond to
	port        uint16    // Port to respond to
	sourceNode  string    // Node name to respond to
	deadline    time.Time // Must respond by this deadline  超时的绝对时间
	relayFactor uint8     // Number of duplicate responses to relay back to sender
	respLock    sync.Mutex
}

func (q *Query) EventType() EventType {
	return EventQuery
}

func (q *Query) String() string {
	return fmt.Sprintf("query: %s", q.Name)
}

// Deadline returns the time by which a response must be sent
func (q *Query) Deadline() time.Time {
	return q.deadline
}

// 构建query的相应内容
func (q *Query) createResponse(buf []byte) messageQueryResponse {
	// Create response
	return messageQueryResponse{
		LTime:   q.LTime, // 请求中的LTime
		ID:      q.id,    // 请求中的Id
		From:    q.serf.config.NodeName,
		Payload: buf,
	}
}

// Check response size
// 校验响应内容长度
func (q *Query) checkResponseSize(resp []byte) error {
	if len(resp) > q.serf.config.QueryResponseSizeLimit {
		return fmt.Errorf("response exceeds limit of %d bytes", q.serf.config.QueryResponseSizeLimit)
	}
	return nil
}

// 响应query信息
func (q *Query) respondWithMessageAndResponse(raw []byte, resp messageQueryResponse) error {
	// Check the size limit 长度限制
	if err := q.checkResponseSize(raw); err != nil {
		return err
	}

	q.respLock.Lock()
	defer q.respLock.Unlock()

	// Check if we've already responded
	// 是否已经响应过
	if q.deadline.IsZero() {
		return fmt.Errorf("response already sent")
	}

	// Ensure we aren't past our response deadline
	// 确保我们没有超过最后期限
	if time.Now().After(q.deadline) {
		return fmt.Errorf("response is past the deadline")
	}

	// Send the response directly to the originator
	// 将响应直接发送给请求者
	udpAddr := net.UDPAddr{IP: q.addr, Port: int(q.port)}

	addr := memberlist.Address{
		Addr: udpAddr.String(),
		Name: q.sourceNode,
	}
	if err := q.serf.memberlist.SendToAddress(addr, raw); err != nil {
		return err
	}

	// Relay the response through up to relayFactor other nodes
	// 通过中继响应其他节点
	if err := q.serf.relayResponse(q.relayFactor, udpAddr, q.sourceNode, &resp); err != nil {
		return err
	}

	// Clear the deadline, responses sent
	// 清除截止日期，标记已回复
	q.deadline = time.Time{}

	return nil
}

// Respond is used to send a response to the user query
// response用于向用户query发送响应
func (q *Query) Respond(buf []byte) error {
	// Create response
	// 响应内容
	resp := q.createResponse(buf)

	// Encode response 编码+类型
	raw, err := encodeMessage(messageQueryResponseType, resp)
	if err != nil {
		return fmt.Errorf("failed to format response: %v", err)
	}

	if err := q.respondWithMessageAndResponse(raw, resp); err != nil {
		return fmt.Errorf("failed to respond to key query: %v", err)
	}

	return nil
}
