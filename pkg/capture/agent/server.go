package agent

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	"gta/pkg/auth"
	"gta/pkg/capture/agent/proto"
)

// StreamSessionMetadataKey 是 agent 在开流时通过 gRPC metadata 声明目标会话的键。
//
// 存在的理由：Push 流里 session_id 只在 PacketBatch 携带，而 agent 抓不到包时
// 一个 batch 都不会发。没有它，服务端无法区分「Agent 已连上但目标端口没流量」
// 和「Agent 压根没连上」——这两个状态对用户是完全不同的两件事（前者该去启动游戏，
// 后者该去检查 agent 是否运行）。旧版 agent 不带该 metadata 时，服务端退化为
// 收到首个合法 batch 后才绑定会话（见 Push）。
const StreamSessionMetadataKey = "session-id"

// SessionOwnerChecker 查询抓包会话的归属 owner。
// 抽成接口是为了避免 pkg/capture 依赖 pkg/store（造成反向依赖/导入环）：
// 宿主（cmd/gta-pipeline）用 ControlStore.GetSession 实现它注入进来。
type SessionOwnerChecker interface {
	// SessionOwner 返回会话归属 owner；会话不存在时返回 ("", false)。
	SessionOwner(sessionID string) (owner string, ok bool)
}

// SessionOwnerCheckerFunc 是 SessionOwnerChecker 的函数适配器。
type SessionOwnerCheckerFunc func(sessionID string) (string, bool)

func (f SessionOwnerCheckerFunc) SessionOwner(sessionID string) (string, bool) {
	return f(sessionID)
}

// ProbeAssignChecker 查询某会话当前指派给的探针。由宿主注入 pkg/probe.Manager。
type ProbeAssignChecker interface {
	// ProbeForSession 返回指派到该会话的探针 id；无指派返回 ("", false)。
	ProbeForSession(sessionID string) (probeID string, ok bool)
}

// ProbeAssignSetter 是宿主注入 ProbeAssignChecker 的可选入口（IngestServer 实现）。
type ProbeAssignSetter interface{ SetProbeAssignChecker(ProbeAssignChecker) }

// IngestServer 实现 proto.AgentIngestServer：接收 gta-agent 的包推送并经 Hub 路由。
//
// 鉴权：由宿主在 grpc.Server 上挂 pkg/auth 的 StreamInterceptor，
// Push 的 stream ctx 里能取到 Principal（OwnerFrom）。匿名模式下
// resolver 未配置任何 token，owner 统一为 auth.AnonymousOwner（"local"），
// 与本地单机创建的会话归属一致。
type IngestServer struct {
	proto.UnimplementedAgentIngestServer

	hub      *Hub
	sessions SessionOwnerChecker
	// probeAssign 非 nil 时，探针凭证（principal 带 ProbeID）推流需要校验
	// 该会话确实指派给了本探针（防止 A 探针往 B 的会话灌数据）。
	probeAssign ProbeAssignChecker

	mu        sync.Mutex
	streamSeq uint64 // 流编号（日志用）
	// live 按会话记录 Agent 连接活性：streams>0 即有 agent 已连上（哪怕零流量），
	// lastSeen 是最近一次收到该会话数据包的时间（零值 = 从未收到）。
	// 会话结束后条目不主动清理——会话数有限，且保留 lastSeen 让 UI 在
	// 会话停止后仍能显示"最后收到数据于 X"，比立刻抹掉更有诊断价值。
	live map[string]*sessionLiveness
}

// sessionLiveness 是单个会话的 Agent 连接活性。
type sessionLiveness struct {
	streams  int
	lastSeen time.Time
}

// NewIngestServer 构造 AgentIngest server。
// sessions 为 nil 时跳过会话归属校验（仅建议单机匿名模式使用）。
func NewIngestServer(hub *Hub, sessions SessionOwnerChecker) *IngestServer {
	return &IngestServer{hub: hub, sessions: sessions, live: make(map[string]*sessionLiveness)}
}

// SetProbeAssignChecker 注入探针指派校验源（宿主在启用探针管理面时调用）。
func (s *IngestServer) SetProbeAssignChecker(c ProbeAssignChecker) { s.probeAssign = c }

