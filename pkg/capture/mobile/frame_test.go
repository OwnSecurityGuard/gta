package mobile

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// frame 构造一条长度前缀帧（4 字节大端）。
func be4Frame(payload []byte) []byte {
	buf := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(payload)))
	copy(buf[4:], payload)
	return buf
}

func newReasm(t *testing.T, style FrameStyle) *Reassembler {
	t.Helper()
	cfg := MobileConfig{ListenAddr: "127.0.0.1:0"}
	cfg.applyDefaults()
	cfg.FrameStyle = style
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate config: %v", err)
	}
	return NewReassembler(cfg)
}

func TestReassemblerLengthPrefixSingleFrame(t *testing.T) {
	r := newReasm(t, FrameLengthPrefix)
	frames, dropped := r.Write("c1", "request", be4Frame([]byte("login")))
	if dropped != 0 {
		t.Fatalf("unexpected dropped=%d", dropped)
	}
	if len(frames) != 1 {
		t.Fatalf("want 1 frame, got %d", len(frames))
	}
	if !bytes.Equal(frames[0], []byte("login")) {
		t.Fatalf("frame payload mismatch: %q", frames[0])
	}
}

// 半包/粘包：一条消息被 TCP 拆成多次 read，需要缓冲后重组。
func TestReassemblerLengthPrefixSplitAcrossReads(t *testing.T) {
	r := newReasm(t, FrameLengthPrefix)
	msg := be4Frame([]byte("hello world, this is a split message"))

	// 第一次 write：只给长度头 + payload 前 5 字节
	part1 := msg[:9]
	frames, dropped := r.Write("c1", "request", part1)
	if dropped != 0 || len(frames) != 0 {
		t.Fatalf("partial write should not emit frame, got frames=%d dropped=%d", len(frames), dropped)
	}

	// 第二次 write：剩余部分，此时应完整
	frames, dropped = r.Write("c1", "request", msg[9:])
	if dropped != 0 || len(frames) != 1 {
		t.Fatalf("want 1 completed frame, got frames=%d dropped=%d", len(frames), dropped)
	}
	if !bytes.Equal(frames[0], []byte("hello world, this is a split message")) {
		t.Fatalf("reassembled payload mismatch: %q", frames[0])
	}
}

// 粘包：一次 read 里包含多条消息。
func TestReassemblerLengthPrefixMultipleFrames(t *testing.T) {
	r := newReasm(t, FrameLengthPrefix)
	msg := append(be4Frame([]byte("aaa")), be4Frame([]byte("bbbb"))...)
	frames, dropped := r.Write("c1", "request", msg)
	if dropped != 0 || len(frames) != 2 {
		t.Fatalf("want 2 frames, got frames=%d dropped=%d", len(frames), dropped)
	}
	if !bytes.Equal(frames[0], []byte("aaa")) || !bytes.Equal(frames[1], []byte("bbbb")) {
		t.Fatalf("frame payloads mismatch: %q %q", frames[0], frames[1])
	}
}

// 长度头本身被拆开：先来 2 字节，再来 2 字节 + payload。
func TestReassemblerLengthPrefixHeaderSplit(t *testing.T) {
	r := newReasm(t, FrameLengthPrefix)
	msg := be4Frame([]byte("payload"))
	frames, _ := r.Write("c1", "request", msg[:2])
	if len(frames) != 0 {
		t.Fatalf("header-only write should not emit, got %d", len(frames))
	}
	frames, dropped := r.Write("c1", "request", msg[2:])
	if dropped != 0 || len(frames) != 1 {
		t.Fatalf("want 1 frame, got frames=%d dropped=%d", len(frames), dropped)
	}
	if !bytes.Equal(frames[0], []byte("payload")) {
		t.Fatalf("payload mismatch: %q", frames[0])
	}
}

