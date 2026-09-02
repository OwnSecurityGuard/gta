package main

import (
	"sync/atomic"
	"time"

	"gta/pkg/event"
)

// 待解码溢出队列参数。
//
// 为什么需要它（修复两处丢包）：
//  1. 实时抓包时 decodeCh 满 → 旧逻辑 default 分支直接丢掉解码任务；
//  2. 无解码器期间 → 旧逻辑只保留最近 2048 个包（undecodedRingMax），超出丢最旧。
//
// 两者语义相同：包已经进了解码流水线之前、但暂时送不进去。统一用本队列承载，
// 按 FIFO 回灌，因此解码顺序严格等于抓包顺序。只有达到字节上限时才丢最旧
// （此时 raw 早已落库，可离线 decode_raw 补救）。
const (
	// pendingQueueMaxBytes 是溢出队列的默认字节上限（按包的 raw 长度累计）。
	// 64MB ≈ 4 万帧 @1600B，足以吸收解码抖动与插件热重载窗口。
	pendingQueueMaxBytes = 64 << 20
	// pendingDrainTimeout 是停机前限时排空溢出队列的上限，防止 Stop 卡死。
	pendingDrainTimeout = 5 * time.Second
)

// pendingQueue 是「待解码包」的 FIFO 溢出队列。非并发安全：
// 由 capture 主循环 goroutine 独占读写（与 raws/events 缓冲一致）。
type pendingQueue struct {
	items    []event.Packet
	bytes    int
	maxBytes int

	dropped atomic.Int64 // 因达到上限丢最旧的包数
}

// newPendingQueue 创建指定字节上限的溢出队列；maxBytes <= 0 用默认值。
func newPendingQueue(maxBytes int) *pendingQueue {
	if maxBytes <= 0 {
		maxBytes = pendingQueueMaxBytes
	}
	return &pendingQueue{maxBytes: maxBytes}
}

// len 返回当前积压包数。
func (q *pendingQueue) len() int { return len(q.items) }

// head 返回队头（调用方须确保非空）。
func (q *pendingQueue) head() event.Packet { return q.items[0] }

// headOr 返回队头；队列为空时返回 fallback。
// 用于「优先发旧包、没有旧包就发新包」的单一 select 写法。
func (q *pendingQueue) headOr(fallback event.Packet) event.Packet {
	if len(q.items) == 0 {
		return fallback
	}
	return q.items[0]
}

// popHead 弹出队头（调用方须确保非空）。
func (q *pendingQueue) popHead() {
	q.bytes -= len(q.items[0].Raw)
	q.items[0] = event.Packet{} // 释放底层字节引用
	q.items = q.items[1:]
	if len(q.items) == 0 {
		q.items = nil // 让积压排空后 slice 能被回收
	}
}

// push 追加一个包；超过字节上限时丢最旧的（保序地丢弃历史，保留较新的流上下文）。
// 丢最旧而非丢最新：被丢的包 raw 已落库可离线补救，保留较新数据能减少解码器
// 接入后重放的历史量。
func (q *pendingQueue) push(pkt event.Packet) {
	q.items = append(q.items, pkt)
	q.bytes += len(pkt.Raw)
	for q.bytes > q.maxBytes && len(q.items) > 1 {
		q.popHead()
		q.dropped.Add(1)
	}
}

// Dropped 返回因达到上限被丢弃的包数。
func (q *pendingQueue) Dropped() int64 { return q.dropped.Load() }
