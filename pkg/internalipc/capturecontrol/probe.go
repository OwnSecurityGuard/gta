package capturecontrol

// probe.go 是 CaptureControl 的探针管理 RPC（docs/plans/2026-09-05 §6/§8）。
//
// 信任边界与 owner/all_owners 语义同 ListPlugins：gta-mcp 从 HTTP auth ctx
// 透传调用方身份，本层只做「creator 轴」过滤与校验（探针是个人资源，
// 仅注册者本人可见可用；all_owners=true 仅 admin 透传）。
// 实际控制逻辑全部委托 pkg/probe.Manager（desired-state 模型）。

import (
	"context"
	"errors"
	"fmt"
	"time"

	pb "gta/pkg/internalipc/proto"
	"gta/pkg/probe"
	"gta/pkg/store"
)

// ProbeAdmin 是探针管理面需要的最小接口（pkg/probe.Manager 满足它）。
type ProbeAdmin interface {
	List(ctx context.Context) ([]store.ProbeMeta, error)
	Get(ctx context.Context, probeID string) (*store.ProbeMeta, error)
	StartCapture(ctx context.Context, probeID string, d probe.Desired) error
	StopCapture(ctx context.Context, probeID string) (string, error)
	UpdateFilter(ctx context.Context, probeID string, ports []int32, hosts []string) error
	Retry(ctx context.Context, probeID string) error
	Rename(ctx context.Context, probeID, name string) error
	Revoke(ctx context.Context, probeID string) error
	ListArchiveCached(ctx context.Context, probeID string, fromMs, toMs int64) ([]store.ArchiveSegmentMeta, error)
	UpsertArchiveSegments(ctx context.Context, probeID string, segs []store.ArchiveSegmentMeta) error
	StartArchiveUpload(ctx context.Context, probeID, targetSession string, fromMs, toMs int64) error
}

// SetProbeAdmin 注入探针管理面（nil = 探针管理未启用，RPC 返回错误）。
func (s *Server) SetProbeAdmin(pa ProbeAdmin) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probes = pa
}

// probeAdmin 返回注入的管理面（未启用时 error）。
func (s *Server) probeAdmin() (ProbeAdmin, error) {
	s.mu.Lock()
	pa := s.probes
	s.mu.Unlock()
	if pa == nil {
		return nil, errors.New("probe management is not enabled on this pipeline")
	}
	return pa, nil
}

// authorizeProbe creator 轴校验：非 admin 只能看到/操作自己注册的探针。
// 返回 nil 表示放行。
func authorizeProbe(p *store.ProbeMeta, owner string, allOwners bool) error {
	if allOwners {
		return nil
	}
	if owner == "" {
		// 匿名语境（本地直连 pipeline 的 CLI）：保持宽松，与既有 owner 语义一致。
		return nil
	}
	if p.Owner != owner {
		return fmt.Errorf("probe %s belongs to another user", p.ProbeID)
	}
	return nil
}

// probeInfoToProto 把 ProbeMeta（已合并在线态）转为 pb.ProbeInfo。
func probeInfoToProto(p *store.ProbeMeta) *pb.ProbeInfo {
	return &pb.ProbeInfo{
		ProbeId:          p.ProbeID,
		Name:             p.Name,
		Owner:            p.Owner,
		TenantId:         p.TenantID,
		Capabilities:     p.Capabilities,
		Version:          p.Version,
		Hostname:         p.Hostname,
		Os:               p.OS,
		Arch:             p.Arch,
		ConnectionState:  p.ConnectionState,
		LastSeenAt:       formatTime(p.LastSeenAt),
		CaptureState:     p.CaptureState,
		LastSessionId:    p.LastSessionID,
		StatusError:      p.StatusError,
		CaptureIface:     p.CaptureIface,
		CapturePorts:     p.CapturePorts,
		LastPacketUnixMs: p.LastPacketMs,
		LastUploadUnixMs: p.LastUploadMs,
		PacketsCaptured:  p.PacketsCaptured,
		PacketsAcked:     p.PacketsAcked,
		SpoolDepth:       p.SpoolDepth,
		Dropped:          p.Dropped,
		ArchiveBytes:     p.ArchiveBytes,
		ArchiveSegments:  p.ArchiveSegments,
		ArchiveOldestUnix: p.ArchiveOldestMs / 1000,
		ArchiveNewestUnix: p.ArchiveNewestMs / 1000,
		CreatedAt:        formatTime(p.CreatedAt),
	}
}

