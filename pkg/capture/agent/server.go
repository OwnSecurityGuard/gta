package agent

import (
	"io"
	"log/slog"
	"sync"

	"google.golang.org/grpc/peer"

	"gta/pkg/auth"
	"gta/pkg/capture/agent/proto"
)

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

	mu        sync.Mutex
	streamSeq uint64 // 流编号（日志用）
}

// NewIngestServer 构造 AgentIngest server。
// sessions 为 nil 时跳过会话归属校验（仅建议单机匿名模式使用）。
func NewIngestServer(hub *Hub, sessions SessionOwnerChecker) *IngestServer {
	return &IngestServer{hub: hub, sessions: sessions}
}

// Push 接收 agent 的客户端流。
// 每个 PacketBatch 携带 session_id；会话归属与调用者 owner 不一致时按
// batch 逐个拒绝（rejected++、告警日志每流最多一次），流继续保持，
// PushAck.Rejected 汇总拒绝数——这样 agent 能在流结束拿到完整统计，
// 而不是只看到第一个坏 batch 就被掐断。
// 流中途断开（agent 重连）不影响 server 继续接受新流；重连期间
// 未被投递的包直接丢弃（无订阅者/慢消费者分开计数），不做缓存补发。
func (s *IngestServer) Push(stream proto.AgentIngest_PushServer) error {
	owner := auth.OwnerFrom(stream.Context())
	seq := s.nextStreamSeq()
	var (
		batches   uint64
		packets   uint64
		delivered uint64
		dropped   uint64
		rejected  uint64
	)
	slog.Debug("agent ingest stream opened", "stream", seq, "owner", owner, "peer", peerOf(stream))

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
		if !s.authorize(owner, sessionID) {
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
func (s *IngestServer) authorize(owner, sessionID string) bool {
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
	return sessionOwner == owner
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
