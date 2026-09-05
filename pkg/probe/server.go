package probe

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"gta/pkg/auth"
	"gta/pkg/capture/agent"
	"gta/pkg/capture/agent/proto"
	"gta/pkg/event"
	"gta/pkg/store"
)

// heartbeatInterval 是探针心跳周期（服务端用于断流后 30s offline 的对齐参考）。
const heartbeatInterval = 10 * time.Second

// Server 实现 proto.AgentControlServer，与 AgentIngest 共用同一个 gRPC 端口（:9092）。
//
// 鉴权模型：
//   - RegisterProbe：用户 token（claim 启动码所得）；匿名部署 owner=local；
//   - Connect / UploadArchive：必须用 probe_token（auth resolver 解析出 ProbeID）。
type Server struct {
	proto.UnimplementedAgentControlServer
	mgr *Manager
	log *slog.Logger
}

// NewServer 构造 AgentControl server。
func NewServer(mgr *Manager, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{mgr: mgr, log: log}
}

// RegisterProbe 注册探针并换发长期凭证。用户 token 鉴权（ctx Principal）。
//
// 幂等语义：
//   - prev_probe_id 非空：覆盖旧记录（token 丢失重接），owner 必须一致；
//   - 同 owner + 同 hostname 已有探针：视为同一台机器重装，覆盖旧记录（换发新 token）；
//   - 其余情况新建（同一 owner 可注册多台机器）。
func (s *Server) RegisterProbe(ctx context.Context, req *proto.RegisterProbeRequest) (*proto.RegisterProbeAck, error) {
	owner := auth.OwnerFrom(ctx)
	principal, _ := auth.PrincipalFrom(ctx)
	tenant := principal.Tenant
	name := req.GetName()
	if name == "" {
		name = req.GetHostname()
	}
	probeID := req.GetPrevProbeId()

	if existing, err := s.mgr.store.GetProbe(ctx, probeID); err == nil {
		if existing.Owner != owner {
			return nil, status.Errorf(codes.PermissionDenied,
				"probe %s belongs to another user; revoke it first", probeID)
		}
	} else {
		probeID = "" // prev 无效（不存在或为空）：按新注册走
	}

	if probeID == "" {
		// 同 owner + 同 hostname 视为同一台机器（重装 agent 的常见场景）。
		probes, err := s.mgr.store.ListProbes(ctx)
		if err == nil {
			for _, p := range probes {
				if p.Owner == owner && p.Hostname == req.GetHostname() && req.GetHostname() != "" {
					probeID = p.ProbeID
					break
				}
			}
		}
	}
	if probeID == "" {
		probeID = newProbeID()
	}

	token, tokenHash, err := newProbeToken()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate probe token: %v", err)
	}
	caps := req.GetCapabilities()
	if len(caps) == 0 {
		caps = []string{"pcap", "plugin_host"}
	}
	err = s.mgr.store.UpsertProbe(ctx, store.ProbeMeta{
		ProbeID:      probeID,
		Name:         name,
		Owner:        owner,
		TenantID:     tenant,
		Capabilities: joinStrings(caps),
		TokenHash:    tokenHash,
		Version:      req.GetVersion(),
		Hostname:     req.GetHostname(),
		OS:           req.GetOs(),
		Arch:         req.GetArch(),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "persist probe: %v", err)
	}
	s.log.Info("probe registered", "probe_id", probeID, "name", name, "owner", owner,
		"hostname", req.GetHostname(), "capabilities", caps)
	return &proto.RegisterProbeAck{ProbeId: probeID, ProbeToken: token}, nil
}

