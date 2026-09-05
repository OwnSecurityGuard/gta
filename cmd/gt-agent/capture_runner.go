package main

// captureRunner 是探针侧的抓包状态机（三维度的第二/三维）。
//
// 状态：idle → starting → running → stopped / failed（docs/plans/2026-09-05 §5.2）。
//   - Start 幂等：同会话同参数且在 running → no-op（远端指令重放安全）；
//   - Stop 后进程常驻（stopped 保留 last session 供 UI 展示），不退出；
//   - 失败停在 failed（带 error），等待本地重试或平台 Retry 指令；
//   - UpdateFilter 在 running 下热更新 BPF（不断流）。
//
// 数据面：capture goroutine → packets chan → ingestClient（spool 落盘 → 推流）。
// captured/last_packet 在 chan 入口统计；acked/last_upload 由 ingest 确认回调统计。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gametrace/pkg/capture/agent/proto"
	"gametrace/pkg/spool"
)

// capture 状态常量（与 proto.ProbeCaptureStatus.state 的取值一致）。
const (
	stateIdle     = "idle"
	stateStarting = "starting"
	stateRunning  = "running"
	stateStopped  = "stopped"
	stateFailed   = "failed"
)

// DataStats 是数据面快照（三维度的第三维）。
type DataStats struct {
	LastPacketMs    int64 // 0 = 从未
	LastUploadMs    int64 // 0 = 从未
	PacketsCaptured uint64
	PacketsAcked    uint64
	SpoolDepth      uint64
	Dropped         uint64
}

// CaptureParams 是一次抓包的参数（AssignCapture / 本地 start 共用）。
type CaptureParams struct {
	SessionID string
	Iface     string
	Ports     []int32
	Hosts     []string
	BPF       string // 显式 BPF；空则按 Ports/Hosts 派生
	SnapLen   int32
	Promisc   bool
}

// deriveBPF 把 ports/hosts/显式 BPF 统一成最终过滤表达式。
// 显式 BPF 非空时直接用（覆盖派生）；全空返回空串（不过滤）。
func deriveBPF(p CaptureParams) string {
	if p.BPF != "" {
		return p.BPF
	}
	var parts []string
	for _, port := range p.Ports {
		if port > 0 && port <= 65535 {
			parts = append(parts, fmt.Sprintf("tcp port %d", port))
		}
	}
	for _, h := range p.Hosts {
		if h = strings.TrimSpace(h); h != "" {
			parts = append(parts, "host "+h)
		}
	}
	return strings.Join(parts, " or ")
}

// captureRunner 是并发安全的抓包状态机。所有字段经 mu 读写；
// 抓包/推流 goroutine 通过 runner 内部 channel 与状态机通信。
type captureRunner struct {
	mu         sync.Mutex
	state      string
	params     CaptureParams // last/当前参数（stopped 保留供 UI 展示）
	lastErr    string
	updatedMs  int64
	live       *liveCapture // running 时非 nil（SetFilter 用）
	cancelFn   context.CancelFunc
	packets    chan *proto.RawPacket
	capEnded   chan error
	spool      *spool.Queue
	spoolDir   string
	ingestAddr string
	token      string

	// retention 是归档留存策略（nil = 发后即焚）；Start 打开 spool 时应用，
	// 运行期变更经 archiver.applyRetention 热切换。
	retention *spool.Retention
	// 推流批参数（命令行 flag 可覆盖默认值）。
	batchSize     int
	batchInterval time.Duration

	// 数据统计（atomic；心跳无锁读取）
	lastPacketMs    atomic.Int64
	lastUploadMs    atomic.Int64
	packetsCaptured atomic.Uint64
	packetsAcked    atomic.Uint64
	dropped         atomic.Uint64
}

func newCaptureRunner() *captureRunner {
	return &captureRunner{state: stateIdle, packets: nil, batchSize: 128, batchInterval: 200 * time.Millisecond}
}

// setRetention 设置后续 Start 打开 spool 时应用的留存策略；已在运行的队列同步热切换。
func (r *captureRunner) setRetention(rt *spool.Retention) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retention = rt
	if r.spool != nil {
		r.spool.EnableRetention(rt)
	}
}

// Queue 返回当前 spool 队列与其目录（idle 时队列为 nil）。
func (r *captureRunner) Queue() (*spool.Queue, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.spool, r.spoolDir
}

// Close 停机收尾：刷盘并关闭 spool。磁盘数据保留（归档模式留存 / 未确认数据续传）。
func (r *captureRunner) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.spool == nil {
		return nil
	}
	err := r.spool.Close()
	r.spool = nil
	return err
}

// State 返回当前状态机快照（本地控制面与心跳共用）。
func (r *captureRunner) State() (state, sessionID, iface, portsCSV, lastErr string, updatedMs int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state, r.params.SessionID, r.params.Iface, joinInt32s(r.params.Ports), r.lastErr, r.updatedMs
}

// Data 返回数据面快照。
func (r *captureRunner) Data() DataStats {
	depth := uint64(0)
	r.mu.Lock()
	q := r.spool
	r.mu.Unlock()
	if q != nil {
		// Depth 返回 (未确认条数, 未确认字节数)。
		n, _ := q.Depth()
		depth = uint64(n)
	}
	return DataStats{
		LastPacketMs:    r.lastPacketMs.Load(),
		LastUploadMs:    r.lastUploadMs.Load(),
		PacketsCaptured: r.packetsCaptured.Load(),
		PacketsAcked:    r.packetsAcked.Load(),
		SpoolDepth:      depth,
		Dropped:         r.dropped.Load(),
	}
}

