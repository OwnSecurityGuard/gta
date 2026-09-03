package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
	"gta/pkg/analyze"
	"gta/pkg/capture"
	"gta/pkg/capture/agent"
	"gta/pkg/capture/mobile"
	"gta/pkg/capture/pcapfile"
	"gta/pkg/decode"
	"gta/pkg/event"
	"gta/pkg/internalipc"
	"gta/pkg/internalipc/capturecontrol"
	"gta/pkg/plugin"
	protocolconfig "gta/pkg/protocol/config"
	protocolcorrelation "gta/pkg/protocol/correlation"
	protocolresolver "gta/pkg/protocol/resolver"
	"gta/pkg/schema"
	"gta/pkg/state"
	"gta/pkg/store"
)

// captureTask 是一次抓包会话的完整对象，有独立生命周期（Created → Running → Closed）。
// 由 pipelineService.StartSession 创建，run goroutine 退出时通过 onFinalize 回调通知 pipelineService。
type captureTask struct {
	// 不可变配置（创建时设定）
	sessionID  string
	dbPath     string
	port       int
	iface      string
	pcapFile   string
	sourceName string
	liveCfg    *capturecontrol.LiveConfig
	mobileCfg  *capturecontrol.MobileConfig
	// agentHub 非 nil 时，本会话额外打开 agent capture source，
	// 接收 gta-agent 经 AgentIngest server 推送的本机原始帧。
	agentHub *agent.Hub
	// agentOnly 为 true 时不打开任何基础 source，仅订阅 agent hub
	//（hub 未配置时 openCaptureSources 返回错误）。
	agentOnly bool
	start     time.Time

	// 解码插件绑定：创建时设定，运行中可经 SetSessionPlugin 热切换（pluginMu 保护）。
	pluginMu sync.RWMutex
	plugin   string
	// reresolve 用于外部（SetSessionPlugin）通知 run 循环立即重解析解码器（buffer 1，非阻塞发送）。
	reresolve chan struct{}

	// 依赖（注入，不持有所有权）
	registry *plugin.RegistryServer
	// owner 是会话发起者的属主（来自 StartSession RPC 的 auth 上下文；
	// 未接入认证时为空串 = 匿名/本地语义），用于 owner 作用域的插件路由。
	owner  string
	rules  []*analyze.CompiledRule
	logger *slog.Logger // 带 session_id 等上下文字段的 logger

	// Protocol Behavior Resolver（可选）：protocolCfg 非空时在 Start 时构建。
	protocolCfg      *protocolconfig.File
	protocolResolver *protocolresolver.ProtocolResolver
	corrStore        *protocolcorrelation.Store

	// 生命周期（atomic，无锁）
	state atomic.Int32 // capture.State 的 int32 值

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// 统计快照（atomic.Value，无锁读取）
	statsSnap atomic.Value // taskStats

	// 资源（session 级，run 退出时关闭）
	sqliteStore *store.SQLiteStore
	// source 不放 struct——是 run 的 local variable

	// finalize 回调：run 退出时通知 pipelineService（写 ControlStore + removeTask）
	onFinalize func(task *captureTask)
}

// taskStats 是 captureTask 的统计快照，用 atomic.Value 无锁读取。
type taskStats struct {
	RawCount     int64
	EventCount   int64
	MetricCount  int64
	DecodeErrors int64
	DecodeDrops  int64 // 待解码溢出队列达到字节上限时丢最旧的包数（raw 已持久化，可离线补救）
	// OverflowDrops 是写缓冲在 sqlite 持续写入失败、达到保留上限后被迫丢最旧的条数。
	// 非空即表示发生过数据丢失，必须暴露给上层（旧实现静默丢弃且无任何痕迹）。
	OverflowDrops int64
	// PendingDecode 是当前待解码溢出队列的积压包数（>0 表示解码侧落后于抓包）。
	PendingDecode int64
	PacketsIn     uint64
	PacketsOut   uint64
	BytesIn      uint64
	BytesOut     uint64
	Drops        uint64
	Errors       uint64
	Err          string
}

// 解码流水线参数：capture 与解码解耦的核心配置。
const (
	// decodeQueueSize 是 capture→解码 有界队列容量；实时模式满时丢包保抓包。
	decodeQueueSize = 1024
	// decodedQueueSize 是 解码→后处理 有界队列容量。
	decodedQueueSize = 64
	// decodeWindow 是同时在途的解码请求数（流水线深度）。
	decodeWindow = 16
	// decodeWaitTimeout 是单个解码 Future 的最长等待（防插件挂死阻塞排空）。
	decodeWaitTimeout = 30 * time.Second
	// reconnectBackoff 是断流重连的最小间隔，避免重连失败进入紧循环。
	reconnectBackoff = 5 * time.Second
	// 无解码器 / 解码队列满时待解码包进溢出队列（见 pending_decode.go），
	// 容量按字节计、FIFO 回灌，不再有 2048 条的硬上限。
)

