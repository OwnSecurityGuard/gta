package main

// archive.go 是探针侧的本地归档器（docs/plans/2026-09-05 §8）：
//
//   - 留存：captureRunner 打开的 spool 以归档模式运行（Ack 不删段），
//     按保留窗口（MaxAge/MaxBytes）由 EnforceRetention 清理，绝不删未确认数据；
//   - 查询：归档跨会话目录（spool base 下每个子目录是一个历史会话），
//     按时间窗列出段清单（远端 archive_query 指令 / 本地接口共用）；
//   - 回放：按段 ReadSegment 保留原始时间戳重放，经 AgentIngest.Push
//     推到平台指定的目标会话（离线导入，服务端落 source=probe-archive 新会话）。
//
// 设计取舍：留存元数据不额外建索引，直接复用 spool 的段元数据（idx 文件）；
// 跨目录扫描有 60s 缓存，避免 10s 心跳反复开目录。

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"gta/pkg/capture/agent"
	"gta/pkg/capture/agent/proto"
	"gta/pkg/spool"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// 归档默认参数（probe.json 的 archive 字段为 0 时取用）。
const (
	defaultArchiveMaxAgeHrs = 24
	defaultArchiveMaxBytes  = 4 << 30 // 4GB
	archiveRefreshInterval  = 60 * time.Second
	archiveEnforceInterval  = 5 * time.Minute
)

// archiver 管理本机全部会话 spool 目录的留存与查询回放。
type archiver struct {
	runner *captureRunner
	cfg    *agentConfig

	mu          sync.Mutex
	cached      []archiveSeg // 段清单缓存（refresh 时重建）
	cachedAt    time.Time
	uploadMu    sync.Mutex
	uploadInFly map[string]bool // targetSession -> true，防并发重放同一目标
}

// archiveSeg 是一个可回放段的定位信息。
type archiveSeg struct {
	Dir  string           // 所属 spool 会话目录
	Info spool.SegmentInfo
}

func newArchiver(runner *captureRunner, cfg *agentConfig) *archiver {
	return &archiver{runner: runner, cfg: cfg}
}

// retentionFrom 把 probe.json 的 archive 配置转成 spool 留存策略。
// 未启用返回 nil（恢复发后即焚语义）。
func retentionFrom(cfg *agentConfig) *spool.Retention {
	if cfg == nil || !cfg.Archive.Enabled {
		return nil
	}
	hrs := cfg.Archive.MaxAgeHrs
	if hrs <= 0 {
		hrs = defaultArchiveMaxAgeHrs
	}
	bytes := cfg.Archive.MaxBytes
	if bytes <= 0 {
		bytes = defaultArchiveMaxBytes
	}
	return &spool.Retention{
		MaxAge:   time.Duration(hrs) * time.Hour,
		MaxBytes: bytes,
	}
}

// spoolBaseCustom 是 --spool-dir 覆盖的根目录（main 启动时设置一次，
// 早于任何 goroutine 读取，无需加锁）。
var spoolBaseCustom string

// spoolBase 返回 spool 根目录（每个子目录 = 一个会话的上行缓冲 + 留存）。
func spoolBase() string {
	if spoolBaseCustom != "" {
		return spoolBaseCustom
	}
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "gta-agent", "spool")
}

// ArchiveStatus 是留存观测快照（心跳 ProbeArchiveStatus 用，单位毫秒）。
type ArchiveStatus struct {
	Bytes    int64
	Segments int
	OldestMs int64 // 0 = 空
	NewestMs int64 // 0 = 空
}

// Status 返回全部留存数据的观测快照（读缓存；过期则先刷新）。
func (a *archiver) Status() ArchiveStatus {
	segs := a.snapshot()
	st := ArchiveStatus{Segments: len(segs)}
	for _, s := range segs {
		st.Bytes += int64(s.Info.Bytes)
		if st.OldestMs == 0 || s.Info.FirstMs < st.OldestMs {
			st.OldestMs = s.Info.FirstMs
		}
		if s.Info.LastMs > st.NewestMs {
			st.NewestMs = s.Info.LastMs
		}
	}
	return st
}

// ArchiveSegInfo 是查询应答里的段摘要（毫秒时间戳）。
type ArchiveSegInfo struct {
	SegID    string
	Dir      string
	FirstMs  int64
	LastMs   int64
	Packets  uint64
	Bytes    uint64
	LinkType uint32
}

