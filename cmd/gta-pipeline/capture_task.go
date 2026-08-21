package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/gopacket/pcap"

	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
	"gta/pkg/analyze"
	"gta/pkg/capture"
	"gta/pkg/capture/pcapfile"
	"gta/pkg/capture/pcaplive"
	"gta/pkg/decode"
	"gta/pkg/event"
	"gta/pkg/internalipc"
	"gta/pkg/internalipc/capturecontrol"
	"gta/pkg/plugin"
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
	start      time.Time

	// 解码插件绑定：创建时设定，运行中可经 SetSessionPlugin 热切换（pluginMu 保护）。
	pluginMu sync.RWMutex
	plugin   string
	// reresolve 用于外部（SetSessionPlugin）通知 run 循环立即重解析解码器（buffer 1，非阻塞发送）。
	reresolve chan struct{}

	// 依赖（注入，不持有所有权）
	registry *plugin.RegistryServer
	rules    []*analyze.CompiledRule
	logger   *slog.Logger // 带 session_id 等上下文字段的 logger

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
	PacketsIn    uint64
	PacketsOut   uint64
	BytesIn      uint64
	BytesOut     uint64
	Drops        uint64
	Errors       uint64
	Err          string
}

// Start CAS Created→Running，只允许一次。重复调用返回 ErrAlreadyStarted。
// 非幂等：成功后启动 run goroutine。
func (t *captureTask) Start() error {
	if !t.state.CompareAndSwap(int32(capture.StateCreated), int32(capture.StateRunning)) {
		return internalipc.ErrAlreadyStarted
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
		source       capture.Source
		engine       *analyze.Engine
		baseline     *state.BaselineManager
		raws         []event.Packet
		events       []*event.Event
		enrichedSCs  []store.EnrichedStateChange
		metrics      []event.Metric
		rawCount     int64
		eventCount   int64
		metricCount  int64
		decodeErrors int64
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
				t.logger.Error("write raw", "error", err)
			} else {
				t.logger.Debug("flushed raw packets", "count", len(raws))
				rawCount += int64(len(raws))
			}
			raws = raws[:0]
		}
		if len(events) > 0 {
			if err := t.sqliteStore.AppendEvents(fctx, events); err != nil {
				t.logger.Error("write events", "error", err)
			} else {
				t.logger.Debug("flushed decoded events", "count", len(events))
				eventCount += int64(len(events))
				// 事件写入成功后，再写入语义增强后的状态变更投影
				if len(enrichedSCs) > 0 {
					if err := t.sqliteStore.WriteEnrichedStateChanges(fctx, t.sessionID, enrichedSCs); err != nil {
						t.logger.Error("write enriched state changes", "error", err)
					}
					enrichedSCs = enrichedSCs[:0]
				}
			}
			events = events[:0]
		}
		if len(metrics) > 0 {
			if err := t.sqliteStore.WriteMetrics(fctx, metrics); err != nil {
				t.logger.Error("write metrics", "error", err)
			} else {
				t.logger.Debug("flushed metrics", "count", len(metrics))
				metricCount += int64(len(metrics))
			}
			metrics = metrics[:0]
		}

		// 更新统计快照（含 source.Stats()）
		snap := taskStats{
			RawCount:     rawCount,
			EventCount:   eventCount,
			MetricCount:  metricCount,
			DecodeErrors: decodeErrors,
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
	var dispatcher *decode.Dispatcher
	var decoderClient pb.DecoderClient // 当前 dispatcher 绑定的 client 指针，用于检测插件是否变化
	var lastResolve time.Time

	// 订阅插件注册表事件（注册/注销/上下线），变化时立即重解析解码器（0 延迟）。
	// 配合 SetSessionPlugin 的 reresolve 通道，使解码侧无需等待 3s 节流或 1s tick。
	evtCh, evtUnsub := t.registry.Subscribe()
	defer evtUnsub()

	resolveDecoder := func(force bool) {
		now := time.Now()
		if !force && dispatcher != nil && now.Sub(lastResolve) < 3*time.Second {
			return // 已有解码器且未到节流周期，跳过
		}
		lastResolve = now

		found, sr, ok := t.resolveDecoderClient()
		if !ok {
			found = nil
		}
		switch decoderAction(found, decoderClient, dispatcher != nil) {
		case "idle":
			t.logger.Debug("no decoder plugin available yet, will retry", "plugin", t.getPlugin())
			return
		case "drop":
			t.logger.Warn("decoder plugin went offline, dropping decoder; capture will store raw packets only", "plugin", t.getPlugin())
			dispatcher.Close()
			dispatcher = nil
			decoderClient = nil
			return
		case "keep":
			return
		case "build":
			// 插件变化（新注册 / 重启 / 替换）：关闭旧 dispatcher，建立新流。
			if dispatcher != nil {
				dispatcher.Close()
			}
			d, err := decode.NewDispatcher(found, t.sessionID, t.logger, sr, decode.WithServerPort(t.port))
			if err != nil {
				t.logger.Warn("open dispatcher stream failed, decode disabled", "error", err, "plugin", t.getPlugin())
				decoderClient = nil
				return
			}
			dispatcher = d
			decoderClient = found
			t.logger.Info("decoder attached via hot-reload", "plugin", t.getPlugin())
			// Semantic Contract v1 §13：规则聚合字段 ↔ manifest aggregatable/groupable 对齐（仅告警）。
			t.checkAggregationContract(found)
		}
	}
	engine = analyze.NewEngine(t.rules, t.logger)
	baseline = state.NewBaselineManager(nil)

	sources, err := openCaptureSources(t.ctx, t.iface, t.port, t.pcapFile, t.liveCfg)
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
		case <-t.ctx.Done():
			fctx, fcancel := context.WithTimeout(context.Background(), 10*time.Second)
			flush(fctx, true)
			fcancel()
			t.logger.Info("capture stopped", "raw", rawCount, "events", eventCount, "metrics", metricCount)
			return
		case <-tick.C:
			resolveDecoder(false)
			flush(t.ctx, false)
		case pkt, ok := <-pktCh:
			if !ok {
				t.logger.Info("packet source closed, flushing remaining data")
				fctx, fcancel := context.WithTimeout(context.Background(), 10*time.Second)
				flush(fctx, true)
				fcancel()
				return
			}
			// 在入缓冲与解码前为原始包分配稳定 ID，确保 raw_packet_id 能定位到 raw_packets 表的真实记录。
			if pkt.ID == "" {
				pkt.ID = string(event.NewEventID())
			}
			raws = append(raws, pkt)
			resolveDecoder(false)
			if dispatcher == nil {
				continue // 无解码器，只存 raw packets
			}
			if pkt.Protocol != "tcp" {
				t.logger.Debug("skipped non-tcp packet", "protocol", pkt.Protocol)
				continue
			}
			evs, err := dispatcher.DecodeV2(t.ctx, pkt)
			if err != nil {
				t.logger.Debug("decode failed", "error", err, "src", pkt.Src, "dst", pkt.Dst)
				decodeErrors++
				// 解码失败时强制重新解析解码器：若插件已重启并重新注册，立即切换，
				// 不必等待心跳超时（默认 30s）。
				resolveDecoder(true)
				continue
			}
			for _, ev := range evs {
				if ev == nil {
					continue
				}
				t.logger.Debug("decoded packet v2", "event_id", ev.Identity.ID, "event_type", ev.Identity.Type, "session", ev.Identity.SessionID)
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
	}
}

