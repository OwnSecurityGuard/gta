// Package probe 实现探针（probe）的服务端管理：注册、远端控制通道与三维度状态。
//
// 设计（docs/plans/2026-09-05-probe-control-archive-design.md）：
//   - 探针是个人资源：仅注册者（owner）本人可用，不做项目共享；
//   - 成员机在 NAT 后，远端控制走探针 outbound 的 gRPC 双向流（Connect），
//     服务端在流上下发指令（Command），探针上行心跳/指令结果（ControlEvent）；
//   - 控制模型是 desired-state，不是指令队列：任何 API 只更新"平台期望探针
//     处于的抓包状态"，探针在线时立即对齐；断线期间的指令不补偿堆积，
//     重连后以期望状态校正。这比补发指令队列简单且不会状态错乱；
//   - 状态三维度：connection（流活性）/ capture（状态机）/ data（包时间与计数），
//     由探针心跳聚合上报，服务端只存快照不做推断。
package probe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"gametrace/pkg/capture/agent"
	"gametrace/pkg/capture/agent/proto"
	"gametrace/pkg/store"
)

// ErrProbeOffline 表示目标探针当前没有活跃的控制流。
var ErrProbeOffline = errors.New("probe offline")

// ErrProbeBusy 表示控制流存在但发送缓冲已满（探针卡死或网络假在线）。
var ErrProbeBusy = errors.New("probe control channel busy")

// cmdTimeout 是等待探针返回指令结果的上限。
// 抓包启动（开网卡/BPF/建推流）走 starting 状态异步推进，本超时只覆盖
// 指令送达 + 探针确认受理，10s 覆盖跨网段 RTT 富余。
const cmdTimeout = 10 * time.Second

// ProbeStore 是 Manager 对探针持久化的最小依赖（store.ControlStoreBackend 满足它）。
type ProbeStore interface {
	GetProbe(ctx context.Context, probeID string) (*store.ProbeMeta, error)
	GetProbeByTokenHash(ctx context.Context, tokenHash string) (*store.ProbeMeta, error)
	UpsertProbe(ctx context.Context, m store.ProbeMeta) error
	UpdateProbeStatus(ctx context.Context, probeID string, st store.ProbeRuntimeStatus) error
	SetProbeConnection(ctx context.Context, probeID, state string, seen time.Time) error
	ListProbes(ctx context.Context) ([]store.ProbeMeta, error)
	RenameProbe(ctx context.Context, probeID, name string) error
	RevokeProbe(ctx context.Context, probeID string) error
	DeleteProbe(ctx context.Context, probeID string) error
	ReplaceProbeSegments(ctx context.Context, probeID string, segs []store.ArchiveSegmentMeta) error
	ListProbeSegments(ctx context.Context, probeID string, fromMs, toMs int64) ([]store.ArchiveSegmentMeta, error)
}

// Desired 是平台期望某探针处于的抓包状态（desired-state 控制模型的核心）。
// SessionID 为空串表示期望停止抓包。
type Desired struct {
	SessionID string
	Iface     string
	Ports     []int32
	Hosts     []string
	BPF       string
	SnapLen   int32
	Promisc   bool
}

// probeConn 是一条活跃控制流的发送端。
type probeConn struct {
	send   chan *proto.Command // 缓冲 16；满即视为探针假在线
	cancel context.CancelFunc
}

// Manager 是探针控制面的核心。并发约定：
//   - conns/desired/latest/queryWait/pendingResults 全部在 mu 下读写；
//   - 指令发送不持锁（channel send 会阻塞），靠 send 缓冲 + 非阻塞投递保证
//     锁外不卡；等待指令结果通过 per-command channel 完成。
type Manager struct {
	store    ProbeStore
	hub      *agent.Hub               // 离线导入回放投递用；nil 禁用
	sessions agent.SessionOwnerChecker // 回放目标会话归属校验；nil 跳过
	log      *slog.Logger

	mu             sync.Mutex
	conns          map[string]*probeConn
	latest         map[string]store.ProbeRuntimeStatus
	desired        map[string]Desired
	queryWait      map[string]chan *proto.ArchiveSegmentsReply // probeID → 归档查询应答
	pendingResults map[string]chan *proto.CommandResult        // cmdID → 指令结果
	seq            uint64
}