// 写缓冲的保留上限（条数）。
//
// sqlite 写入失败时缓冲必须保留等下轮重试（否则数据永久丢失），
// 但若持续失败（磁盘满 / 库损坏）缓冲会无界增长直至 OOM，
// 故设上限：超过时丢最旧的并计入 OverflowDrops。
// 取值按「单条体积 × 上限 ≈ 可容忍的内存占用」估算。
const (
	rawBufMax        = 32768 // ≈ 50MB @1600B/帧
	eventBufMax      = 65536
	stateChangeMax   = 131072 // 投影条目小（每事件 0..N 条）
	metricBufMax     = 65536
)

// retainOnFailure 在写入失败时保留缓冲等下轮重试；超过 max 时丢最旧的并计数。
// 旧的写法是无条件 buf[:0]，一次写入抖动就永久丢掉整个缓冲窗口的数据。
func retainOnFailure[T any](buf []T, max int, dropped *atomic.Int64) []T {
	if len(buf) <= max {
		return buf
	}
	n := len(buf) - max
	copy(buf, buf[n:])
	for i := max; i < len(buf); i++ {
		buf[i] = *new(T) // 释放引用，便于 GC
	}
	dropped.Add(int64(n))
	return buf[:max]
}

// Start CAS Created→Running，只允许一次。重复调用返回 ErrAlreadyStarted。
// 非幂等：成功后启动 run goroutine。
func (t *captureTask) Start() error {
	if !t.state.CompareAndSwap(int32(capture.StateCreated), int32(capture.StateRunning)) {
		return internalipc.ErrAlreadyStarted
	}
	if t.protocolCfg != nil {
		r, err := protocolresolver.New(t.protocolCfg)
		if err != nil {
			t.state.Store(int32(capture.StateClosed))
			return fmt.Errorf("build protocol resolver: %w", err)
		}
		t.protocolResolver = r
		t.corrStore = protocolcorrelation.New(0)
		t.logger.Info("protocol resolver enabled")
	}
	go t.run()
	return nil
}

// Stop cancel ctx + 等 done，支持 ctx 超时防止 source 卡死导致永久阻塞。
// 返回最终统计快照。不写 ControlStore，不 removeTask——由 finalizeTask 统一处理。
func (t *captureTask) Stop(ctx context.Context) (taskStats, error) {
	t.cancel()
	select {
	case <-t.done:
		return t.Snapshot(), nil
	case <-ctx.Done():
		return taskStats{}, ctx.Err()
	}
}

// State atomic 读，无锁。
func (t *captureTask) State() capture.State {
	return capture.State(t.state.Load())
}

// Snapshot atomic 读 statsSnap，无锁。
func (t *captureTask) Snapshot() taskStats {
	if v := t.statsSnap.Load(); v != nil {
		return v.(taskStats)
	}
	return taskStats{}
}