// ListProbes 处理探针列表 RPC（creator 轴过滤）。
func (s *Server) ListProbes(ctx context.Context, req *pb.ListProbesRequest) (*pb.ListProbesResponse, error) {
	pa, err := s.probeAdmin()
	if err != nil {
		return nil, err
	}
	probes, err := pa.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*pb.ProbeInfo, 0, len(probes))
	for i := range probes {
		if err := authorizeProbe(&probes[i], req.GetOwner(), req.GetAllOwners()); err != nil {
			continue // 不是自己的探针：直接不出现在列表里
		}
		out = append(out, probeInfoToProto(&probes[i]))
	}
	return &pb.ListProbesResponse{Probes: out}, nil
}

// GetProbe 处理单个探针查询 RPC。
func (s *Server) GetProbe(ctx context.Context, req *pb.GetProbeRequest) (*pb.GetProbeResponse, error) {
	pa, err := s.probeAdmin()
	if err != nil {
		return nil, err
	}
	p, err := pa.Get(ctx, req.GetProbeId())
	if err != nil {
		return nil, err
	}
	if err := authorizeProbe(p, req.GetOwner(), req.GetAllOwners()); err != nil {
		return nil, err
	}
	return &pb.GetProbeResponse{Probe: probeInfoToProto(p)}, nil
}

// ProbeStartCapture 选定探针建会话并下发 AssignCapture（建会话 + 指派一体）。
// 会话是用户任务：以 agent-only source 创建（订阅 agent hub，探针推流进来），
// 归属调用方；抓包参数经 desired 下发，探针在线即对齐。
func (s *Server) ProbeStartCapture(ctx context.Context, req *pb.ProbeStartCaptureRequest) (*pb.ProbeStartCaptureResponse, error) {
	pa, err := s.probeAdmin()
	if err != nil {
		return nil, err
	}
	p, err := pa.Get(ctx, req.GetProbeId())
	if err != nil {
		return nil, err
	}
	if err := authorizeProbe(p, req.GetOwner(), req.GetAllOwners()); err != nil {
		return nil, err
	}
	if p.ConnectionState != "online" {
		return nil, fmt.Errorf("probe %s is offline (last seen %s)", p.ProbeID, p.LastSeenAt)
	}

	// 1) 建会话（agentOnly；owner=调用方=探针注册者，推流归属校验天然通过）。
	ctx = withRequestOwner(ctx, req.GetOwner(), req.GetAllOwners())
	started, err := s.engine.StartSession(ctx, StartSessionRequest{
		Plugin:       req.GetPlugin(),
		Port:         int(firstPort(req.GetPorts())),
		Agent:        true,
		ProjectID:    req.GetProjectId(),
		PluginOwners: req.GetPluginOwners(),
	})
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	// 2) 设定 desired 并对齐探针。会话建了但探针不在线（窗口期掉线）：
	// desired 已记录，探针重连后会自动对齐，不回滚会话（用户可手动停止）。
	d := probe.Desired{
		SessionID: started.SessionID,
		Iface:     req.GetIface(),
		Ports:     req.GetPorts(),
		Hosts:     req.GetHosts(),
		SnapLen:   1600,
		Promisc:   true,
	}
	if err := pa.StartCapture(ctx, req.GetProbeId(), d); err != nil {
		// 指派失败：回滚会话，避免留下一个永远等不到数据的空会话。
		_, _ = s.engine.StopSession(ctx, started.SessionID)
		return nil, fmt.Errorf("assign capture to probe: %w", err)
	}
	return &pb.ProbeStartCaptureResponse{SessionId: started.SessionID, DbPath: started.DBPath}, nil
}

// ProbeStopCapture 停止探针抓包并结束会话。未在抓包时 ok 返回空 session_id。
func (s *Server) ProbeStopCapture(ctx context.Context, req *pb.ProbeStopCaptureRequest) (*pb.ProbeStopCaptureResponse, error) {
	pa, err := s.probeAdmin()
	if err != nil {
		return nil, err
	}
	p, err := pa.Get(ctx, req.GetProbeId())
	if err != nil {
		return nil, err
	}
	if err := authorizeProbe(p, req.GetOwner(), req.GetAllOwners()); err != nil {
		return nil, err
	}
	sessionID, err := pa.StopCapture(ctx, req.GetProbeId())
	if err != nil {
		return nil, err
	}
	if sessionID != "" {
		// 结束会话（finalizeTask 会联动 OnSessionClosed → 探针 Stop，幂等）。
		if _, err := s.engine.StopSession(ctx, sessionID); err != nil {
			return nil, err
		}
	}
	return &pb.ProbeStopCaptureResponse{SessionId: sessionID}, nil
}

