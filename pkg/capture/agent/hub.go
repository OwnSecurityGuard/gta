package agent

import (
	"sync"
	"sync/atomic"
	"time"

	"gta/pkg/event"
)

// 投递参数。
//
// 旧实现是「channel 满即丢」（DefaultChannelSize=256，Deliver 用 select/default），
// 慢消费者（capture 主循环被 sqlite 写入拖慢）一卡就成片丢包，且无法向上游传导压力。
// 现在改为两段式：
//  1. 先把默认容量放大一个数量级，吸收正常抖动；
//  2. channel 满时带超时阻塞等待（背压），把压力沿 gRPC 流 → TCP → agent 侧
//     磁盘 spool 传导回去，而不是在服务端静默丢弃。
//
// 只有等待超过 DeliverTimeout（消费者真的跟不上或停摆）才丢包，并计入统计。
const (
	// DefaultChannelSize 是每个会话订阅 channel 的默认容量。
	// 按 1600B/帧估算 ≈ 13MB/订阅，足以扛住秒级的写入抖动。
	DefaultChannelSize = 8192

	// DefaultDeliverTimeout 是订阅 channel 满时单个包的最长等待（背压窗口）。
	// 超过后仍要丢，避免消费者彻底停摆时把 server 流永久挂住。
	// 注意：多订阅者场景总耗时 = 订阅数 × 本超时。
	DefaultDeliverTimeout = 2 * time.Second
)

// Hub 把 AgentIngest server 收到的包路由给按会话订阅的 Source。
//
// 并发约定：取消订阅（cancel）在 h.mu 内标记 closed 并摘除订阅，返回前等待所有
// 在途 Deliver 完成（sub.inflight.Wait）。因此 cancel 返回后绝无并发写入，
// 订阅者（Source）随后 close channel 是安全的——Deliver 本身不在 h.mu 内阻塞，
// 否则一个慢订阅者会连带卡住 Subscribe/cancel 与其他会话。
//
// 丢弃记录约定（重连期间允许丢包，必须明确记录）：
//   - 无订阅者 → hub 级 droppedNoSub；
//   - 订阅 channel 满且背压超时 → 按订阅者逐个计入 sub.dropped（Source.Stats 可见）
//     并累计进 hub 级 droppedBusy。
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[*Subscription]struct{}

	delivered    atomic.Uint64 // 成功投递给订阅者的包次数（多订阅者会重复计）
	droppedBusy  atomic.Uint64 // 背压超时丢弃次数（按订阅者逐个计）
	droppedNoSub atomic.Uint64 // 无订阅者丢弃的包数
}

// Subscription 是一个会话的一个订阅者（对应一个 agent Source）。
type Subscription struct {
	ch chan event.Packet

	// inflight 计数正在投递本订阅的 Deliver 调用；cancel 会等它归零后再让
	// 调用方关闭 ch，从而消除「cancel 返回后仍有并发写」的竞态。
	// 计数在 Hub.mu 内 Add（与 cancel 的摘除动作互斥），锁外 Done。
	inflight sync.WaitGroup

	// timeout 是本订阅的背压窗口（见 SubscribeOption）：
	// 0 → DefaultDeliverTimeout；<0 → 不等，channel 满即丢（旧语义）。
	timeout time.Duration

	delivered uint64 // 成功投递给本订阅者的包数
	dropped   uint64 // 本订阅者背压超时被丢弃的包数
	bytesIn   uint64 // 成功投递的字节数
}

// SubscribeOption 配置一个订阅的投递行为。
type SubscribeOption func(*Subscription)

// WithDeliverTimeout 设置本订阅的背压窗口：channel 满时单个包最多等待 d。
//   - d == 0（默认）：用 DefaultDeliverTimeout；
//   - d < 0：不等，channel 满即丢（退化为旧的非阻塞语义，仅测试/特殊场景使用）。
func WithDeliverTimeout(d time.Duration) SubscribeOption {
	return func(s *Subscription) { s.timeout = d }
}

// Packets 返回本订阅者的包通道。channel 由 Source 创建并负责关闭。
func (s *Subscription) Packets() <-chan event.Packet { return s.ch }