// run 是抓包主循环。
//
// defer 执行顺序（LIFO）：
//  1. close(t.done)           — 注册最早，最后执行（确保 finalize 已完成）
//  2. finalize func            — flush final + state→Closed + close sqliteStore + onFinalize
//  3. dispatcher.Close()       — 在 finalize 之前关闭
//  4. closeCaptureSources      — 在 dispatcher 之前关闭
//  5. tick.Stop()              — 最先执行
func (t *captureTask) run() {
	defer close(t.done) // 最后执行，确保 finalize 已完成

	// 声明 flush 闭包捕获的局部变量（source 是 local variable，不放 struct）
	// engine 需提前声明（flush 闭包引用），稍后在 dispatcher 创建后赋值。
	// 早退路径（无 tcp 插件/dispatcher 错误）下 engine 为 nil，flush 需 nil guard。
	var (
		source      capture.Source
		engine      *analyze.Engine
		baseline    *state.BaselineManager
		raws        []event.Packet
		events      []*event.Event
		enrichedSCs []store.EnrichedStateChange
		metrics     []event.Metric
		rawCount    int64
		eventCount  int64
		metricCount int64

		// decodeErrs 由 decode worker goroutine 并发递增，用原子计数。
		decodeErrs atomic.Int64

		// pq 是待解码溢出队列（无解码器 / decodeCh 满时暂存），由主循环独占。
		pq *pendingQueue

		// 写缓冲达到保留上限后被迫丢最旧的计数（持续写失败时的兜底，必须可见）。
		rawOverflow         atomic.Int64
		eventOverflow       atomic.Int64
		stateChangeOverflow atomic.Int64
		metricOverflow      atomic.Int64
	)

	// flush 是闭包，捕获 source 和局部计数器，每次调用后更新 t.statsSnap。
	flush := func(fctx context.Context, final bool) {
		if engine != nil {
			if final {
				metrics = append(metrics, engine.FlushAll()...)
			} else {
				metrics = append(metrics, engine.Flush()...)
			}
		}
	if len(raws) > 0 {
		if err := t.sqliteStore.AppendRawPackets(fctx, raws); err != nil {
			// 写入失败必须保留缓冲等下轮重试：旧写法失败也清空，
			// 一次磁盘抖动就永久丢掉整个窗口的原始包。
			t.logger.Error("write raw failed, retaining buffer for retry", "error", err, "buffered", len(raws))
			raws = retainOnFailure(raws, rawBufMax, &rawOverflow)
		} else {
			t.logger.Debug("flushed raw packets", "count", len(raws))
			rawCount += int64(len(raws))
			raws = raws[:0]
		}
	}
	if len(events) > 0 {
		if err := t.sqliteStore.AppendEvents(fctx, events); err != nil {
			// 同上：事件写失败不清空（清空会导致解码结果永久丢失）。
			// enrichedSCs 也必须一起保留，否则重试时会与 events 错位。
			t.logger.Error("write events failed, retaining buffer for retry", "error", err, "buffered", len(events))
			events = retainOnFailure(events, eventBufMax, &eventOverflow)
		} else {
			t.logger.Debug("flushed decoded events", "count", len(events))
			eventCount += int64(len(events))
			// 事件写入成功后，再写入语义增强后的状态变更投影
			if len(enrichedSCs) > 0 {
				if err := t.sqliteStore.WriteEnrichedStateChanges(fctx, t.sessionID, enrichedSCs); err != nil {
					// 同上：投影写失败也要保留重试，不能清空。
					t.logger.Error("write enriched state changes failed, retaining buffer for retry",
						"error", err, "buffered", len(enrichedSCs))
					enrichedSCs = retainOnFailure(enrichedSCs, stateChangeMax, &stateChangeOverflow)
				} else {
					enrichedSCs = enrichedSCs[:0]
				}
			}
			events = events[:0]
		}
	}
	if len(metrics) > 0 {
		if err := t.sqliteStore.WriteMetrics(fctx, metrics); err != nil {
			t.logger.Error("write metrics failed, retaining buffer for retry", "error", err, "buffered", len(metrics))
			metrics = retainOnFailure(metrics, metricBufMax, &metricOverflow)
		} else {
			t.logger.Debug("flushed metrics", "count", len(metrics))
			metricCount += int64(len(metrics))
			metrics = metrics[:0]
		}
	}

	// 更新统计快照（含 source.Stats()）
	snap := taskStats{
		RawCount:     rawCount,
		EventCount:   eventCount,
		MetricCount:  metricCount,
		DecodeErrors: decodeErrs.Load(),
		// 写缓冲达到上限被迫丢最旧的数量（持续写失败的唯一可见证据）。
		OverflowDrops: rawOverflow.Load() + eventOverflow.Load() +
			stateChangeOverflow.Load() + metricOverflow.Load(),
	}
	if pq != nil {
		snap.PendingDecode = int64(pq.len())
		// 溢出队列达到字节上限被迫丢最旧的包数（raw 已落库，可离线 decode_raw 补救）。
		snap.DecodeDrops = pq.Dropped()
	}
		if source != nil {
			st := source.Stats()
			snap.PacketsIn = st.PacketsIn
			snap.PacketsOut = st.PacketsOut
			snap.BytesIn = st.BytesIn
			snap.BytesOut = st.BytesOut
			snap.Drops = st.Drops
			snap.Errors = st.Errors
			if err := source.Err(); err != nil {
				snap.Err = err.Error()
			}
		}
		t.statsSnap.Store(snap)
	}

	// finalize defer — 在 close(done) 之前执行
	defer func() {
		fctx, fcancel := context.WithTimeout(context.Background(), 10*time.Second)
		flush(fctx, true)
		fcancel()

		t.state.Store(int32(capture.StateClosed))

		if t.sqliteStore != nil {
			if err := t.sqliteStore.Close(); err != nil {
				t.logger.Error("close sqlite store on run exit", "error", err)
			}
		}

		if t.onFinalize != nil {
			t.onFinalize(t)
		}
	}()

	t.logger.Info("capture session run starting",
		"port", t.port, "plugin", t.getPlugin(), "interface", t.iface, "pcap_file", t.pcapFile)

	// 解码插件是可选的——缺失时跳过 decode，但仍抓包并持久化 raw packets。
	// 抓包（raw capture）不与解码插件强耦合。
	//
	// 解码器支持热加载：resolveDecoder 在抓包循环中按需（节流）从 registry 重新解析，
	// 因此运行中注册 / 注销 / 重启的插件会被立即感知，无需停止再重启 capture。
	//
	// 路由规则（GAP 2 修复）：
	//   - 若本次会话指定了插件名（t.plugin != ""），优先按名精确路由 FindByName，
	//     使 A 项目会话只认插件 A、B 项目会话只认插件 B，多项目并行各用各插件；
	//     名查不到时退化按协议 hint（Find）兼容老用法。
	//   - 未指定插件名时，沿用原行为：按 tcp 协议 hint 取第一个在线插件。
	var decoderClient pb.DecoderClient // 当前 dispatcher 绑定的 client 指针，用于检测插件是否变化
	var lastResolve time.Time
	// lastReconnect 记录上次断流重连时间，用于退避避免紧循环。
	var lastReconnect time.Time

	// —— 解码流水线：capture 主循环与解码解耦 ——
	//
	// 主循环（本 goroutine）只负责：读包 → raw 缓冲 → 投递 decodeCh；
	// 并消费 decodedCh 做协议富化 / 状态投影 / 规则聚合。
	// 后处理只在主循环 goroutine 执行，写缓冲无并发访问，事件全局保序。
	//
	// decode worker（独立 goroutine）消费 decodeCh，经 Dispatcher.Submit 以
	// decodeWindow 个在途请求流水线化解码，按提交顺序 Wait 并把事件发 decodedCh，
	// 单包解码 RTT 不再阻塞抓包。
	//
	// disp 是当前解码器的原子指针：主循环（build/drop 时写）与 worker（每包读）并发访问。
	var disp atomic.Pointer[decode.Dispatcher]
	// pq 缓存送不进解码流水线的 tcp 包：无解码器期间 + decodeCh 满时。
	// 解码器接入 / 队列腾出空间后由主循环按 FIFO 回灌，保证解码顺序 == 抓包顺序。
	pq = newPendingQueue(pendingQueueMaxBytes)

	decodeCh := make(chan event.Packet, decodeQueueSize)
	decodedCh := make(chan []*event.Event, decodedQueueSize)

	// drainPending 把溢出队列里能送的包回灌进 decodeCh（非阻塞）。
	// 解码器刚接入、或 decodeCh 腾出空间后调用；队列空 / 队列满 / 无解码器时立即返回。
	drainPending := func() {
		for pq.len() > 0 && disp.Load() != nil {
			select {
			case decodeCh <- pq.head():
				pq.popHead()
			default:
				return // decodeCh 仍满，留给下一轮
			}
		}
	}

	// feedDecode 把一个包送入解码流水线，保证全局 FIFO 顺序。
	// 送不进去（无解码器 / 解码队列满）时进溢出队列，绝不丢包。
	feedDecode := func(pkt event.Packet) {
		drainPending()
		// 仍有积压或无解码器：新包必须排在队尾，否则会插队乱序。
		if disp.Load() == nil || pq.len() > 0 {
			pq.push(pkt)
			return
		}
		select {
		case decodeCh <- pkt:
		default:
			pq.push(pkt)
		}
	}

	// processDecoded 把一批解码事件做协议富化 / 状态投影 / 规则聚合，追加到写缓冲。
	processDecoded := func(evs []*event.Event) {
		for _, ev := range evs {
			if ev == nil {
				continue
			}
			t.logger.Debug("decoded packet v2", "event_id", ev.Identity.ID, "event_type", ev.Identity.Type, "session", ev.Identity.SessionID)
			// Protocol Behavior Resolver：把 JSON 解释为通信语义（identity/role/correlation/delivery/error）。
			t.enrichProtocol(ev)
			events = append(events, ev)

			// State 层投影：从 _state_changes 提取并做 before/after 基线富化
			scChanges, err := baseline.Apply(ev, t.sessionID)
			if err != nil {
				t.logger.Error("state projection", "event_id", ev.Identity.ID, "error", err)
			} else {
				enrichedSCs = append(enrichedSCs, scChanges...)
			}

			ms, err := engine.Process(t.ctx, ev)
			if err != nil {
				t.logger.Error("analyze event", "event_id", ev.Identity.ID, "error", err)
				continue
			}
			metrics = append(metrics, ms...)
		}
	}

	// drainPendingBlocking 在停机路径上限时排空溢出队列，尽量把已抓到的包送完。
	// 期间继续消费 decodedCh，避免与 decode worker 相互阻塞造成死锁。
	drainPendingBlocking := func(limit time.Duration) {
		deadline := time.Now().Add(limit)
		for pq.len() > 0 && disp.Load() != nil && time.Now().Before(deadline) {
			select {
			case decodeCh <- pq.head():
				pq.popHead()
			case evs := <-decodedCh:
				processDecoded(evs)
			case <-t.ctx.Done():
				t.logger.Warn("pending decode replay interrupted by shutdown", "remaining", pq.len())
				return
			}
		}
	}

	// decode worker：流水线化解码（decodeWindow 个在途），按提交顺序交付事件。
	go func() {
		defer close(decodedCh)
		var window []*decode.Future

		// 解码失败可能因插件重启：通知主循环立即重解析
		// （等价旧同步循环中出错路径的 resolveDecoder(true)）。
		signalReresolve := func() {
			select {
			case t.reresolve <- struct{}{}:
			default:
			}
		}
		drainHead := func() {
			f := window[0]
			wctx, wcancel := context.WithTimeout(context.Background(), decodeWaitTimeout)
			evs, err := f.Wait(wctx)
			wcancel()
			window = window[1:]
			if err != nil {
				t.logger.Debug("decode failed", "error", err)
				decodeErrs.Add(1)
				signalReresolve()
				return
			}
			if len(evs) == 0 {
				return
			}
			decodedCh <- evs
		}

		for pkt := range decodeCh {
			d := disp.Load()
			if d == nil {
				continue // 解码器已下线：raw 已入缓冲持久化，仅跳过解码
			}
			f, err := d.Submit(pkt)
			if err != nil {
				t.logger.Debug("decode submit failed", "error", err)
				decodeErrs.Add(1)
				signalReresolve()
				continue
			}
			window = append(window, f)
			for len(window) >= decodeWindow {
				drainHead()
			}
		}
		// decodeCh 关闭（shutdown）：按序排空在途窗口后退出。
		for len(window) > 0 {
			drainHead()
		}
	}()

	// shutdownDecode 停止解码流水线并排空剩余事件（主循环各退出路径恰好调用一次）。
	shutdownDecode := func() {
		close(decodeCh)
		for evs := range decodedCh {
			processDecoded(evs)
		}
	}

	// 订阅插件注册表事件（注册/注销/上下线），变化时立即重解析解码器（0 延迟）。
	// 配合 SetSessionPlugin 的 reresolve 通道，使解码侧无需等待 3s 节流或 1s tick。
	evtCh, evtUnsub := t.registry.Subscribe()
	defer evtUnsub()

	resolveDecoder := func(force bool) {
		now := time.Now()
		if !force && disp.Load() != nil && now.Sub(lastResolve) < 3*time.Second {
			return // 已有解码器且未到节流周期，跳过
		}
		lastResolve = now

		// 断流检测：当前 dispatcher 的 recvLoop 已退出（流错误 / 对端关闭），
		// 但插件注册信息未变（decoderClient 相同），decoderAction 会返回 "keep" 不重建。
		// 此时必须关闭旧 dispatcher 并清空 decoderClient，强制下次重建。
		if d := disp.Load(); d != nil && !d.IsHealthy() {
			// 退避：避免断流后立即重连失败进入紧循环（首次 1s，上限 5s）。
			if now.Sub(lastReconnect) < reconnectBackoff {
				return
			}
			lastReconnect = now
			t.logger.Warn("decoder stream broken, force reconnect", "plugin", t.getPlugin())
			_ = d.Close()
			disp.Store(nil)
			decoderClient = nil
		}

		found, sr, ok := t.resolveDecoderClient()
		if !ok {
			found = nil
		}
		switch decoderAction(found, decoderClient, disp.Load() != nil) {
		case "idle":
			t.logger.Debug("no decoder plugin available yet, will retry", "plugin", t.getPlugin())
			return
		case "drop":
			t.logger.Warn("decoder plugin went offline, dropping decoder; capture will store raw packets only", "plugin", t.getPlugin())
			if d := disp.Swap(nil); d != nil {
				_ = d.Close()
			}
			decoderClient = nil
			return
		case "keep":
			return
		case "build":
			// 插件变化（新注册 / 重启 / 替换）：关闭旧 dispatcher，建立新流。
			if d := disp.Swap(nil); d != nil {
				_ = d.Close()
			}
			d, err := decode.NewDispatcher(found, t.sessionID, t.logger, sr, decode.WithServerPort(t.port))
			if err != nil {
				t.logger.Warn("open dispatcher stream failed, decode disabled", "error", err, "plugin", t.getPlugin())
				decoderClient = nil
				return
			}
			disp.Store(d)
			decoderClient = found
			t.logger.Info("decoder attached via hot-reload", "plugin", t.getPlugin())
			// Semantic Contract v1 §13：规则聚合字段 ↔ manifest aggregatable/groupable 对齐（仅告警）。
			t.checkAggregationContract(found)

			// 补解码：解码器刚接入，把积压（无解码器期间 / 解码队列满时缓存）
			// 的包按序回灌。这里只做一次非阻塞回灌，后续由主循环每包/每 tick
			// 继续排空——避免在此处长时间阻塞导致抓包主循环停摆。
			if n := pq.len(); n > 0 {
				drainPending()
				t.logger.Info("decoder attached, replaying pending packets",
					"plugin", t.getPlugin(), "pending", n, "remaining", pq.len())
			}
		}
	}
	engine = analyze.NewEngine(t.rules, t.logger)
	baseline = state.NewBaselineManager(nil)

	sources, err := openCaptureSources(t.ctx, t.iface, t.port, t.pcapFile, t.liveCfg, t.mobileCfg, t.agentHub, t.sessionID, t.agentOnly)
	if err != nil {
		t.logger.Error("capture init failed", "error", err, "interface", t.iface, "port", t.port, "pcap_file", t.pcapFile)
		return
	}
	defer closeCaptureSources(sources)
	if len(sources) > 0 {
		source = sources[0]
	}
	pktCh := mergePacketSources(t.ctx, sources)
	if t.pcapFile != "" {
		t.logger.Info("pcap replay started", "file", t.pcapFile)
	} else {
		t.logger.Info("live capture started", "interface", t.iface, "sources", len(sources))
	}

	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	for {
		select {
		case <-t.reresolve:
			// 外部（SetSessionPlugin）请求立即重解析解码器，跳过节流。
			resolveDecoder(true)
		case evt := <-evtCh:
			// 插件注册表状态变化（注册/注销/上线/下线）：立即重解析解码器，跳过节流。
			// 解码器指针未变时 decoderAction 返回 keep，无副作用；变化时才 build/drop。
			t.logger.Debug("plugin registry event, re-resolving decoder", "type", evt.Type, "plugin", t.getPlugin())
			resolveDecoder(true)
			drainPending()
		case <-t.ctx.Done():
			drainPendingBlocking(pendingDrainTimeout)
			shutdownDecode()
			fctx, fcancel := context.WithTimeout(context.Background(), 10*time.Second)
			flush(fctx, true)
			fcancel()
			t.logger.Info("capture stopped", "raw", rawCount, "events", eventCount, "metrics", metricCount,
				"pending_decode", pq.len(), "decode_drops", pq.Dropped(),
				"overflow_drops", rawOverflow.Load()+eventOverflow.Load()+stateChangeOverflow.Load()+metricOverflow.Load())
			return
		case <-tick.C:
			resolveDecoder(false)
			// 低流量 / 无新包时也要推进回灌，否则解码器接入后积压一直躺着。
			drainPending()
			flush(t.ctx, false)
		case evs := <-decodedCh:
			// 解码完成的事件：协议富化 / 状态投影 / 规则聚合（主循环执行，事件保序）。
			processDecoded(evs)
		case pkt, ok := <-pktCh:
			if !ok {
				t.logger.Info("packet source closed, flushing remaining data")
				drainPendingBlocking(pendingDrainTimeout)
				shutdownDecode()
				fctx, fcancel := context.WithTimeout(context.Background(), 10*time.Second)
				flush(fctx, true)
				fcancel()
				return
			}
			// 在入缓冲与解码前为原始包分配稳定 ID，确保 raw_packet_id 能定位到 raw_packets 表的真实记录。
		if pkt.ID == "" {
			pkt.ID = string(event.NewEventID())
		}
		// 派生 conn_id：移动代理在 Metadata 携带真实 conn_id（优先保留）；
		// agent / 本地网卡抓包没有，按规范五元组（双向排序）派生，使
		// raw_packets 落库与解码事件上下文都带连接标识——Connections 页面
		// 依赖 raw_packets.conn_id 聚合，缺失时整个页面为空。
		deriveConnID(&pkt)
			raws = append(raws, pkt)
			resolveDecoder(false)
			if pkt.Protocol != "tcp" {
				t.logger.Debug("skipped non-tcp packet", "protocol", pkt.Protocol)
				continue
			}
			if t.pcapFile != "" {
				// pcap 回放：无实时约束，解码队列满时背压等待（不丢包）。
				// 溢出队列非空时优先回灌旧包（FIFO），再送新包；
				// 等待期间继续消费 decodedCh，避免与 decode worker 相互阻塞。
				sent := false
				for !sent {
					next := pq.headOr(pkt)
					select {
					case decodeCh <- next:
						if pq.len() > 0 {
							pq.popHead()
						} else {
							sent = true
						}
					case evs := <-decodedCh:
						processDecoded(evs)
					case <-t.ctx.Done():
						sent = true
					}
				}
				continue
			}
			// 实时抓包：解码队列满 / 无解码器时进溢出队列等回灌，不再丢包
			//（旧实现在队列满时直接 decodeDropN++ 丢弃解码任务）。
			feedDecode(pkt)
		}
	}
}