// Segments 返回与 [fromMs, toMs]（0 = 不限）重叠的段，按时间升序。
func (a *archiver) Segments(_ context.Context, fromMs, toMs int64) ([]ArchiveSegInfo, error) {
	segs := a.snapshot()
	out := make([]ArchiveSegInfo, 0, len(segs))
	for _, s := range segs {
		if fromMs > 0 && s.Info.LastMs < fromMs {
			continue
		}
		if toMs > 0 && s.Info.FirstMs > toMs {
			continue
		}
		out = append(out, ArchiveSegInfo{
			SegID: s.Info.SegID, Dir: s.Dir,
			FirstMs: s.Info.FirstMs, LastMs: s.Info.LastMs,
			Packets: s.Info.Packets, Bytes: s.Info.Bytes, LinkType: s.Info.LinkType,
		})
	}
	return out, nil
}

// snapshot 返回段清单缓存（TTL 内复用；过期时同步刷新一次）。
func (a *archiver) snapshot() []archiveSeg {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cached != nil && time.Since(a.cachedAt) < archiveRefreshInterval {
		return a.cached
	}
	segs, err := a.scanAll()
	if err != nil {
		slog.Warn("archive scan failed (serving stale cache)", "error", err)
		if a.cached != nil {
			return a.cached
		}
		return nil
	}
	a.cached = segs
	a.cachedAt = time.Now()
	return a.cached
}

// invalidate 丢弃缓存（留存清理后调用，让下一次查询看到删除结果）。
func (a *archiver) invalidate() {
	a.mu.Lock()
	a.cached = nil
	a.mu.Unlock()
}

// scanAll 扫描 spool base 下全部会话目录，汇总各段摘要（时间升序）。
// 当前活跃队列直接用内存中的 Queue；历史目录临时只读打开（归档模式，
// recover 不会删读游标前的段）。
func (a *archiver) scanAll() ([]archiveSeg, error) {
	liveQ, liveDir := a.runner.Queue()
	base := spoolBase()
	ents, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	// 空策略：只为拿到归档模式语义（Open 不删段），不做任何清理。
	openArchive := &spool.Retention{}
	var out []archiveSeg
	var firstErr error
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(base, e.Name())
		if liveDir != "" && dir == liveDir && liveQ != nil {
			for _, info := range liveQ.Segments() {
				out = append(out, archiveSeg{Dir: dir, Info: info})
			}
			continue
		}
		q, err := spool.Open(dir, spool.Options{Retention: openArchive})
		if err != nil {
			slog.Warn("archive: open session spool failed, skipping", "dir", dir, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, info := range q.Segments() {
			out = append(out, archiveSeg{Dir: dir, Info: info})
		}
		_ = q.Close()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Info.FirstMs < out[j].Info.FirstMs })
	return out, firstErr
}

// Run 常驻：应用留存策略到活跃队列 + 周期清理（历史目录一起扫）+ 失效缓存。
func (a *archiver) Run(ctx context.Context) {
	a.applyRetention()
	ticker := time.NewTicker(archiveEnforceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.applyRetention()
			a.enforce()
			a.invalidate()
		}
	}
}

// applyRetention 把当前配置的留存策略同步到活跃队列（配置热变更的落地路径）。
func (a *archiver) applyRetention() {
	rt := retentionFrom(a.cfg)
	a.runner.setRetention(rt)
}

// enforce 对全部会话目录执行一次留存清理（MaxAge 各目录独立；MaxBytes
// 由 spool 队列在各自目录内执行——跨目录统一预算留给需要时再加）。
func (a *archiver) enforce() {
	rt := retentionFrom(a.cfg)
	if rt == nil {
		return // 归档关闭：无留存可言（活跃队列已被 setRetention 切回发后即焚）
	}
	now := time.Now()
	if q, _ := a.runner.Queue(); q != nil {
		if n, err := q.EnforceRetention(now); err != nil {
			slog.Warn("archive: enforce on live queue failed", "error", err)
		} else if n > 0 {
			slog.Info("archive: expired segments removed", "count", n, "scope", "live")
		}
	}
	base := spoolBase()
	ents, err := os.ReadDir(base)
	if err != nil {
		return
	}
	_, liveDir := a.runner.Queue()
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(base, e.Name())
		if liveDir != "" && dir == liveDir {
			continue // 活跃队列上面已处理
		}
		q, err := spool.Open(dir, spool.Options{Retention: rt})
		if err != nil {
			continue
		}
		if n, err := q.EnforceRetention(now); err != nil {
			slog.Warn("archive: enforce failed", "dir", dir, "error", err)
		} else if n > 0 {
			slog.Info("archive: expired segments removed", "count", n, "dir", dir)
		}
		_ = q.Close()
	}
}