// setState 更新状态机（调用方必须持有 r.mu）。
func (r *captureRunner) setState(state string, errStr string) {
	r.state = state
	r.lastErr = errStr
	r.updatedMs = time.Now().UnixMilli()
}

// Start 启动（或切换）抓包。幂等：同会话同参数且 running → no-op。
// fail-fast 参数错误；运行期失败经 capEnded 进入 failed。
func (r *captureRunner) Start(p CaptureParams, ingestAddr, token string) error {
	if p.SessionID == "" {
		return errors.New("capture start: session_id is required")
	}
	if p.SnapLen <= 0 {
		p.SnapLen = 1600
	}
	bpf := deriveBPF(p)
	p.BPF = bpf

	r.mu.Lock()
	defer r.mu.Unlock()

	// 幂等：同会话同参数已 running，直接受理（指令重放安全）。
	if r.state == stateRunning && r.params.SessionID == p.SessionID &&
		r.params.Iface == p.Iface && r.params.BPF == p.BPF {
		return nil
	}
	// 先停旧的（换会话/换参数）。
	r.stopLocked()

	r.setState(stateStarting, "")
	r.params = p
	r.ingestAddr = ingestAddr
	r.token = token

	runCtx, cancel := context.WithCancel(context.Background())
	r.cancelFn = cancel
	r.packets = make(chan *proto.RawPacket, 1024)
	r.capEnded = make(chan error, 1)

	lc, err := runCapture(runCtx, captureConfig{
		Iface: p.Iface, BPF: bpf, SnapLen: p.SnapLen, Promisc: p.Promisc,
	}, r.packets, r.capEnded)
	if err != nil {
		cancel()
		r.setState(stateFailed, err.Error())
		return err
	}
	r.live = lc

	// spool 按会话隔离（断电续传按会话恢复）。
	if r.spool == nil || r.spoolDir != defaultSpoolDir(p.SessionID) {
		if r.spool != nil {
			_ = r.spool.Close()
			r.spool = nil
		}
		dir := defaultSpoolDir(p.SessionID)
		q, err := spool.Open(dir, spool.Options{Retention: r.retention})
		if err != nil {
			cancel()
			r.setState(stateFailed, fmt.Sprintf("open spool: %v", err))
			return fmt.Errorf("open ingest spool: %w", err)
		}
		r.spool = q
		r.spoolDir = dir
	}

	ic := &ingestClient{
		addr:         ingestAddr,
		token:        token,
		sessionID:    p.SessionID,
		iface:        p.Iface,
		batchSize:    r.batchSize,
		batchInterval: r.batchInterval,
		spool:        r.spool,
		onAck:        r.onAcked,
	}
	// 换会话时计数归零（新会话从 0 开始，UI 语义是"本次抓了多少"）。
	r.packetsCaptured.Store(0)
	r.packetsAcked.Store(0)

	go func() {
		ic.run(runCtx, r.packets)
	}()
	go r.watchCapEnded(runCtx)

	r.setState(stateRunning, "")
	slog.Info("capture started", "session", p.SessionID, "iface", p.Iface, "bpf", bpf)
	return nil
}

// watchCapEnded 抓包源意外关闭（网卡消失）→ 状态机进 failed 并停止推流。
// ctx 正常取消（Stop）时 ended 无错误发送，静默结束。
func (r *captureRunner) watchCapEnded(runCtx context.Context) {
	select {
	case <-runCtx.Done():
		return
	case err := <-r.capEnded:
		if err == nil {
			return
		}
		r.mu.Lock()
		if r.state == stateRunning || r.state == stateStarting {
			r.setState(stateFailed, err.Error())
			if r.cancelFn != nil {
				r.cancelFn()
			}
		}
		r.mu.Unlock()
	}
}

// onAcked 是 ingestClient 的确认回调：推进上传计数（心跳读）。
func (r *captureRunner) onAcked(n int) {
	r.lastUploadMs.Store(time.Now().UnixMilli())
	r.packetsAcked.Add(uint64(n))
}

// Stop 停止抓包（探针常驻）。幂等。
func (r *captureRunner) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == stateIdle || r.state == stateStopped {
		return nil
	}
	r.stopLocked()
	r.setState(stateStopped, "")
	slog.Info("capture stopped", "session", r.params.SessionID)
	return nil
}

// stopLocked 停掉当前抓包与推流（调用方必须持有 r.mu）。
// 不改状态机——由 Start（切换）/Stop（正常停止）/watchCapEnded（失败）决定终态。
func (r *captureRunner) stopLocked() {
	if r.cancelFn != nil {
		r.cancelFn()
		r.cancelFn = nil
	}
	r.live = nil
	// packets/capEnded chan 留给 GC；下次 Start 重建。
}

// UpdateFilter 热更新过滤（running 下 SetBPFFilter，不断流）。
// 空 ports/hosts/bpf = 清除过滤（全抓）。
func (r *captureRunner) UpdateFilter(ports []int32, hosts []string, bpf string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != stateRunning || r.live == nil {
		return errors.New("capture is not running; filter update requires a running capture")
	}
	derived := deriveBPF(CaptureParams{Ports: ports, Hosts: hosts, BPF: bpf})
	if err := r.live.SetFilter(derived); err != nil {
		return err
	}
	r.params.Ports = ports
	r.params.Hosts = hosts
	r.params.BPF = derived
	return nil
}

func joinInt32s(xs []int32) string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, strconv.Itoa(int(x)))
	}
	return strings.Join(out, ",")
}