// nowSessionID 生成基于时间戳的会话 ID（毫秒时间戳 + 4 位随机数，避免同毫秒内碰撞）。
func nowSessionID() string {
	return fmt.Sprintf("%s_%04d", time.Now().Format("20060102_150405.000"), rand.IntN(10000))
}

// getPlugin 读锁返回当前绑定的解码插件名。
// 插件绑定在运行中可被 SetSessionPlugin 修改，因此所有读取都应经由此方法。
func (t *captureTask) getPlugin() string {
	t.pluginMu.RLock()
	defer t.pluginMu.RUnlock()
	return t.plugin
}

// SetSessionPlugin 运行中热切换解码插件绑定，并立即触发 run 循环重解析解码器。
// 返回切换后的插件名；会话未运行或已结束时返回 ErrNoActiveCapture。
func (t *captureTask) SetSessionPlugin(ctx context.Context, plugin string) (string, error) {
	if t.State() != capture.StateRunning {
		return "", internalipc.ErrNoActiveCapture
	}
	t.pluginMu.Lock()
	t.plugin = plugin
	t.pluginMu.Unlock()

	// 非阻塞通知 run 循环立即强制重解析（force=true 跳过节流）。
	// 若已有未消费信号则丢弃——下次循环天然也会触发。
	select {
	case t.reresolve <- struct{}{}:
	default:
	}
	return plugin, nil
}