// 连接关闭时，不完整的残余帧应被丢弃而非输出给解码器。
func TestReassemblerFlushDropsPartial(t *testing.T) {
	r := newReasm(t, FrameLengthPrefix)
	msg := be4Frame([]byte("complete"))
	// 先写完整帧 + 一条残缺帧（只有 4 字节长度头声明 100 字节）
	r.Write("c1", "request", msg)
	partial := make([]byte, 4)
	binary.BigEndian.PutUint32(partial, 100)
	r.Write("c1", "request", partial)

	frames, dropped := r.Flush("c1", "request")
	if len(frames) != 0 {
		t.Fatalf("flush should not emit partial frame, got %d", len(frames))
	}
	if dropped != 4 {
		t.Fatalf("flush should drop 4 residual bytes, got %d", dropped)
	}
}

// raw 模式：一个数据块即一帧，不重组。
func TestReassemblerRawPassthrough(t *testing.T) {
	r := newReasm(t, FrameRaw)
	frames, dropped := r.Write("c1", "request", []byte("chunk-a"))
	if dropped != 0 || len(frames) != 1 || !bytes.Equal(frames[0], []byte("chunk-a")) {
		t.Fatalf("raw write mismatch: frames=%d dropped=%d", len(frames), dropped)
	}
	frames, _ = r.Write("c1", "request", []byte("chunk-b"))
	if len(frames) != 1 || !bytes.Equal(frames[0], []byte("chunk-b")) {
		t.Fatalf("raw second write mismatch: frames=%d", len(frames))
	}
}

// 声明长度超过上限视为协议异常：整段缓冲丢弃。
func TestReassemblerOversizeFrameDropped(t *testing.T) {
	cfg := MobileConfig{ListenAddr: "127.0.0.1:0", FrameStyle: FrameLengthPrefix}
	cfg.applyDefaults()
	cfg.MaxFrameSize = 64
	r := NewReassembler(cfg)

	bad := make([]byte, 4)
	binary.BigEndian.PutUint32(bad, 1000) // 声明 1000 > 64
	frames, dropped := r.Write("c1", "request", bad)
	if len(frames) != 0 || dropped != 4 {
		t.Fatalf("oversize frame should drop buffer, got frames=%d dropped=%d", len(frames), dropped)
	}
}

// 两个方向互不影响：request 缓冲不参与 response 重组。
func TestReassemblerDirectionsIndependent(t *testing.T) {
	r := newReasm(t, FrameLengthPrefix)
	req := be4Frame([]byte("req"))
	resp := be4Frame([]byte("resp"))

	frames, _ := r.Write("c1", "request", req)
	if len(frames) != 1 {
		t.Fatalf("request frame should emit, got %d", len(frames))
	}
	// response 只有半帧（长度头）
	frames, _ = r.Write("c1", "response", resp[:4])
	if len(frames) != 0 {
		t.Fatalf("response partial should not emit, got %d", len(frames))
	}
	frames, _ = r.Write("c1", "response", resp[4:])
	if len(frames) != 1 || !bytes.Equal(frames[0], []byte("resp")) {
		t.Fatalf("response frame mismatch: %q", frames[0])
	}
}

// 小端 2 字节前缀。
func TestReassemblerLittleEndian2BytePrefix(t *testing.T) {
	cfg := MobileConfig{ListenAddr: "127.0.0.1:0"}
	cfg.applyDefaults()
	cfg.FrameStyle = FrameLengthPrefix
	cfg.PrefixLen = 2
	cfg.LittleEndian = true
	r := NewReassembler(cfg)

	payload := []byte("le2")
	msg := make([]byte, 2+len(payload))
	binary.LittleEndian.PutUint16(msg[:2], uint16(len(payload)))
	copy(msg[2:], payload)

	frames, dropped := r.Write("c1", "request", msg)
	if dropped != 0 || len(frames) != 1 || !bytes.Equal(frames[0], payload) {
		t.Fatalf("little-endian 2-byte prefix mismatch: frames=%d dropped=%d", len(frames), dropped)
	}
}