// Connect 控制双向流。探针开流首包必须 ProbeHello（probe_id 与凭证一致）。
//
// 服务端 goroutine 模型：
//   - 本 handler 的读循环收 ControlEvent（心跳/结果/归档应答）；
//   - send goroutine 从 conn.send 取 Command 下发；
//   - 连接建立时按 desired 状态对齐（openConn）；流断时清理并标 offline（closeConn）。
func (s *Server) Connect(stream proto.AgentControl_ConnectServer) error {
	principal, _ := auth.PrincipalFrom(stream.Context())
	probeID := principal.ProbeID
	if probeID == "" {
		return status.Error(codes.PermissionDenied,
			"probe token required: register via AgentControl.RegisterProbe first")
	}
	p, err := s.mgr.store.GetProbe(stream.Context(), probeID)
	if err != nil {
		return status.Error(codes.PermissionDenied, "unknown probe")
	}
	if p.TokenHash == "" {
		return status.Error(codes.PermissionDenied, "probe credentials revoked; re-register required")
	}

	// 首包必须是 Hello，且 probe_id 必须与凭证一致（防串号）。
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first ControlEvent must be hello")
	}
	if hello.GetProbeId() != probeID {
		return status.Error(codes.PermissionDenied, "hello probe_id does not match credentials")
	}

	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	s.mgr.openConn(probeID, cancel)
	defer s.mgr.closeConn(probeID)

	_ = s.mgr.store.SetProbeConnection(stream.Context(), probeID, "online", time.Now().UTC())
	s.log.Info("probe connected", "probe_id", probeID, "name", p.Name,
		"version", hello.GetVersion(), "peer", peerOf(stream.Context()))

	// 发送侧：从 conn.send 取指令下发给探针。ctx 取消（新连接顶替 / 吊销 / 停机）即退出。
	sendDone := make(chan error, 1)
	go func() {
		defer close(sendDone)
		for {
			s.mgr.mu.Lock()
			c, ok := s.mgr.conns[probeID]
			if !ok {
				s.mgr.mu.Unlock()
				return
			}
			conn := c
			s.mgr.mu.Unlock()

			select {
			case <-ctx.Done():
				return
			case cmd := <-conn.send:
				if err := stream.Send(cmd); err != nil {
					s.log.Warn("send command failed", "probe_id", probeID, "cmd_id", cmd.GetId(), "error", err)
					return
				}
			}
		}
	}()

	// 读侧：心跳 / 指令结果 / 归档查询应答。
	for {
		evt, err := stream.Recv()
		if err != nil {
			s.log.Info("probe control stream closed", "probe_id", probeID, "error", err)
			return nil
		}
		s.handleEvent(probeID, evt)
	}
}

func (s *Server) handleEvent(probeID string, evt *proto.ControlEvent) {
	switch p := evt.GetPayload().(type) {
	case *proto.ControlEvent_Hello:
		// 首包已在 Connect 校验；重复 hello 忽略。
	case *proto.ControlEvent_Heartbeat:
		s.mgr.applyHeartbeat(probeID, p.Heartbeat)
	case *proto.ControlEvent_Result:
		s.mgr.mu.Lock()
		ch, ok := s.mgr.pendingResults[p.Result.GetId()]
		s.mgr.mu.Unlock()
		if ok {
			select {
			case ch <- p.Result:
			default:
			}
		}
	case *proto.ControlEvent_ArchiveSegments:
		s.mgr.mu.Lock()
		ch, ok := s.mgr.queryWait[probeID]
		s.mgr.mu.Unlock()
		if ok {
			select {
			case ch <- p.ArchiveSegments:
			default:
			}
		}
	default:
		s.log.Warn("unknown control event", "probe_id", probeID)
	}
}