// resolveDecoderClient 解析本次会话应使用的 DecoderClient 与 schema registry。
// 路由规则（GAP 2 修复）：
//   - 指定插件名（t.plugin != ""）：优先 FindByNameFor 精确路由（owner 作用域，
//     t.owner 为空时与旧 FindByName 行为一致），名查不到时退化按协议 hint（FindFor），
//     使 A 项目会话绑 A 插件、B 项目会话绑 B 插件，多项目并行互不干扰。
//   - 未指定插件名：沿用原行为，按 "tcp" 协议 hint 取第一个在线插件（兼容默认抓包场景）。
//
// 解析顺序（名精确 → 协议退化）保持不变。
func (t *captureTask) resolveDecoderClient() (pb.DecoderClient, *schema.Registry, bool) {
	plugin := t.getPlugin()
	if plugin != "" {
		if c, sr, ok := t.registry.FindByNameFor(t.owner, plugin); ok {
			return c, sr, true
		}
		if c, sr, ok := t.registry.FindFor(t.owner, plugin); ok {
			// 退化兼容：把 plugin 字段当作协议 hint
			return c, sr, true
		}
		return nil, nil, false
	}
	return t.registry.FindFor(t.owner, "tcp")
}

// deriveConnID 为无连接标识的包派生 conn_id（幂等：已有则保留）。
// 移动代理在 Packet.Metadata 携带真实 conn_id；agent / 本地网卡抓包没有，
// 按 TCP 五元组双向排序生成规范键（同一连接两个方向得到相同 conn_id），
// 写入 Metadata["conn_id"] 供 AppendRawPackets 落库与解码事件上下文使用。
func deriveConnID(pkt *event.Packet) {
	if pkt.Protocol != "tcp" {
		return
	}
	if c, ok := pkt.Metadata["conn_id"].(string); ok && c != "" {
		return
	}
	if !pkt.Src.IsValid() || !pkt.Dst.IsValid() {
		return
	}
	a, b := pkt.Src.String(), pkt.Dst.String()
	if b < a {
		a, b = b, a
	}
	if pkt.Metadata == nil {
		pkt.Metadata = map[string]any{}
	}
	pkt.Metadata["conn_id"] = "tcp:" + a + "<->" + b
}