// NewManager 构造 Manager。hub/sessions 可为 nil（禁用离线导入）。
func NewManager(st ProbeStore, hub *agent.Hub, sessions agent.SessionOwnerChecker, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		store:          st,
		hub:            hub,
		sessions:       sessions,
		log:            log,
		conns:          make(map[string]*probeConn),
		latest:         make(map[string]store.ProbeRuntimeStatus),
		desired:        make(map[string]Desired),
		queryWait:      make(map[string]chan *proto.ArchiveSegmentsReply),
		pendingResults: make(map[string]chan *proto.CommandResult),
	}
}

// ---- 控制流生命周期（AgentControl.Connect 的服务端一侧） ----

// openConn 登记控制流并按 desired 状态对齐探针。返回是否为本次新建连接。
func (m *Manager) openConn(probeID string, cancel context.CancelFunc) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.conns[probeID]; ok {
		// 旧流还挂着（如网络假死）：作废它。探针侧会因流被服务端关闭而退出。
		old.cancel()
	}
	m.conns[probeID] = &probeConn{send: make(chan *proto.Command, 16), cancel: cancel}
	// 重连/首连都按 desired 对齐一次（不强制重发：探针侧幂等）。
	m.syncLocked(probeID, false)
	return true
}

// closeConn 控制流结束时清理；标记探针 offline（快照字段保留，UI 显示"上次在线 X 前"）。
func (m *Manager) closeConn(probeID string) {
	m.mu.Lock()
	if c, ok := m.conns[probeID]; ok {
		delete(m.conns, probeID)
		c.cancel()
	}
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = m.store.SetProbeConnection(ctx, probeID, "offline", time.Now().UTC())
	m.log.Info("probe disconnected", "probe_id", probeID)
}

// Online 报告探针当前是否有活跃控制流。
func (m *Manager) Online(probeID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.conns[probeID]
	return ok
}

// ---- 指令发送 ----

// nextCmdID 生成递增指令 id（服务端唯一即可，探针只做幂等去重）。
func (m *Manager) nextCmdID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	return fmt.Sprintf("cmd-%d", m.seq)
}

// sendAndWait 投递指令并等待结果。探针侧按 cmd.id 幂等去重。
// cmd 必须已带唯一 Id（调用方用 nextCmdID 生成）。
func (m *Manager) sendAndWait(probeID string, cmd *proto.Command) error {
	m.mu.Lock()
	c, ok := m.conns[probeID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrProbeOffline, probeID)
	}
	ch := make(chan *proto.CommandResult, 1)
	m.pendingResults[cmd.Id] = ch
	select {
	case c.send <- cmd:
	default:
		delete(m.pendingResults, cmd.Id)
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrProbeBusy, probeID)
	}
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.pendingResults, cmd.Id)
		m.mu.Unlock()
	}()
	select {
	case res := <-ch:
		if !res.GetOk() {
			return fmt.Errorf("probe rejected command %s: %s", cmd.Id, res.GetError())
		}
		return nil
	case <-time.After(cmdTimeout):
		return fmt.Errorf("probe %s did not answer command %s in %s", probeID, cmd.Id, cmdTimeout)
	}
}

// syncLocked 让探针向 desired 状态对齐。调用方必须持有 m.mu。
//
// 规则：
//   - desired 带会话：探针未在抓该会话（或停在 failed/stopped）→ 发 AssignCapture；
//     探针已 running 同一会话 → 不动（幂等）；force=true 时也发（重试语义，探针侧幂等受理）。
//   - desired 为空（期望停止）：探针在抓/在启动 → 发 Stop。
func (m *Manager) syncLocked(probeID string, force bool) {
	d, hasDesired := m.desired[probeID]
	_, online := m.conns[probeID]
	if !online {
		return
	}
	lt := m.latest[probeID]
	capturing := lt.CaptureState == "running" || lt.CaptureState == "starting"
	if hasDesired && d.SessionID != "" {
		if force || !capturing || lt.LastSessionID != d.SessionID {
			m.sendLockedNoWait(probeID, &proto.Command{
				Id: m.nextCmdID(),
				Payload: &proto.Command_Assign{
					Assign: &proto.AssignCapture{
						SessionId: d.SessionID,
						Iface:     d.Iface,
						Ports:     d.Ports,
						Hosts:     d.Hosts,
						Bpf:       d.BPF,
						Snaplen:   d.SnapLen,
						Promisc:   d.Promisc,
					},
				},
			})
		}
		return
	}
	if !hasDesired && capturing {
		m.sendLockedNoWait(probeID, &proto.Command{
			Id:      m.nextCmdID(),
			Payload: &proto.Command_Stop{Stop: &proto.StopCaptureCmd{}},
		})
	}
}