// nowSessionID 生成基于时间戳的会话 ID。
func nowSessionID() string {
	return time.Now().Format("20060102_150405.000")
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
//   - 指定插件名（t.plugin != ""）：优先 FindByName 精确路由，名查不到时退化按协议 hint（Find），
//     使 A 项目会话绑 A 插件、B 项目会话绑 B 插件，多项目并行互不干扰。
//   - 未指定插件名：沿用原行为，按 "tcp" 协议 hint 取第一个在线插件（兼容默认抓包场景）。
func (t *captureTask) resolveDecoderClient() (pb.DecoderClient, *schema.Registry, bool) {
	plugin := t.getPlugin()
	if plugin != "" {
		if c, sr, ok := t.registry.FindByName(plugin); ok {
			return c, sr, true
		}
		if c, sr, ok := t.registry.Find(plugin); ok {
			// 退化兼容：把 plugin 字段当作协议 hint
			return c, sr, true
		}
		return nil, nil, false
	}
	return t.registry.Find("tcp")
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

// openCaptureSources 根据配置打开一个或多个 capture source。
// 若 pcapFile 非空则回放文件；若 iface/live.Device 非空则打开指定网卡；
// 否则打开所有可用网卡。live 中的 BPF/SnapLen/Promisc 透传给 pcap-live。
func openCaptureSources(ctx context.Context, iface string, port int, pcapFile string, live *capturecontrol.LiveConfig) ([]capture.Source, error) {
	if pcapFile != "" {
		src, err := capture.Open(ctx, "pcap-file", pcapfile.PcapFileConfig{Path: pcapFile})
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

	devs, err := pcap.FindAllDevs()
	if err != nil {
		return nil, fmt.Errorf("list interfaces: %w", err)
	}
	var sources []capture.Source
	for _, dev := range devs {
		src, err := openLiveSource(ctx, dev.Name, port, bpf, snapLen, promisc)
		if err != nil {
			slog.Warn("skip capture interface", "name", dev.Name, "error", err)
			continue
		}
		sources = append(sources, src)
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("no capture interfaces available")
	}
	return sources, nil
}

// openLiveSource 打开实时网卡抓包 source。
// bpf 为空时默认 "tcp port <port>"；snapLen 为 0 时默认 1600。
func openLiveSource(ctx context.Context, iface string, port int, bpf string, snapLen int32, promisc bool) (capture.Source, error) {
	if bpf == "" {
		bpf = fmt.Sprintf("tcp port %d", port)
	}
	if snapLen == 0 {
		snapLen = 1600
	}
	return capture.Open(ctx, "pcap-live", pcaplive.PcapLiveConfig{
		Device:  iface,
		BPF:     bpf,
		SnapLen: snapLen,
		Promisc: promisc,
	})
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
