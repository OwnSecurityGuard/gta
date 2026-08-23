package mobile

import (
	"encoding/binary"
	"sync"
)

// Reassembler 把移动代理推送的 TCP 字节流按应用层协议重组为帧。
//
// 背景：移动代理抓包不能简单地把一次 TCP read 当作一个 packet——
// 应用层一条消息可能被 TCP 拆成多次 read（粘包/半包）。
// Reassembler 按 (conn_id, direction) 维护独立缓冲，产出完整的应用层帧。
//
// 分帧策略（见 MobileConfig.FrameStyle）：
//   - raw：不重组，每个数据块即一帧（解码器自行处理分包）。
//   - length_prefix：前 N 字节（大端/小端）声明 payload 长度，剩余字节为 payload。
//
// 非线程安全？否：内部加锁，多个 gRPC stream goroutine 可并发调用。
type Reassembler struct {
	style     FrameStyle
	prefixLen int
	bigEndian bool
	maxFrame  int
	maxConn   int

	mu   sync.Mutex
	bufs map[string][]byte
}

// NewReassembler 根据配置创建分帧重组器。调用方需先 applyDefaults。
func NewReassembler(cfg MobileConfig) *Reassembler {
	return &Reassembler{
		style:     cfg.FrameStyle,
		prefixLen: cfg.PrefixLen,
		bigEndian: !cfg.LittleEndian,
		maxFrame:  cfg.MaxFrameSize,
		maxConn:   cfg.MaxConnSize,
		bufs:      make(map[string][]byte),
	}
}

func (r *Reassembler) key(connID, direction string) string { return connID + "|" + direction }

// Write 追加一段数据，返回完整帧列表与因防御策略丢弃的字节数。
// 帧数据是缓冲区的切片，调用方在使用期间不应修改，必要时应复制。
func (r *Reassembler) Write(connID, direction string, data []byte) (frames [][]byte, dropped int) {
	if len(data) == 0 {
		return nil, 0
	}
	if r.style == FrameRaw {
		// raw 模式不做重组，一个数据块即一帧。
		return [][]byte{data}, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	k := r.key(connID, direction)
	buf := r.bufs[k]
	if len(buf)+len(data) > r.maxConn {
		// 单连接缓冲超限：丢弃整条连接缓冲，防御异常协议（如长度头被恶意篡改）。
		dropped += len(buf) + len(data)
		r.bufs[k] = nil
		return nil, dropped
	}
	buf = append(buf, data...)
	frames, buf, dropped = r.extract(buf)
	if len(buf) == 0 {
		delete(r.bufs, k)
	} else {
		r.bufs[k] = buf
	}
	return frames, dropped
}

// Flush 在连接关闭时调用，输出残余缓冲的尾帧。
// length_prefix 模式下不完整的残余（半包/半长度头）会被丢弃，避免把残缺数据交给解码器。
func (r *Reassembler) Flush(connID, direction string) (frames [][]byte, dropped int) {
	if r.style == FrameRaw {
		return nil, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	k := r.key(connID, direction)
	buf := r.bufs[k]
	delete(r.bufs, k)
	if len(buf) == 0 {
		return nil, 0
	}
	// 缓冲未满足一个完整帧：丢弃。
	return nil, len(buf)
}

// Drop 销毁连接时清理该连接所有方向的缓冲。
func (r *Reassembler) Drop(connID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prefix := connID + "|"
	for k := range r.bufs {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(r.bufs, k)
		}
	}
}

// extract 从 buf 中切出完整帧，返回剩余未满足长度的部分（rest）。
// 注意返回的帧是 rest 底层数组的切片，调用方使用期间不应并发修改缓冲。
func (r *Reassembler) extract(buf []byte) (frames [][]byte, rest []byte, dropped int) {
	for len(buf) >= r.prefixLen {
		n := int(r.readLen(buf[:r.prefixLen]))
		if n == 0 {
			// 零长度帧：仅消费长度头，不产出。
			buf = buf[r.prefixLen:]
			continue
		}
		if n > r.maxFrame {
			// 声明长度超上限：协议异常，丢弃整段缓冲。
			dropped += len(buf)
			buf = buf[:0]
			break
		}
		if len(buf) < r.prefixLen+n {
			break // payload 尚未收齐
		}
		frames = append(frames, buf[r.prefixLen:r.prefixLen+n])
		buf = buf[r.prefixLen+n:]
	}
	return frames, buf, dropped
}

// readLen 读取长度前缀（1/2/4 字节，按配置字节序）。
func (r *Reassembler) readLen(p []byte) uint32 {
	switch len(p) {
	case 1:
		return uint32(p[0])
	case 2:
		if r.bigEndian {
			return uint32(binary.BigEndian.Uint16(p))
		}
		return uint32(binary.LittleEndian.Uint16(p))
	default:
		if r.bigEndian {
			return binary.BigEndian.Uint32(p)
		}
		return binary.LittleEndian.Uint32(p)
	}
}
