package memberlist

import (
	"bytes"
	"compress/lzw"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/go-msgpack/codec"
	"github.com/sean-/seed"
)

// pushPullScale is the minimum number of nodes
// before we start scaling the push/pull timing. The scale
// effect is the log2(Nodes) - log2(pushPullScale). This means
// that the 33rd node will cause us to double the interval,
// while the 65th will triple it.
const pushPullScaleThreshold = 32

const (
	// Constant litWidth 2-8
	lzwLitWidth = 8
)

func init() {
	seed.Init()
}

// Decode reverses the encode operation on a byte slice input
func decode(buf []byte, out interface{}) error {
	r := bytes.NewReader(buf)
	hd := codec.MsgpackHandle{}
	dec := codec.NewDecoder(r, &hd)
	return dec.Decode(out)
}

// Encode writes an encoded object to a new bytes buffer
// Encode将已编码的对象写入新的字节缓冲区
// 对响应数据进行编码
func encode(msgType messageType, in interface{}) (*bytes.Buffer, error) {
	buf := bytes.NewBuffer(nil)
	buf.WriteByte(uint8(msgType))
	hd := codec.MsgpackHandle{}
	enc := codec.NewEncoder(buf, &hd)
	err := enc.Encode(in)
	return buf, err
}

// Returns a random offset between 0 and n
func randomOffset(n int) int {
	if n == 0 {
		return 0
	}
	return int(rand.Uint32() % uint32(n))
}

// suspicionTimeout computes the timeout that should be used when
// a node is suspected
// suspicious timeout计算节点被怀疑时应该使用的超时时间  最小的
// n 是所有的节点数
// interval 是ping的周期时间
// 将所有节点数以log10缩小 缩小的倍数*interval(代表整个集群需要的周期数)  *suspicionMult(放大一些时间)
func suspicionTimeout(suspicionMult, n int, interval time.Duration) time.Duration {
	nodeScale := math.Max(1.0, math.Log10(math.Max(1.0, float64(n))))
	// multiply by 1000 to keep some precision because time.Duration is an int64 type
	// 乘以1000以保持一定的精度，因为时间。Duration是一个int64类型
	timeout := time.Duration(suspicionMult) * time.Duration(nodeScale*1000) * interval / 1000
	return timeout
}

// retransmitLimit computes the limit of retransmissions
// 获取board的重试次数 随着集群规模动态调整重试次数 每增加一个量级就增加一倍retransmitMult
func retransmitLimit(retransmitMult, n int) int {
	nodeScale := math.Ceil(math.Log10(float64(n + 1)))
	limit := retransmitMult * int(nodeScale)
	return limit
}

// shuffleNodes randomly shuffles the input nodes using the Fisher-Yates shuffle
// 对nodes进行重新洗牌
func shuffleNodes(nodes []*nodeState) {
	n := len(nodes)
	rand.Shuffle(n, func(i, j int) {
		nodes[i], nodes[j] = nodes[j], nodes[i]
	})
}

// pushPushScale is used to scale the time interval at which push/pull
// syncs take place. It is used to prevent network saturation as the
// cluster size grows
// pushPushScale用于衡量推/拉同步发生的时间间隔。它用于防止随着簇大小增长而导致的网络饱和
func pushPullScale(interval time.Duration, n int) time.Duration {
	// Don't scale until we cross the threshold
	// 节点数没有达到pushPullScaleThreshold不更改时间，使用配置的间隔时间
	if n <= pushPullScaleThreshold {
		return interval
	}

	// 随着节点数的增加 增加对应的时间间隔 节点数放大的倍数
	multiplier := math.Ceil(math.Log2(float64(n))-math.Log2(pushPullScaleThreshold)) + 1.0
	return time.Duration(multiplier) * interval
}

// moveDeadNodes moves nodes that are dead and beyond the gossip to the dead interval
// to the end of the slice and returns the index of the first moved node.
// 移动stateDead的节点放到最后 返回第一个dead的节点下标
func moveDeadNodes(nodes []*nodeState, gossipToTheDeadTime time.Duration) int {
	numDead := 0
	n := len(nodes)
	for i := 0; i < n-numDead; i++ {
		if nodes[i].State != StateDead {
			continue
		}

		// Respect the gossip to the dead interval
		// 已经标记为dead但是未达到gossipToTheDeadTime时间间隔
		if time.Since(nodes[i].StateChange) <= gossipToTheDeadTime {
			continue
		}

		// Move this node to the end
		// 已经dead的节点交换到最后
		nodes[i], nodes[n-numDead-1] = nodes[n-numDead-1], nodes[i]
		numDead++
		i-- // 交换过来的节点可能是个dead节点 要重新进行校验
	}
	return n - numDead
}

// kRandomNodes is used to select up to k random nodes, excluding any nodes where
// the filter function returns true. It is possible that less than k nodes are
// returned.
// kRandomNodes用于选择最多k个随机节点，不包括filter函数返回true的任何节点。返回的节点可能少于k个。
func kRandomNodes(k int, nodes []*nodeState, filterFn func(*nodeState) bool) []*nodeState {
	n := len(nodes)
	kNodes := make([]*nodeState, 0, k)
OUTER:
	// Probe up to 3*n times, with large n this is not necessary
	// since k << n, but with small n we want search to be
	// exhaustive
	for i := 0; i < 3*n && len(kNodes) < k; i++ {
		// Get random node
		// 每次在0-n之间随机 会有重复的情况
		idx := randomOffset(n)
		node := nodes[idx]

		// Give the filter a shot at it.
		// filterFn过滤
		if filterFn != nil && filterFn(node) {
			continue OUTER
		}

		// Check if we have this node already
		// 之前已经被随机到了
		for j := 0; j < len(kNodes); j++ {
			if node == kNodes[j] {
				continue OUTER
			}
		}

		// Append the node
		kNodes = append(kNodes, node)
	}
	return kNodes
}

