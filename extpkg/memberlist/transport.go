package memberlist

import (
	"fmt"
	"net"
	"time"
)

// Packet is used to provide some metadata about incoming packets from peers
// over a packet connection, as well as the packet payload.
// udp接收后的包装
type Packet struct {
	// Buf has the raw contents of the packet.
	Buf []byte

	// From has the address of the peer. This is an actual net.Addr so we
	// can expose some concrete details about incoming packets.
	From net.Addr

	// Timestamp is the time when the packet was received. This should be
	// taken as close as possible to the actual receipt time to help make an
	// accurate RTT measurement during probes.
	Timestamp time.Time
}

// Transport is used to abstract over communicating with other peers. The packet
// interface is assumed to be best-effort and the stream interface is assumed to
// be reliable.
// 链接的管理  建立链接 发送数据 获取数据 net_transport.go
type Transport interface {
	// FinalAdvertiseAddr is given the user's configured values (which
	// might be empty) and returns the desired IP and port to advertise to
	// the rest of the cluster.
	// FinalAdvertiseAddr得到用户配置的值(可能为空)，并返回所需的IP和端口，以便向集群的其余部分发布
	// 返回广播地址
	FinalAdvertiseAddr(ip string, port int) (net.IP, int, error)

	// WriteTo is a packet-oriented interface that fires off the given
	// payload to the given address in a connectionless fashion. This should
	// return a time stamp that's as close as possible to when the packet
	// was transmitted to help make accurate RTT measurements during probes.
	//
	// This is similar to net.PacketConn, though we didn't want to expose
	// that full set of required methods to keep assumptions about the
	// underlying plumbing to a minimum. We also treat the address here as a
	// string, similar to Dial, so it's network neutral, so this usually is
	// in the form of "host:port".
	WriteTo(b []byte, addr string) (time.Time, error)

	// PacketCh returns a channel that can be read to receive incoming
	// packets from other peers. How this is set up for listening is left as
	// an exercise for the concrete transport implementations.
	PacketCh() <-chan *Packet

	// DialTimeout is used to create a connection that allows us to perform
	// two-way communication with a peer. This is generally more expensive
	// than packet connections so is used for more infrequent operations
	// such as anti-entropy or fallback probes if the packet-oriented probe
	// failed.
	// DialTimeout用于创建一个连接，允许我们与对等体进行双向通信。
	// 这通常比包连接更昂贵，因此用于更不频繁的操作，如反熵或面向包的探测失败时的回退探测。
	DialTimeout(addr string, timeout time.Duration) (net.Conn, error)

	// StreamCh returns a channel that can be read to handle incoming stream
	// connections from other peers. How this is set up for listening is
	// left as an exercise for the concrete transport implementations.
	// StreamCh返回一个通道，可以读取该通道来处理来自其他对等体的流连接。
	// 如何设置侦听，留给具体的传输实现来实现。
	StreamCh() <-chan net.Conn

	// Shutdown is called when memberlist is shutting down; this gives the
	// transport a chance to clean up any listeners.
	// 当成员列表关闭时调用Shutdown;这使传输有机会清理任何侦听器。
	Shutdown() error
}

type Address struct {
	// Addr is a network address as a string, similar to Dial. This usually is
	// in the form of "host:port". This is required.
	Addr string

	// Name is the name of the node being addressed. This is optional but
	// transports may require it.
	Name string
}

func (a *Address) String() string {
	if a.Name != "" {
		return fmt.Sprintf("%s (%s)", a.Name, a.Addr)
	}
	return a.Addr
}

type IngestionAwareTransport interface {
	Transport
	// IngestPacket pulls a single packet off the conn, and only closes it if shouldClose is true.
	IngestPacket(conn net.Conn, addr net.Addr, now time.Time, shouldClose bool) error
	// IngestStream hands off the conn to the transport and doesn't close it.
	IngestStream(conn net.Conn) error
}

type NodeAwareTransport interface {
	IngestionAwareTransport
	WriteToAddress(b []byte, addr Address) (time.Time, error)
	DialAddressTimeout(addr Address, timeout time.Duration) (net.Conn, error)
}

type shimNodeAwareTransport struct {
	Transport
}

var _ NodeAwareTransport = (*shimNodeAwareTransport)(nil)

func (t *shimNodeAwareTransport) IngestPacket(conn net.Conn, addr net.Addr, now time.Time, shouldClose bool) error {
	iat, ok := t.Transport.(IngestionAwareTransport)
	if !ok {
		panic("shimNodeAwareTransport does not support IngestPacket")
	}
	return iat.IngestPacket(conn, addr, now, shouldClose)
}

func (t *shimNodeAwareTransport) IngestStream(conn net.Conn) error {
	iat, ok := t.Transport.(IngestionAwareTransport)
	if !ok {
		panic("shimNodeAwareTransport does not support IngestStream")
	}
	return iat.IngestStream(conn)
}

func (t *shimNodeAwareTransport) WriteToAddress(b []byte, addr Address) (time.Time, error) {
	return t.WriteTo(b, addr.Addr)
}

func (t *shimNodeAwareTransport) DialAddressTimeout(addr Address, timeout time.Duration) (net.Conn, error) {
	return t.DialTimeout(addr.Addr, timeout)
}