// sendLockedNoWait 锁内非阻塞投递（fire-and-forget；结果经 pendingResults 或日志暴露）。
// cmd 必须已带唯一 Id。调用方必须持有 m.mu。
func (m *Manager) sendLockedNoWait(probeID string, cmd *proto.Command) {
	c, ok := m.conns[probeID]
	if !ok {
		return
	}
	m.pendingResults[cmd.Id] = make(chan *proto.CommandResult, 1)
	select {
	case c.send <- cmd:
		m.log.Info("command dispatched", "probe_id", probeID, "cmd_id", cmd.Id, "payload", cmd.String())
	default:
		delete(m.pendingResults, cmd.Id)
		m.log.Warn("control channel full, command dropped (desired-state will re-align)", "probe_id", probeID)
	}
	// fire-and-forget 通道的结果无人读会积压：起 goroutine 排干。
	go func(id string, ch chan *proto.CommandResult) {
		select {
		case res := <-ch:
			if !res.GetOk() {
				m.log.Warn("command failed", "probe_id", probeID, "cmd_id", id, "error", res.GetError())
			}
		case <-time.After(cmdTimeout):
		}
	}(cmd.Id, m.pendingResults[cmd.Id])
}

// ---- 对外管理 API（capturecontrol / mcp 链路使用） ----