// makeCompoundMessage takes a list of messages and generates
// a single compound message containing all of them
// makeCompoundMessage接受一个消息列表，并生成一个包含所有消息的复合消息
// [类型 1个字节][消息数 1个字节][每个消息长度 每个两字节][每个消息内容....]
func makeCompoundMessage(msgs [][]byte) *bytes.Buffer {
	// Create a local buffer
	buf := bytes.NewBuffer(nil)

	// Write out the type  长度
	buf.WriteByte(uint8(compoundMsg))

	// Write out the number of message  消息数量
	buf.WriteByte(uint8(len(msgs)))

	// Add the message lengths  每个消息的长度 2个字节
	for _, m := range msgs {
		binary.Write(buf, binary.BigEndian, uint16(len(m)))
	}

	// Append the messages  每个消息内容
	for _, m := range msgs {
		buf.Write(m)
	}

	return buf
}

// decodeCompoundMessage splits a compound message and returns
// the slices of individual messages. Also returns the number
// of truncated messages and any potential error
// decodeCompoundMessage拆分复合消息并返回单个消息的片段。还返回截断消息的数量和任何潜在错误
func decodeCompoundMessage(buf []byte) (trunc int, parts [][]byte, err error) {
	if len(buf) < 1 {
		err = fmt.Errorf("missing compound length byte")
		return
	}
	numParts := uint8(buf[0]) // 代表这个复合消息里面有几个消息
	buf = buf[1:]

	// Check we have enough bytes
	// 每个消息会额外使用两个字节记录长度
	if len(buf) < int(numParts*2) {
		err = fmt.Errorf("truncated len slice")
		return
	}

	// Decode the lengths
	// 每个消息的长度 每个消息使用两个字节记录长度
	lengths := make([]uint16, numParts)
	for i := 0; i < int(numParts); i++ {
		lengths[i] = binary.BigEndian.Uint16(buf[i*2 : i*2+2])
	}
	buf = buf[numParts*2:]

	// Split each message
	for idx, msgLen := range lengths {
		// 长度不足 丢弃 trunc是丢弃的数量
		if len(buf) < int(msgLen) {
			trunc = int(numParts) - idx
			return
		}

		// Extract the slice, seek past on the buffer
		// 提取片，查找过去的缓冲区
		slice := buf[:msgLen]
		buf = buf[msgLen:]
		parts = append(parts, slice)
	}
	return
}

// compressPayload takes an opaque input buffer, compresses it
// and wraps it in a compress{} message that is encoded.
// 数据压缩
// compressPayload接受一个不透明的输入缓冲区，对其进行压缩，并将其封装在经过编码的compress{}消息中。
func compressPayload(inp []byte) (*bytes.Buffer, error) {
	var buf bytes.Buffer
	compressor := lzw.NewWriter(&buf, lzw.LSB, lzwLitWidth)

	_, err := compressor.Write(inp)
	if err != nil {
		return nil, err
	}

	// Ensure we flush everything out
	if err := compressor.Close(); err != nil {
		return nil, err
	}

	// Create a compressed message
	c := compress{
		Algo: lzwAlgo,
		Buf:  buf.Bytes(),
	}
	return encode(compressMsg, &c)
}

// decompressPayload is used to unpack an encoded compress{}
// message and return its payload uncompressed
// decompressPayload用于解压缩一个已编码的compress{}消息，并返回其未压缩的有效负载
func decompressPayload(msg []byte) ([]byte, error) {
	// Decode the message
	var c compress
	if err := decode(msg, &c); err != nil {
		return nil, err
	}
	return decompressBuffer(&c)
}

// decompressBuffer is used to decompress the buffer of
// a single compress message, handling multiple algorithms
// 对压缩的数据进行解压缩
func decompressBuffer(c *compress) ([]byte, error) {
	// Verify the algorithm
	if c.Algo != lzwAlgo {
		return nil, fmt.Errorf("Cannot decompress unknown algorithm %d", c.Algo)
	}

	// Create a uncompressor
	uncomp := lzw.NewReader(bytes.NewReader(c.Buf), lzw.LSB, lzwLitWidth)
	defer uncomp.Close()

	// Read all the data
	var b bytes.Buffer
	_, err := io.Copy(&b, uncomp)
	if err != nil {
		return nil, err
	}

	// Return the uncompressed bytes
	return b.Bytes(), nil
}

// joinHostPort returns the host:port form of an address, for use with a
// transport.
func joinHostPort(host string, port uint16) string {
	return net.JoinHostPort(host, strconv.Itoa(int(port)))
}

// hasPort is given a string of the form "host", "host:port", "ipv6::address",
// or "[ipv6::address]:port", and returns true if the string includes a port.
func hasPort(s string) bool {
	// IPv6 address in brackets.
	if strings.LastIndex(s, "[") == 0 {
		return strings.LastIndex(s, ":") > strings.LastIndex(s, "]")
	}

	// Otherwise the presence of a single colon determines if there's a port
	// since IPv6 addresses outside of brackets (count > 1) can't have a
	// port.
	return strings.Count(s, ":") == 1
}

// ensurePort makes sure the given string has a port number on it, otherwise it
// appends the given port as a default.
func ensurePort(s string, port int) string {
	if hasPort(s) {
		return s
	}

	// If this is an IPv6 address, the join call will add another set of
	// brackets, so we have to trim before we add the default port.
	s = strings.Trim(s, "[]")
	s = net.JoinHostPort(s, strconv.Itoa(port))
	return s
}