// ProbeUpdateFilter 热更新抓包过滤（BPF 重编译，不中断抓包）。
func (s *Server) ProbeUpdateFilter(ctx context.Context, req *pb.ProbeUpdateFilterRequest) (*pb.ProbeUpdateFilterResponse, error) {
	pa, err := s.probeAdmin()
	if err != nil {
		return nil, err
	}
	p, err := pa.Get(ctx, req.GetProbeId())
	if err != nil {
		return nil, err
	}
	if err := authorizeProbe(p, req.GetOwner(), req.GetAllOwners()); err != nil {
		return nil, err
	}
	if err := pa.UpdateFilter(ctx, req.GetProbeId(), req.GetPorts(), req.GetHosts()); err != nil {
		return &pb.ProbeUpdateFilterResponse{Ok: false}, nil
	}
	return &pb.ProbeUpdateFilterResponse{Ok: true}, nil
}

// ProbeRetryCapture 让 failed 状态的探针重试上一次 assign。
func (s *Server) ProbeRetryCapture(ctx context.Context, req *pb.ProbeRetryCaptureRequest) (*pb.ProbeRetryCaptureResponse, error) {
	pa, err := s.probeAdmin()
	if err != nil {
		return nil, err
	}
	p, err := pa.Get(ctx, req.GetProbeId())
	if err != nil {
		return nil, err
	}
	if err := authorizeProbe(p, req.GetOwner(), req.GetAllOwners()); err != nil {
		return nil, err
	}
	if err := pa.Retry(ctx, req.GetProbeId()); err != nil {
		return &pb.ProbeRetryCaptureResponse{Ok: false}, nil
	}
	return &pb.ProbeRetryCaptureResponse{Ok: true}, nil
}

// ProbeRename 改探针显示名。
func (s *Server) ProbeRename(ctx context.Context, req *pb.ProbeRenameRequest) (*pb.ProbeRenameResponse, error) {
	pa, err := s.probeAdmin()
	if err != nil {
		return nil, err
	}
	p, err := pa.Get(ctx, req.GetProbeId())
	if err != nil {
		return nil, err
	}
	if err := authorizeProbe(p, req.GetOwner(), req.GetAllOwners()); err != nil {
		return nil, err
	}
	if err := pa.Rename(ctx, req.GetProbeId(), req.GetName()); err != nil {
		return &pb.ProbeRenameResponse{Ok: false}, nil
	}
	return &pb.ProbeRenameResponse{Ok: true}, nil
}

// ProbeRevoke 作废 probe_token（探针下次启动需重新接入）。
func (s *Server) ProbeRevoke(ctx context.Context, req *pb.ProbeRevokeRequest) (*pb.ProbeRevokeResponse, error) {
	pa, err := s.probeAdmin()
	if err != nil {
		return nil, err
	}
	p, err := pa.Get(ctx, req.GetProbeId())
	if err != nil {
		return nil, err
	}
	if err := authorizeProbe(p, req.GetOwner(), req.GetAllOwners()); err != nil {
		return nil, err
	}
	if err := pa.Revoke(ctx, req.GetProbeId()); err != nil {
		return &pb.ProbeRevokeResponse{Ok: false}, nil
	}
	return &pb.ProbeRevokeResponse{Ok: true}, nil
}

// ProbeListArchive 查询探针本地归档段。refresh=true 且探针在线时实时查询并刷新缓存；
// 探针离线时退化为缓存（from_cache=true 提示可能过期）。
func (s *Server) ProbeListArchive(ctx context.Context, req *pb.ProbeListArchiveRequest) (*pb.ProbeListArchiveResponse, error) {
	pa, err := s.probeAdmin()
	if err != nil {
		return nil, err
	}
	p, err := pa.Get(ctx, req.GetProbeId())
	if err != nil {
		return nil, err
	}
	if err := authorizeProbe(p, req.GetOwner(), req.GetAllOwners()); err != nil {
		return nil, err
	}
	fromMs, toMs := req.GetFromUnix()*1000, req.GetToUnix()*1000
	if req.GetRefresh() && p.ConnectionState == "online" {
		segs, qerr := s.queryProbeArchive(ctx, pa, req.GetProbeId(), fromMs, toMs)
		if qerr == nil {
			return &pb.ProbeListArchiveResponse{Segments: segMetaToProto(segs), FromCache: false}, nil
		}
		// 实时查询失败（离线/超时）：落回缓存。
	}
	segs, err := pa.ListArchiveCached(ctx, req.GetProbeId(), fromMs, toMs)
	if err != nil {
		return nil, err
	}
	return &pb.ProbeListArchiveResponse{Segments: segMetaToProto(segs), FromCache: true}, nil
}