// decoderAction 给定一次 registry.Find 的结果与当前 dispatcher 状态，
// 决定解码器热加载动作。抽成纯函数便于单测热加载策略。
//
//	client == nil（Find 未命中）：已有 dispatcher → "drop"（插件下线）；否则 "idle"
//	client 与当前 decoderClient 相同      → "keep"（无需重建）
//	client 与当前 decoderClient 不同      → "build"（插件新注册 / 重启 / 替换）
func decoderAction(client, current pb.DecoderClient, haveDispatcher bool) string {
	if client == nil {
		if haveDispatcher {
			return "drop"
		}
		return "idle"
	}
	if client == current {
		return "keep"
	}
	return "build"
}

// openCaptureSources 打开基础 source，并在 agentHub 非 nil 时追加 agent source。
// agent source 消费 AgentIngest server 按 session_id 路由的 gta-agent 推送，
// 与其它 source（live/mobile/pcap-file）并行 merge。agentOnly 为 true 时跳过
// 基础 source，仅打开 agent source（hub 未配置时报错）。
func openCaptureSources(ctx context.Context, iface string, port int, pcapFile string, live *capturecontrol.LiveConfig, mcfg *capturecontrol.MobileConfig, agentHub *agent.Hub, sessionID string, agentOnly bool) ([]capture.Source, error) {
	var sources []capture.Source
	if !agentOnly {
		var err error
		sources, err = openCaptureSourcesBase(ctx, iface, port, pcapFile, live, mcfg)
		if err != nil {
			return nil, err
		}
	}
	if agentHub != nil {
		src, err := capture.Open(ctx, agent.SourceName, agent.Config{
			Hub:       agentHub,
			SessionID: sessionID,
		})
		if err != nil {
			return nil, err
		}
		sources = append(sources, src)
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("no capture source available: agent source requires agent ingest to be configured (pipeline -agent-ingest-addr)")
	}
	return sources, nil
}

