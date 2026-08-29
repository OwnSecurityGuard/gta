package agent

import (
	"sync"
	"sync/atomic"

	"gta/pkg/event"
)

// DefaultChannelSize 是每个会话订阅 channel 的默认容量。
// 与 mobile source 的输出缓冲一致；满即丢弃（背压策略：丢包优于阻塞 server 流）。
const DefaultChannelSize = 256

// Hub 把 AgentIngest server 收到的包路由给按会话订阅的 Source。
//
// 并发约定：Deliver 的投递与 Subscribe 的取消都在 h.mu 内完成，
// 保证 cancel 返回后绝无并发写入，订阅者（Source）随后 close channel 是安全的。
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[*subscription]struct{}

	delivered    atomic.Uint64 // 成功投递给订阅者的包数
	droppedBusy  atomic.Uint64 // 订阅 channel 满丢弃（慢消费者）
	droppedNoSub atomic.Uint64 // 无订阅者丢弃（会话未在抓包 / 包先于 Source 到达）
}

// subscription 是一个会话的一个订阅者。
type subscription struct {
	ch chan event.Packet
}

// NewHub 构造空 Hub。
func NewHub() *Hub {
	return &Hub{subs: make(map[string]map[*subscription]struct{})}
}

// Subscribe 用调用方提供的 channel 订阅某会话的包流（容量由调用方决定）。
// 返回取消订阅函数。取消订阅（cancel）返回后 Hub 保证不再向 channel 写入，
// 调用方可以安全地关闭它（Source.Close 负责）。
func (h *Hub) Subscribe(sessionID string, ch chan event.Packet) func() {
	if ch == nil {
		ch = make(chan event.Packet, DefaultChannelSize)
	}
	sub := &subscription{ch: ch}
	h.mu.Lock()
	if h.subs[sessionID] == nil {
		h.subs[sessionID] = make(map[*subscription]struct{})
	}
	h.subs[sessionID][sub] = struct{}{}
	h.mu.Unlock()
	cancel := func() {
		h.mu.Lock()
		if set, ok := h.subs[sessionID]; ok {
			delete(set, sub)
			if len(set) == 0 {
				delete(h.subs, sessionID)
			}
		}
		h.mu.Unlock()
	}
	return cancel
}

// Deliver 把一批包投递给订阅了 sessionID 的 Source。
// 非阻塞：channel 满即丢弃该包（计数 droppedBusy），无订阅者丢弃（计数 droppedNoSub）。
// 返回 (delivered, dropped) 供 PushAck 统计。
func (h *Hub) Deliver(sessionID string, pkts []event.Packet) (delivered, dropped uint64) {
	for _, pkt := range pkts {
		h.mu.Lock()
		nSubs := len(h.subs[sessionID])
		sent := false
		for sub := range h.subs[sessionID] {
			select {
			case sub.ch <- pkt:
				sent = true
			default:
			}
		}
		h.mu.Unlock()
		switch {
		case sent:
			h.delivered.Add(1)
			delivered++
		case nSubs == 0:
			h.droppedNoSub.Add(1)
			dropped++
		default:
			h.droppedBusy.Add(1)
			dropped++
		}
	}
	return delivered, dropped
}

// Subscribers 返回某会话当前订阅者数（测试/观测用）。
func (h *Hub) Subscribers(sessionID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[sessionID])
}

// Stats 返回 Hub 级累计统计（测试/观测用）。
func (h *Hub) Stats() (delivered, droppedBusy, droppedNoSub uint64) {
	return h.delivered.Load(), h.droppedBusy.Load(), h.droppedNoSub.Load()
}