// List 返回全部探针（鉴权过滤在调用方做 creator 轴判定），并合并在线状态与最新快照。
func (m *Manager) List(ctx context.Context) ([]store.ProbeMeta, error) {
	probes, err := m.store.ListProbes(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range probes {
		id := probes[i].ProbeID
		if _, ok := m.conns[id]; ok {
			probes[i].ConnectionState = "online"
		} else if probes[i].ConnectionState != "offline" {
			probes[i].ConnectionState = "offline"
		}
		if lt, ok := m.latest[id]; ok {
			mergeRuntime(&probes[i], lt)
		}
	}
	return probes, nil
}

// Get 返回单个探针（含在线状态合并）。不存在返回 store 查询错误。
func (m *Manager) Get(ctx context.Context, probeID string) (*store.ProbeMeta, error) {
	p, err := m.store.GetProbe(ctx, probeID)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.conns[probeID]; ok {
		p.ConnectionState = "online"
	} else if p.ConnectionState != "offline" {
		p.ConnectionState = "offline"
	}
	if lt, ok := m.latest[probeID]; ok {
		mergeRuntime(p, lt)
	}
	return p, nil
}

func mergeRuntime(p *store.ProbeMeta, lt store.ProbeRuntimeStatus) {
	p.CaptureState = lt.CaptureState
	p.LastSessionID = lt.LastSessionID
	p.StatusError = lt.StatusError
	p.CaptureIface = lt.CaptureIface
	p.CapturePorts = lt.CapturePorts
	p.LastPacketMs = lt.LastPacketMs
	p.LastUploadMs = lt.LastUploadMs
	p.PacketsCaptured = lt.PacketsCaptured
	p.PacketsAcked = lt.PacketsAcked
	p.SpoolDepth = lt.SpoolDepth
	p.Dropped = lt.Dropped
	p.ArchiveBytes = lt.ArchiveBytes
	p.ArchiveSegments = lt.ArchiveSegments
	p.ArchiveOldestMs = lt.ArchiveOldestMs
	p.ArchiveNewestMs = lt.ArchiveNewestMs
}

// StartCapture 设定期望状态并对齐探针。指令被受理即返回；
// 抓包状态机推进（starting → running）由心跳异步反映，调用方轮询三态展示。
func (m *Manager) StartCapture(ctx context.Context, probeID string, d Desired) error {
	if _, err := m.store.GetProbe(ctx, probeID); err != nil {
		return fmt.Errorf("probe %s: %w", probeID, err)
	}
	m.mu.Lock()
	m.desired[probeID] = d
	m.syncLocked(probeID, false)
	m.mu.Unlock()
	return nil
}

// StopCapture 停止探针抓包（探针常驻），返回被停止的会话 id（未在抓包为空）。
func (m *Manager) StopCapture(ctx context.Context, probeID string) (string, error) {
	p, err := m.store.GetProbe(ctx, probeID)
	if err != nil {
		return "", fmt.Errorf("probe %s: %w", probeID, err)
	}
	m.mu.Lock()
	sessionID := p.LastSessionID
	delete(m.desired, probeID)
	m.syncLocked(probeID, false)
	m.mu.Unlock()
	return sessionID, nil
}

// UpdateFilter 热更新抓包过滤（探针侧 SetBPFFilter，不中断抓包），并同步 desired。
func (m *Manager) UpdateFilter(ctx context.Context, probeID string, ports []int32, hosts []string) error {
	if _, err := m.store.GetProbe(ctx, probeID); err != nil {
		return fmt.Errorf("probe %s: %w", probeID, err)
	}
	err := m.sendAndWait(probeID, &proto.Command{
		Id:      m.nextCmdID(),
		Payload: &proto.Command_Filter{Filter: &proto.UpdateFilter{Ports: ports, Hosts: hosts}},
	})
	if err != nil {
		return err
	}
	m.mu.Lock()
	if d, ok := m.desired[probeID]; ok {
		d.Ports, d.Hosts, d.BPF = ports, hosts, ""
		m.desired[probeID] = d
	}
	m.mu.Unlock()
	return nil
}

// Retry 让 failed 的探针重试上一次 assign（desired 不变，强制重发；探针侧幂等）。
func (m *Manager) Retry(ctx context.Context, probeID string) error {
	if _, err := m.store.GetProbe(ctx, probeID); err != nil {
		return fmt.Errorf("probe %s: %w", probeID, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.desired[probeID]; !ok {
		return errors.New("no pending capture assignment to retry")
	}
	m.syncLocked(probeID, true)
	return nil
}

// Rename 改机器名。
func (m *Manager) Rename(ctx context.Context, probeID, name string) error {
	return m.store.RenameProbe(ctx, probeID, name)
}

// Revoke 作废凭证（探针下次启动需重新接入）。
func (m *Manager) Revoke(ctx context.Context, probeID string) error {
	m.mu.Lock()
	if c, ok := m.conns[probeID]; ok {
		delete(m.conns, probeID)
		c.cancel()
	}
	delete(m.desired, probeID)
	m.mu.Unlock()
	return m.store.RevokeProbe(ctx, probeID)
}

// Delete 删除探针记录。
func (m *Manager) Delete(ctx context.Context, probeID string) error {
	m.mu.Lock()
	if c, ok := m.conns[probeID]; ok {
		delete(m.conns, probeID)
		c.cancel()
	}
	delete(m.desired, probeID)
	delete(m.latest, probeID)
	m.mu.Unlock()
	return m.store.DeleteProbe(ctx, probeID)
}

// ProbeForSession 返回当前指派到某会话的探针 id（AgentIngest assigned-probe 校验用）。
func (m *Manager) ProbeForSession(sessionID string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, d := range m.desired {
		if d.SessionID == sessionID {
			return id, true
		}
	}
	// desired 已清（会话可能已停）但探针还在推流的窗口：从 latest 兜底。
	for id, lt := range m.latest {
		if lt.LastSessionID == sessionID && (lt.CaptureState == "running" || lt.CaptureState == "starting") {
			return id, true
		}
	}
	return "", false
}

// OnSessionClosed 在抓包会话结束时联动：清除该会话的 desired 并让探针停止抓包
//（探针侧收到 Stop 后关闭 pcap handle，归档收尾，进程常驻）。
func (m *Manager) OnSessionClosed(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, d := range m.desired {
		if d.SessionID != sessionID {
			continue
		}
		delete(m.desired, id)
		m.syncLocked(id, false)
		m.log.Info("probe capture desired cleared (session closed)", "probe_id", id, "session_id", sessionID)
	}
}

// ---- 心跳与快照 ----

// applyHeartbeat 消化探针心跳：刷新 latest 快照并落库（探针离线后 UI 仍有最后状态）。
func (m *Manager) applyHeartbeat(probeID string, hb *proto.ProbeHeartbeat) {
	now := time.Now().UTC()
	st := store.ProbeRuntimeStatus{
		ConnectionState: "online",
		LastSeenAt:      now,
	}
	if c := hb.GetCapture(); c != nil {
		st.CaptureState = c.GetState()
		st.LastSessionID = c.GetSessionId()
		st.StatusError = c.GetError()
		st.CaptureIface = c.GetIface()
		st.CapturePorts = joinInt32(c.GetPorts())
	}
	if d := hb.GetData(); d != nil {
		st.LastPacketMs = d.GetLastPacketUnixMs()
		st.LastUploadMs = d.GetLastUploadUnixMs()
		st.PacketsCaptured = d.GetPacketsCaptured()
		st.PacketsAcked = d.GetPacketsAcked()
		st.SpoolDepth = d.GetSpoolDepth()
		st.Dropped = d.GetDropped()
	}
	if a := hb.GetArchive(); a != nil {
		st.ArchiveBytes = a.GetBytes()
		st.ArchiveSegments = a.GetSegments()
		st.ArchiveOldestMs = a.GetOldestUnix() * 1000
		st.ArchiveNewestMs = a.GetNewestUnix() * 1000
	}
	m.mu.Lock()
	m.latest[probeID] = st
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.store.UpdateProbeStatus(ctx, probeID, st); err != nil {
		m.log.Warn("persist probe heartbeat failed", "probe_id", probeID, "error", err)
	}
}

func joinInt32(xs []int32) string {
	out := make([]byte, 0, len(xs)*6)
	for i, x := range xs {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, []byte(fmt.Sprint(x))...)
	}
	return string(out)
}

// ---- 归档查询 / 回放 ----

// QueryArchive 向探针实时查询本地归档段（与 [from,to] 毫秒区间有交集）。
// 探针离线返回错误；调用方可退化为读服务端缓存（store.ListProbeSegments）。
func (m *Manager) QueryArchive(ctx context.Context, probeID string, fromMs, toMs int64) ([]store.ArchiveSegmentMeta, error) {
	ch := make(chan *proto.ArchiveSegmentsReply, 1)
	m.mu.Lock()
	c, ok := m.conns[probeID]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrProbeOffline, probeID)
	}
	old := m.queryWait[probeID]
	m.queryWait[probeID] = ch
	cmd := &proto.Command{Id: m.nextCmdID(), Payload: &proto.Command_ArchiveQuery{
		ArchiveQuery: &proto.ArchiveQuery{FromUnix: fromMs / 1000, ToUnix: toMs / 1000},
	}}
	select {
	case c.send <- cmd:
	default:
		m.queryWait[probeID] = old
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrProbeBusy, probeID)
	}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if m.queryWait[probeID] == ch {
			delete(m.queryWait, probeID)
		}
		m.mu.Unlock()
	}()

	select {
	case reply := <-ch:
		out := make([]store.ArchiveSegmentMeta, 0, len(reply.GetSegments()))
		for _, s := range reply.GetSegments() {
			out = append(out, store.ArchiveSegmentMeta{
				SegID:    s.GetSegId(),
				FirstMs:  s.GetFirstUnix() * 1000,
				LastMs:   s.GetLastUnix() * 1000,
				Packets:  s.GetPackets(),
				Bytes:    s.GetBytes(),
				LinkType: s.GetLinkType(),
			})
		}
		return out, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// StartArchiveUpload 令探针把 [from,to] 的本地归档回放进 targetSession。
// 数据经独立 UploadArchive 流上行（不占控制流），Manager.UploadArchive 负责接收投递。
func (m *Manager) StartArchiveUpload(ctx context.Context, probeID, targetSession string, fromMs, toMs int64) error {
	return m.sendAndWait(probeID, &proto.Command{
		Id: m.nextCmdID(),
		Payload: &proto.Command_ArchiveUpload{
			ArchiveUpload: &proto.ArchiveUpload{
				TargetSessionId: targetSession,
				FromUnix:        fromMs / 1000,
				ToUnix:          toMs / 1000,
			},
		},
	})
}

// ListArchiveCached 返回服务端缓存的归档段清单（探针离线时的退化查询路径）。
// fromMs/toMs 为 0 时不限区间。
func (m *Manager) ListArchiveCached(ctx context.Context, probeID string, fromMs, toMs int64) ([]store.ArchiveSegmentMeta, error) {
	return m.store.ListProbeSegments(ctx, probeID, fromMs, toMs)
}

// UpsertArchiveSegments 把一次实时查询的结果合并进缓存（按 SegID 覆盖）。
// 查询是按时间窗的，不能整表替换——窗口外的段必须保留。
func (m *Manager) UpsertArchiveSegments(ctx context.Context, probeID string, segs []store.ArchiveSegmentMeta) error {
	if len(segs) == 0 {
		return nil
	}
	existing, err := m.store.ListProbeSegments(ctx, probeID, 0, 0)
	if err != nil {
		return err
	}
	byID := make(map[string]store.ArchiveSegmentMeta, len(existing)+len(segs))
	for _, s := range existing {
		byID[s.SegID] = s
	}
	for _, s := range segs {
		byID[s.SegID] = s
	}
	out := make([]store.ArchiveSegmentMeta, 0, len(byID))
	for _, s := range byID {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FirstMs < out[j].FirstMs })
	return m.store.ReplaceProbeSegments(ctx, probeID, out)
}