// openCaptureSourcesBase 根据配置打开一个或多个 capture source。
// 若 pcapFile 非空则回放文件；若 live.Device 非空则打开指定网卡；
// 若 mobile 非空则启动移动代理抓包源（gRPC server，等待 gta-singbox-agent 推送）；
// 否则打开所有可用网卡。live 中的 BPF/SnapLen/Promisc 透传给 pcap-live。
// （agent source 的追加不在这里，见 openCaptureSources 包装层。）
func openCaptureSourcesBase(ctx context.Context, iface string, port int, pcapFile string, live *capturecontrol.LiveConfig, mcfg *capturecontrol.MobileConfig) ([]capture.Source, error) {
	if pcapFile != "" {
		src, err := capture.Open(ctx, "pcap-file", pcapfile.PcapFileConfig{Path: pcapFile})
		if err != nil {
			return nil, err
		}
		return []capture.Source{src}, nil
	}

	if mcfg != nil {
		src, err := capture.Open(ctx, "mobile", mobile.MobileConfig{
			ListenAddr: mcfg.ListenAddr,
			Activity:   mcfg.Activity,
		})
		if err != nil {
			return nil, err
		}
		return []capture.Source{src}, nil
	}

	var bpf string
	var snapLen int32
	var promisc bool
	if live != nil {
		bpf = live.BPF
		snapLen = live.SnapLen
		promisc = live.Promisc
		if live.Device != "" {
			src, err := openLiveSource(ctx, live.Device, port, bpf, snapLen, promisc)
			if err != nil {
				return nil, err
			}
			return []capture.Source{src}, nil
		}
	}

	if iface != "" {
		src, err := openLiveSource(ctx, iface, port, bpf, snapLen, promisc)
		if err != nil {
			return nil, err
		}
		return []capture.Source{src}, nil
	}

	// 实时抓包能力（网卡枚举 + pcaplive source）按 -tags pcap 门控，
	// 见 pcap_live_pcap.go / pcap_live_nopcap.go。
	ifaces, err := listInterfaces()
	if err != nil {
		return nil, err
	}
	var sources []capture.Source
	for _, dev := range ifaces {
		src, err := openLiveSource(ctx, dev, port, bpf, snapLen, promisc)
		if err != nil {
			slog.Warn("skip capture interface", "name", dev, "error", err)
			continue
		}
		sources = append(sources, src)
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("no capture interfaces available")
	}
	return sources, nil
}

// closeCaptureSources 关闭所有 source，忽略错误。
func closeCaptureSources(sources []capture.Source) {
	for _, src := range sources {
		_ = src.Close()
	}
}

// mergePacketSources 把多个 capture source 的 packet channel 合并成一个。
// 当 ctx 取消或所有 source 关闭时，返回的 channel 也会关闭。
func mergePacketSources(ctx context.Context, sources []capture.Source) <-chan event.Packet {
	out := make(chan event.Packet)
	var wg sync.WaitGroup
	for _, src := range sources {
		wg.Add(1)
		go func(s capture.Source) {
			defer wg.Done()
			for pkt := range s.Packets() {
				select {
				case out <- pkt:
				case <-ctx.Done():
					return
				}
			}
		}(src)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}