// Stats 返回本订阅者的投递/丢弃统计（Source.Stats 透出用）。
func (s *Subscription) Stats() (delivered, dropped, bytesIn uint64) {
	return atomic.LoadUint64(&s.delivered), atomic.LoadUint64(&s.dropped), atomic.LoadUint64(&s.bytesIn)
}

// send 投递一个包，channel 满时最多等待本订阅的背压窗口（把压力传导给上游）。
// 返回 false 表示超时/非阻塞模式下被丢弃。
func (s *Subscription) send(pkt event.Packet) bool {
	// 快路径：有空间直接写，不建 timer。
	select {
	case s.ch <- pkt:
		return true
	default:
	}
	switch {
	case s.timeout < 0:
		return false // 显式非阻塞（旧语义）
	case s.timeout == 0:
		s.timeout = DefaultDeliverTimeout
	}
	// 慢路径：channel 满，限时等待消费者腾出空间。
	t := time.NewTimer(s.timeout)
	defer t.Stop()
	select {
	case s.ch <- pkt:
		return true
	case <-t.C:
		return false
	}
}

// NewHub 构造空 Hub。
func NewHub() *Hub {
	return &Hub{subs: make(map[string]map[*Subscription]struct{})}
}

// Subscribe 用调用方提供的 channel 订阅某会话的包流（容量由调用方决定）。
// 返回订阅句柄与取消订阅函数。取消订阅（cancel）返回后 Hub 保证不再向
// channel 写入，调用方可以安全地关闭它（Source.Close 负责）。
//
// cancel 可能阻塞最多 DefaultDeliverTimeout（等在途投递结束），这是关闭安全的代价。
func (h *Hub) Subscribe(sessionID string, ch chan event.Packet, opts ...SubscribeOption) (*Subscription, func()) {
	if ch == nil {
		ch = make(chan event.Packet, DefaultChannelSize)
	}
	sub := &Subscription{ch: ch}
	for _, opt := range opts {
		opt(sub)
	}
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
		// 等在途投投递结束：此后不再有人写 sub.ch，close 才安全。
		sub.inflight.Wait()
	}
	return sub, cancel
}

// Deliver 把一批包投递给订阅了 sessionID 的所有 Source。
//
// 有界阻塞：订阅 channel 满时最多等待 DefaultDeliverTimeout，给慢消费者一个
// 追上的窗口并把背压沿 gRPC 流传回 agent；超时才丢（计入 sub.dropped 与
// hub 级 droppedBusy）。投递过程不持 h.mu，避免一个慢订阅者卡住整个 Hub。
// 无订阅者时整包丢弃（hub 级 droppedNoSub）。
//
// 返回 (delivered, droppedBusy, droppedNoSub) 供 PushAck 统计。
// 多订阅者场景 delivered/droppedBusy 按订阅者逐次计，两者之和可能大于包数。
func (h *Hub) Deliver(sessionID string, pkts []event.Packet) (delivered, droppedBusy, droppedNoSub uint64) {
	// 步骤 1：锁内取订阅快照并登记在途计数（与 cancel 的 closed 标记互斥）。
	// 快照成 slice 是为了让后续投递（可能阻塞）不持锁。
	h.mu.Lock()
	set := h.subs[sessionID]
	if len(set) == 0 {
		h.mu.Unlock()
		h.droppedNoSub.Add(uint64(len(pkts)))
		return 0, 0, uint64(len(pkts))
	}
	subs := make([]*Subscription, 0, len(set))
	for sub := range set {
		sub.inflight.Add(1)
		subs = append(subs, sub)
	}
	h.mu.Unlock()
	defer func() {
		for _, sub := range subs {
			sub.inflight.Done()
		}
	}()

	// 步骤 2：锁外逐包逐订阅者投递。Deliver 由单条 server 流串行调用，
	// 因此包序与订阅者内的投递顺序都保持。
	for _, pkt := range pkts {
		for _, sub := range subs {
			if !sub.send(pkt) {
				atomic.AddUint64(&sub.dropped, 1)
				h.droppedBusy.Add(1)
				droppedBusy++
				continue
			}
			atomic.AddUint64(&sub.delivered, 1)
			atomic.AddUint64(&sub.bytesIn, uint64(len(pkt.Raw)))
			h.delivered.Add(1)
			delivered++
		}
	}
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