// queryProbeArchive 实时查询并刷新缓存；探针离线返回错误。
func (s *Server) queryProbeArchive(ctx context.Context, pa ProbeAdmin, probeID string, fromMs, toMs int64) ([]store.ArchiveSegmentMeta, error) {
	q, ok := pa.(interface {
		QueryArchive(ctx context.Context, probeID string, fromMs, toMs int64) ([]store.ArchiveSegmentMeta, error)
	})
	if !ok {
		return nil, errors.New("archive query unsupported")
	}
	segs, err := q.QueryArchive(ctx, probeID, fromMs, toMs)
	if err != nil {
		return nil, err
	}
	_ = pa.UpsertArchiveSegments(ctx, probeID, segs)
	return segs, nil
}

// ProbeImportArchive 把探针本地归档按时间窗回放导入为新会话。
// 流程：建 agent-only 会话（owner=调用方）→ 令探针回放推流 → 返回会话 id。
// 服务端落 source=agent 的新会话，不回填老会话。
func (s *Server) ProbeImportArchive(ctx context.Context, req *pb.ProbeImportArchiveRequest) (*pb.ProbeImportArchiveResponse, error) {
	pa, err := s.probeAdmin()
	if err != nil {
		return nil, err
	}
	p, err := pa.Get(ctx, req.GetProbeId())
	if err != nil {
		return nil, err
	}
	if err := authorizeProbe(p, req.GetOwner(), req.GetAllOwners()); err != nil {
		return nil, err
	}
	if p.ConnectionState != "online" {
		return nil, fmt.Errorf("probe %s is offline; archive import requires the probe to be online", p.ProbeID)
	}

	ctx = withRequestOwner(ctx, req.GetOwner(), req.GetAllOwners())
	// 会话来源标记（设计 §8.3）：source=probe-archive + 探针与时间窗，
	// 落 sessions.extra 列，供列表展示"探针归档"徽标与导入溯源。
	started, err := s.engine.StartSession(ctx, StartSessionRequest{
		Agent:     true,
		ProjectID: req.GetProjectId(),
		Metadata: map[string]string{
			"source":     "probe-archive",
			"probe_id":   p.ProbeID,
			"probe_name": p.Name,
			"from_unix":  fmt.Sprintf("%d", req.GetFromUnix()),
			"to_unix":    fmt.Sprintf("%d", req.GetToUnix()),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create import session: %w", err)
	}
	fromMs, toMs := req.GetFromUnix()*1000, req.GetToUnix()*1000
	if err := pa.StartArchiveUpload(ctx, req.GetProbeId(), started.SessionID, fromMs, toMs); err != nil {
		_, _ = s.engine.StopSession(ctx, started.SessionID)
		return nil, fmt.Errorf("start archive upload: %w", err)
	}
	return &pb.ProbeImportArchiveResponse{SessionId: started.SessionID, DbPath: started.DBPath}, nil
}

// segMetaToProto 归档段元数据转 proto。
func segMetaToProto(segs []store.ArchiveSegmentMeta) []*pb.ProbeArchiveSegmentMeta {
	out := make([]*pb.ProbeArchiveSegmentMeta, 0, len(segs))
	for _, s := range segs {
		out = append(out, &pb.ProbeArchiveSegmentMeta{
			SegId:     s.SegID,
			FirstUnix: s.FirstMs / 1000,
			LastUnix:  s.LastMs / 1000,
			Packets:   s.Packets,
			Bytes:     s.Bytes,
			LinkType:  s.LinkType,
		})
	}
	return out
}

// firstPort 返回端口列表的第一个（会话记录的展示端口；0 = 未指定）。
func firstPort(ports []int32) int32 {
	if len(ports) == 0 {
		return 0
	}
	return ports[0]
}

// formatTime RFC3339 格式化（零值返回空串，UI 显示"从未"）。
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
