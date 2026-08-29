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
//
// 丢弃记录约定（重连期间允许丢包，必须明确记录）：
//   - 无订阅者 → hub 级 droppedNoSub；
//   - 订阅 channel 满 → 按订阅者逐个计入 sub.dropped（可在 Source.Stats 观察）
//     并累计进 hub 级 droppedBusy。
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[*Subscription]struct{}

	delivered    atomic.Uint64 // 成功投递给订阅者的包次数（多订阅者会重复计）
	droppedBusy  atomic.Uint64 // 订阅 channel 满丢弃次数（按订阅者逐个计）
	droppedNoSub atomic.Uint64 // 无订阅者丢弃的包数
}

// Subscription 是一个会话的一个订阅者（对应一个 agent Source）。
type Subscription struct {
	ch chan event.Packet

	delivered uint64 // 成功投递给本订阅者的包数
	dropped   uint64 // 本订阅者 channel 满被丢弃的包数
	bytesIn   uint64 // 成功投递的字节数
}

// Packets 返回本订阅者的包通道。channel 由 Source 创建并负责关闭。
func (s *Subscription) Packets() <-chan event.Packet { return s.ch }

// Stats 返回本订阅者的投递/丢弃统计（Source.Stats 透出用）。
func (s *Subscription) Stats() (delivered, dropped, bytesIn uint64) {
	return atomic.LoadUint64(&s.delivered), atomic.LoadUint64(&s.dropped), atomic.LoadUint64(&s.bytesIn)
}

// NewHub 构造空 Hub。
func NewHub() *Hub {
	return &Hub{subs: make(map[string]map[*Subscription]struct{})}
}

// Subscribe 用调用方提供的 channel 订阅某会话的包流（容量由调用方决定）。
// 返回订阅句柄与取消订阅函数。取消订阅（cancel）返回后 Hub 保证不再向
// channel 写入，调用方可以安全地关闭它（Source.Close 负责）。
func (h *Hub) Subscribe(sessionID string, ch chan event.Packet) (*Subscription, func()) {
	if ch == nil {
		ch = make(chan event.Packet, DefaultChannelSize)
	}
	sub := &Subscription{ch: ch}
	h.mu.Lock()
	if h.subs[sessionID] == nil {
		h.subs[sessionID] = make(map[*Subscription]struct{})
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
	return sub, cancel
}

// Deliver 把一批包投递给订阅了 sessionID 的所有 Source。
//
// 非阻塞：某个订阅者 channel 满时该订阅者丢这一个包（计入其 sub.dropped 与
// hub 级 droppedBusy），不影响其他订阅者，也不阻塞 server 流。
// 无订阅者时整包丢弃（hub 级 droppedNoSub）。
//
// 返回 (delivered, droppedBusy, droppedNoSub) 供 PushAck 统计。
// 多订阅者场景 delivered/droppedBusy 按订阅者逐次计，两者之和可能大于包数。
func (h *Hub) Deliver(sessionID string, pkts []event.Packet) (delivered, droppedBusy, droppedNoSub uint64) {
	// 整批持锁一次：Deliver 内的 select/default 非阻塞，锁内投递不会长时间
	// 占锁；换来与 cancel 的互斥（cancel 返回后绝无并发写）。
	h.mu.Lock()
	subs := h.subs[sessionID]
	for _, pkt := range pkts {
		if len(subs) == 0 {
			h.droppedNoSub.Add(1)
			droppedNoSub++
			continue
		}
		for sub := range subs {
			select {
			case sub.ch <- pkt:
				atomic.AddUint64(&sub.delivered, 1)
				atomic.AddUint64(&sub.bytesIn, uint64(len(pkt.Raw)))
				h.delivered.Add(1)
				delivered++
			default:
				atomic.AddUint64(&sub.dropped, 1)
				h.droppedBusy.Add(1)
				droppedBusy++
			}
		}
	}
	h.mu.Unlock()
	return delivered, droppedBusy, droppedNoSub
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