// Upload 把 [fromMs, toMs]（0 = 不限）内的留存段按时间序回放到目标会话。
// 用探针凭证建 AgentIngest.Push 流（目标会话必须已指派给本探针，服务端校验）；
// 保留原始包 id 与时间戳（服务端 INSERT OR REPLACE 幂等）。异步执行由调用方负责。
func (a *archiver) Upload(ctx context.Context, targetSession string, fromMs, toMs int64, ingestAddr, probeToken string) error {
	if targetSession == "" {
		return ErrArchiveNoTarget
	}
	// 防并发重放同一目标：上一个回放没跑完就拒绝新指令（重复导入无害但浪费带宽）。
	a.uploadMu.Lock()
	if a.uploadInFly == nil {
		a.uploadInFly = make(map[string]bool)
	}
	if a.uploadInFly[targetSession] {
		a.uploadMu.Unlock()
		return ErrArchiveBusy
	}
	a.uploadInFly[targetSession] = true
	a.uploadMu.Unlock()
	defer func() {
		a.uploadMu.Lock()
		delete(a.uploadInFly, targetSession)
		a.uploadMu.Unlock()
	}()

	segs, err := a.Segments(ctx, fromMs, toMs)
	if err != nil {
		return err
	}
	if len(segs) == 0 {
		return ErrArchiveEmpty
	}

	conn, err := grpc.NewClient("passthrough:///"+ingestAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	pushCtx := metadata.AppendToOutgoingContext(ctx,
		"authorization", "Bearer "+probeToken,
		agent.StreamSessionMetadataKey, targetSession,
	)
	stream, err := proto.NewAgentIngestClient(conn).Push(pushCtx)
	if err != nil {
		return err
	}

	started := time.Now()
	var sent uint64
	const replayBatch = 256
	batch := &proto.PacketBatch{SessionId: targetSession, Iface: "probe-archive"}
	sendBatch := func() error {
		if len(batch.Packets) == 0 {
			return nil
		}
		if err := stream.Send(batch); err != nil {
			return err
		}
		sent += uint64(len(batch.Packets))
		batch = &proto.PacketBatch{SessionId: targetSession, Iface: "probe-archive"}
		return nil
	}

	for _, seg := range segs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := a.readSeg(seg, func(pkt *proto.RawPacket) error {
			batch.Packets = append(batch.Packets, pkt)
			if len(batch.Packets) >= replayBatch {
				return sendBatch()
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if err := sendBatch(); err != nil {
		return err
	}
	ack, err := stream.CloseAndRecv()
	if err != nil {
		return err
	}
	slog.Info("archive replay finished", "target", targetSession, "segments", len(segs),
		"packets", sent, "delivered", ack.GetDelivered(), "rejected", ack.GetRejected(),
		"elapsed", time.Since(started).Round(time.Second))
	if ack.GetRejected() > 0 {
		return ErrArchiveRejected
	}
	return nil
}

// readSeg 从段所属目录读出整段记录：活跃队列直接读；历史目录临时只读打开。
func (a *archiver) readSeg(seg ArchiveSegInfo, fn func(*proto.RawPacket) error) error {
	liveQ, liveDir := a.runner.Queue()
	if liveDir != "" && seg.Dir == liveDir && liveQ != nil {
		return liveQ.ReadSegment(seg.SegID, fn)
	}
	q, err := spool.Open(seg.Dir, spool.Options{Retention: &spool.Retention{}})
	if err != nil {
		return err
	}
	defer q.Close()
	return q.ReadSegment(seg.SegID, fn)
}

// 归档回放的错误集（调用方按需转成指令结果里的 error 描述）。
var (
	ErrArchiveNoTarget  = errArchive("archive upload: target session is required")
	ErrArchiveBusy      = errArchive("archive upload: another replay to this target is still running")
	ErrArchiveEmpty     = errArchive("archive upload: no archived data in the requested time range")
	ErrArchiveRejected  = errArchive("archive upload: some batches were rejected by the server (session not assigned to this probe?)")
)

type errArchive string

func (e errArchive) Error() string { return string(e) }