// SessionLiveness 返回某会话的 Agent 连接活性，供状态查询（GetCaptureStatus）上报。
//
// connected：当前存在活跃的 Push 流——注意它只表示"连上了"，不代表有流量。
// lastSeen：最近一次收到该会话数据包的时间；零值表示从未收到过数据。
func (s *IngestServer) SessionLiveness(sessionID string) (connected bool, lastSeen time.Time) {
	if sessionID == "" {
		return false, time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.live[sessionID]
	if l == nil {
		return false, time.Time{}
	}
	return l.streams > 0, l.lastSeen
}

// entry 取（或建）某会话的活性记录。调用方必须持有 s.mu。
func (s *IngestServer) entry(sessionID string) *sessionLiveness {
	l := s.live[sessionID]
	if l == nil {
		l = &sessionLiveness{}
		s.live[sessionID] = l
	}
	return l
}

// openStream 记录一条 Push 流建立（agent 已连上）。
func (s *IngestServer) openStream(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entry(sessionID).streams++
}

// closeStream 记录一条 Push 流结束。
func (s *IngestServer) closeStream(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l := s.live[sessionID]; l != nil && l.streams > 0 {
		l.streams--
	}
}

// touch 记录该会话刚收到数据（用于"最近一次流量"展示）。
func (s *IngestServer) touch(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entry(sessionID).lastSeen = time.Now()
}

// Push 接收 agent 的客户端流。
// 每个 PacketBatch 携带 session_id；会话归属与调用者 owner 不一致时按
// batch 逐个拒绝（rejected++、告警日志每流最多一次），流继续保持，
// PushAck.Rejected 汇总拒绝数——这样 agent 能在流结束拿到完整统计，
// 而不是只看到第一个坏 batch 就被掐断。
// 流中途断开（agent 重连）不影响 server 继续接受新流；重连期间
// 未被投递的包直接丢弃（无订阅者/慢消费者分开计数），不做缓存补发。
func (s *IngestServer) Push(stream proto.AgentIngest_PushServer) error {
	principal, _ := auth.PrincipalFrom(stream.Context())
	owner := principal.Owner
	probeID := principal.ProbeID
	seq := s.nextStreamSeq()
	var (
		batches   uint64
		packets   uint64
		delivered uint64
		dropped   uint64
		rejected  uint64
	)
	slog.Debug("agent ingest stream opened", "stream", seq, "owner", owner, "peer", peerOf(stream))

	// 会话绑定：优先用开流 metadata（零流量也能判定"已连接"）；旧版 agent 不带
	// metadata 时退化为收到首个合法 batch 后按 batch 的 session_id 绑定。
	// 绑定只做一次——一个 agent 进程只服务一个会话，后续 batch 的 session_id
	// 变化（异常 agent）不改变本流的绑定归属。
	bound := sessionIDFromMD(stream.Context())
	if bound != "" {
		s.openStream(bound)
		defer s.closeStream(bound)
	}

	for {
		batch, err := stream.Recv()
		if err == io.EOF {
			return stream.SendAndClose(&proto.PushAck{
				Batches:   batches,
				Packets:   packets,
				Delivered: delivered,
				Dropped:   dropped,
				Rejected:  rejected,
			})
		}
		if err != nil {
			return err
		}
		batches++
		sessionID := batch.GetSessionId()
		if !s.authorize(owner, sessionID, probeID) {
			rejected++
			if rejected == 1 {
				// 每条流只告警一次，避免坏 agent 刷日志。
				slog.Warn("agent ingest batch rejected: session owner mismatch (subsequent rejections counted silently)",
					"stream", seq, "owner", owner, "session_id", sessionID)
			}
			continue
		}
		pkts := packetsFromBatch(batch)
		packets += uint64(len(pkts))
		if bound == "" {
			// 旧版 agent：首个合法 batch 才暴露会话，此时补登记连接活性。
			bound = sessionID
			s.openStream(bound)
			defer s.closeStream(bound)
		}
		if len(pkts) > 0 {
			s.touch(sessionID)
		}
		d, drBusy, drNoSub := s.hub.Deliver(sessionID, pkts)
		delivered += d
		dropped += drBusy + drNoSub
	}
}

// authorize 校验调用者 owner 是否拥有该会话。
// 规则：
//   - sessions 为 nil：放行（无校验能力，单机匿名模式的退路）；
//   - 会话不存在：拒绝（agent 指向了错误/已结束的会话）；
//   - 会话 owner 与调用者 owner 一致：放行。
//     兼容约定：owner 为空的既有会话视为匿名（"local"）所有。
//   - 探针凭证（probeID 非空）附加指派校验：平台侧该会话已指派给别的探针时拒绝
//     （防 A 探针往 B 的会话灌数据）；无指派记录（老 agent / 会话刚结束的尾巴）不拦。
func (s *IngestServer) authorize(owner, sessionID, probeID string) bool {
	if sessionID == "" {
		return false
	}
	if s.sessions == nil {
		return true
	}
	sessionOwner, ok := s.sessions.SessionOwner(sessionID)
	if !ok {
		return false
	}
	if sessionOwner == "" {
		sessionOwner = auth.AnonymousOwner
	}
	if sessionOwner != owner {
		return false
	}
	if probeID != "" && s.probeAssign != nil {
		if assigned, assignedOK := s.probeAssign.ProbeForSession(sessionID); assignedOK && assigned != probeID {
			slog.Warn("agent ingest batch rejected: session assigned to another probe",
				"session_id", sessionID, "caller_probe", probeID, "assigned_probe", assigned)
			return false
		}
	}
	return true
}

func (s *IngestServer) nextStreamSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamSeq++
	return s.streamSeq
}

func peerOf(stream proto.AgentIngest_PushServer) string {
	if p, ok := peer.FromContext(stream.Context()); ok {
		return p.Addr.String()
	}
	return "unknown"
}

// sessionIDFromMD 从开流 metadata 读取目标会话 id（见 StreamSessionMetadataKey）。
// 缺失返回空串——调用方据此退化为 batch 级绑定。
func sessionIDFromMD(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	if vals := md.Get(StreamSessionMetadataKey); len(vals) > 0 {
		return vals[0]
	}
	return ""
}
