package main

import (
	"gta/pkg/capture/agent/proto"
)

// batchAccumulator 按「条数阈值 + 时间阈值」攒批，控制上行带宽。
// 条数满即返回整批；时间阈值由调用方在发送循环里用 Flush 兜底。
// 非并发安全：由单个发送 goroutine 独占使用。
type batchAccumulator struct {
	maxSize int
	buf     []*proto.RawPacket
}

func newBatchAccumulator(maxSize int) *batchAccumulator {
	if maxSize <= 0 {
		maxSize = 128
	}
	return &batchAccumulator{maxSize: maxSize}
}

// Push 追加一个包；攒满 maxSize 时返回整批并清空，否则返回 nil。
func (b *batchAccumulator) Push(p *proto.RawPacket) []*proto.RawPacket {
	b.buf = append(b.buf, p)
	if len(b.buf) >= b.maxSize {
		return b.Flush()
	}
	return nil
}

// Flush 取出当前未满的批（可能为空）。
func (b *batchAccumulator) Flush() []*proto.RawPacket {
	if len(b.buf) == 0 {
		return nil
	}
	out := b.buf
	b.buf = nil
	return out
}

// Len 返回当前滞留的包数。
func (b *batchAccumulator) Len() int { return len(b.buf) }