// UploadArchive 接收探针的归档回放流并投递给目标会话（离线导入的数据面）。
//
// 校验：probe_token 身份；目标会话存在且 owner 与探针一致（防止把数据灌进别人的会话）。
// 包序与原始时间戳保持（Hub 背压沿 gRPC 流传导回探针侧读盘速率）。
func (s *Server) UploadArchive(stream proto.AgentControl_UploadArchiveServer) error {
	principal, _ := auth.PrincipalFrom(stream.Context())
	probeID := principal.ProbeID
	if probeID == "" {
		return status.Error(codes.PermissionDenied, "probe token required")
	}
	if s.mgr.hub == nil {
		return status.Error(codes.Unavailable, "archive import disabled (no agent hub)")
	}

	var (
		target    string
		packets   uint64
		dropped   uint64
		firstRecv bool
	)
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			_ = stream.SendAndClose(&proto.UploadArchiveAck{Packets: packets, Dropped: dropped})
			s.log.Info("archive upload finished", "probe_id", probeID,
				"target_session", target, "packets", packets, "dropped", dropped)
			return nil
		}
		if err != nil {
			return err
		}
		if chunk.GetFinal() {
			continue
		}
		if !firstRecv {
			target = chunk.GetTargetSessionId()
			if !s.authorizeImport(stream.Context(), probeID, target) {
				return status.Errorf(codes.PermissionDenied, "session %s is not importable by probe %s", target, probeID)
			}
			firstRecv = true
		}
		if chunk.GetPacket() == nil {
			continue
		}
		pkt := agent.PacketFromProto(chunk.GetPacket(), "")
		d, _, _ := s.mgr.hub.Deliver(target, []event.Packet{pkt})
		packets += d
	}
}

// authorizeImport 校验回放目标会话：存在 且 owner 与探针注册者一致。
func (s *Server) authorizeImport(ctx context.Context, probeID, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	if s.mgr.sessions == nil {
		return true // 单机匿名模式无校验能力
	}
	sessionOwner, ok := s.mgr.sessions.SessionOwner(sessionID)
	if !ok {
		return false
	}
	if sessionOwner == "" {
		sessionOwner = auth.AnonymousOwner
	}
	p, err := s.mgr.store.GetProbe(ctx, probeID)
	if err != nil {
		return false
	}
	return p.Owner == sessionOwner
}

func peerOf(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok {
		return p.Addr.String()
	}
	return "unknown"
}

// ---- 凭证生成 ----

// newProbeID 生成 "prb_" + 8 字节随机 hex。
func newProbeID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("prb_%x%d", time.Now().UnixNano(), os.Getpid())
	}
	return "prb_" + hex.EncodeToString(b[:])
}

// newProbeToken 生成探针长期凭证（明文 gta_prb_* 仅下发一次）与其 SHA-256。
func newProbeToken() (token, tokenHash string, err error) {
	b := make([]byte, 24)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	token = "gta_prb_" + hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}

func joinStrings(xs []string) string {
	return strings.Join(xs, ",")
}

// ---- AuthResolver：把 probe_token 接入 pkg/auth 的 resolver 链 ----

// AuthResolver 按 probes.token_hash 解析探针凭证。
// 解析成功返回 Owner=probe.owner 且 ProbeID=probe_id 的身份。
type AuthResolver struct {
	store ProbeStore
}

// NewAuthResolver 构造探针凭证 resolver（挂入 auth.NewFirstResolver 的 extra）。
func NewAuthResolver(st ProbeStore) *AuthResolver { return &AuthResolver{store: st} }

// Resolve 实现 auth.Resolver。
func (r *AuthResolver) Resolve(token string) (*auth.Principal, bool) {
	if r == nil || r.store == nil || token == "" {
		return nil, false
	}
	sum := sha256.Sum256([]byte(token))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p, err := r.store.GetProbeByTokenHash(ctx, hex.EncodeToString(sum[:]))
	if err != nil {
		return nil, false
	}
	return &auth.Principal{Owner: p.Owner, Tenant: p.TenantID, ProbeID: p.ProbeID}, true
}

// Required 报告探针子系统是否可能持有身份（有任意注册探针即算）。
// 只影响 HTTP 层 401 语义；探针走 gRPC，不依赖它。
func (r *AuthResolver) Required() bool {
	if r == nil || r.store == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	probes, err := r.store.ListProbes(ctx)
	return err == nil && len(probes) > 0
}
