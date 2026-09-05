// probe_tools.go — 探针管理的 MCP 工具（v2 探针优化，docs/plans/2026-09-05 §6/§8）。
//
// 探针是基础设施：一级页面是 Sessions（probe_start_capture 就是"创建抓包"），
// 本文件的工具同时服务会话创建（probe_start_capture）与管理页（list/rename/revoke）。
// 身份透传与 owner 语义同 proxy_lease.go：pipeline 侧做 creator 轴校验。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"gametrace/pkg/auth"
	pb "gametrace/pkg/internalipc/proto"
)

// probeToJSON 把 pipeline 返回的 ProbeInfo 转为前端 JSON（snake_case）。
func probeToJSON(p *pb.ProbeInfo) map[string]any {
	return map[string]any{
		"probe_id":            p.GetProbeId(),
		"name":                p.GetName(),
		"owner":               p.GetOwner(),
		"tenant_id":           p.GetTenantId(),
		"capabilities":        p.GetCapabilities(),
		"version":             p.GetVersion(),
		"hostname":            p.GetHostname(),
		"os":                  p.GetOs(),
		"arch":                p.GetArch(),
		"connection_state":    p.GetConnectionState(),
		"last_seen_at":        p.GetLastSeenAt(),
		"capture_state":       p.GetCaptureState(),
		"last_session_id":     p.GetLastSessionId(),
		"status_error":        p.GetStatusError(),
		"capture_iface":       p.GetCaptureIface(),
		"capture_ports":       p.GetCapturePorts(),
		"last_packet_unix_ms": p.GetLastPacketUnixMs(),
		"last_upload_unix_ms": p.GetLastUploadUnixMs(),
		"packets_captured":    p.GetPacketsCaptured(),
		"packets_acked":       p.GetPacketsAcked(),
		"spool_depth":         p.GetSpoolDepth(),
		"dropped":             p.GetDropped(),
		"archive_bytes":       p.GetArchiveBytes(),
		"archive_segments":    p.GetArchiveSegments(),
		"archive_oldest_unix": p.GetArchiveOldestUnix(),
		"archive_newest_unix": p.GetArchiveNewestUnix(),
		"created_at":          p.GetCreatedAt(),
	}
}

// fillOwner 透传调用方身份（owner/all_owners）到 pipeline 请求。
func fillOwner(ctx context.Context, setOwner func(owner string, allOwners bool)) {
	if p, ok := auth.PrincipalFrom(ctx); ok {
		setOwner(p.Owner, p.IsAdmin)
	}
}

// writeProbeSessionMeta 探针链路建会话后写本地 metadata.json（与 start_capture
// 同一模式）：list_all_sessions 以文件系统元数据为列表源，不写则导入/探针会话
// 在 UI 列表不可见。current=true 时同时写 current.json（服务端默认会话语义）。
func (m *mcpCapture) writeProbeSessionMeta(ctx context.Context, sessionID, dbPath, source, plugin string, port int, extra map[string]any, current bool) {
	meta := sessionMetadata{
		Owner:     auth.OwnerFrom(ctx),
		SessionID: sessionID,
		StartedAt: time.Now().Format(time.RFC3339),
		Status:    "running",
		Port:      port,
		Plugin:    plugin,
		Source:    source,
		DBPath:    dbPath,
		Extra:     extra,
	}
	if err := m.sessionMgr.writeSessionMetadata(sessionID, meta); err != nil {
		slog.Warn("write probe session metadata failed", "session_id", sessionID, "error", err)
	}
	if current {
		m.sessionMgr.writeCurrent(meta)
	}
}

// handleListProbes 列出调用方可见的探针（creator 轴过滤；admin 全量）。
func (m *mcpCapture) handleListProbes(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	grpcReq := &pb.ListProbesRequest{}
	fillOwner(ctx, func(o string, a bool) { grpcReq.Owner, grpcReq.AllOwners = o, a })
	resp, err := m.pipelineClient.ListProbes(ctx, grpcReq)
	if err != nil {
		return errorResult(fmt.Errorf("list probes: %w", err)), nil
	}
	probes := make([]map[string]any, 0, len(resp.GetProbes()))
	for _, p := range resp.GetProbes() {
		probes = append(probes, probeToJSON(p))
	}
	b, _ := json.MarshalIndent(map[string]any{"ok": true, "probes": probes}, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}

// handleGetProbe 查询单个探针的三维度状态。
func (m *mcpCapture) handleGetProbe(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	grpcReq := &pb.GetProbeRequest{ProbeId: req.GetString("probe_id", "")}
	fillOwner(ctx, func(o string, a bool) { grpcReq.Owner, grpcReq.AllOwners = o, a })
	resp, err := m.pipelineClient.GetProbe(ctx, grpcReq)
	if err != nil {
		return errorResult(fmt.Errorf("get probe: %w", err)), nil
	}
	b, _ := json.MarshalIndent(map[string]any{"ok": true, "probe": probeToJSON(resp.GetProbe())}, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}

// handleProbeStartCapture 选定探针创建抓包会话（Sessions 一级页的"创建抓包"）：
// 建会话 + AssignCapture 一体，返回 session_id 后用 get_session_status 轮询三态。
func (m *mcpCapture) handleProbeStartCapture(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	grpcReq := &pb.ProbeStartCaptureRequest{
		ProbeId:   req.GetString("probe_id", ""),
		Iface:     req.GetString("iface", ""),
		Plugin:    req.GetString("plugin", ""),
		ProjectId: req.GetString("project_id", ""),
	}
	grpcReq.Hosts = req.GetStringSlice("hosts", nil)
	for _, p := range req.GetIntSlice("ports", nil) {
		grpcReq.Ports = append(grpcReq.Ports, int32(p))
	}
	fillOwner(ctx, func(o string, a bool) {
		grpcReq.Owner, grpcReq.AllOwners = o, a
		grpcReq.PluginOwners = m.pluginOwnersFor(ctx, o)
	})
	resp, err := m.pipelineClient.ProbeStartCapture(ctx, grpcReq)
	if err != nil {
		return errorResult(fmt.Errorf("probe start capture: %w", err)), nil
	}
	port := 0
	if len(grpcReq.Ports) > 0 {
		port = int(grpcReq.Ports[0])
	}
	m.writeProbeSessionMeta(ctx, resp.GetSessionId(), resp.GetDbPath(), "probe",
		grpcReq.Plugin, port, map[string]any{"probe_id": grpcReq.ProbeId}, true)
	b, _ := json.MarshalIndent(map[string]any{
		"ok": true, "session_id": resp.GetSessionId(), "probe_id": grpcReq.ProbeId,
	}, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}

// handleProbeStopCapture 停止探针抓包并结束会话。
func (m *mcpCapture) handleProbeStopCapture(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	grpcReq := &pb.ProbeStopCaptureRequest{ProbeId: req.GetString("probe_id", "")}
	fillOwner(ctx, func(o string, a bool) { grpcReq.Owner, grpcReq.AllOwners = o, a })
	resp, err := m.pipelineClient.ProbeStopCapture(ctx, grpcReq)
	if err != nil {
		return errorResult(fmt.Errorf("probe stop capture: %w", err)), nil
	}
	b, _ := json.MarshalIndent(map[string]any{"ok": true, "session_id": resp.GetSessionId()}, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}

// handleProbeUpdateFilter 热更新探针抓包过滤（不中断抓包）。
func (m *mcpCapture) handleProbeUpdateFilter(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	grpcReq := &pb.ProbeUpdateFilterRequest{ProbeId: req.GetString("probe_id", "")}
	grpcReq.Hosts = req.GetStringSlice("hosts", nil)
	for _, p := range req.GetIntSlice("ports", nil) {
		grpcReq.Ports = append(grpcReq.Ports, int32(p))
	}
	fillOwner(ctx, func(o string, a bool) { grpcReq.Owner, grpcReq.AllOwners = o, a })
	resp, err := m.pipelineClient.ProbeUpdateFilter(ctx, grpcReq)
	if err != nil {
		return errorResult(fmt.Errorf("probe update filter: %w", err)), nil
	}
	b, _ := json.MarshalIndent(map[string]any{"ok": resp.GetOk()}, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}

// handleProbeRetryCapture 让 failed 的探针重试上一次 assign。
func (m *mcpCapture) handleProbeRetryCapture(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	grpcReq := &pb.ProbeRetryCaptureRequest{ProbeId: req.GetString("probe_id", "")}
	fillOwner(ctx, func(o string, a bool) { grpcReq.Owner, grpcReq.AllOwners = o, a })
	resp, err := m.pipelineClient.ProbeRetryCapture(ctx, grpcReq)
	if err != nil {
		return errorResult(fmt.Errorf("probe retry capture: %w", err)), nil
	}
	b, _ := json.MarshalIndent(map[string]any{"ok": resp.GetOk()}, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}

// handleProbeRename 改探针显示名。
func (m *mcpCapture) handleProbeRename(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	grpcReq := &pb.ProbeRenameRequest{
		ProbeId: req.GetString("probe_id", ""),
		Name:    req.GetString("name", ""),
	}
	fillOwner(ctx, func(o string, a bool) { grpcReq.Owner, grpcReq.AllOwners = o, a })
	resp, err := m.pipelineClient.ProbeRename(ctx, grpcReq)
	if err != nil {
		return errorResult(fmt.Errorf("probe rename: %w", err)), nil
	}
	b, _ := json.MarshalIndent(map[string]any{"ok": resp.GetOk()}, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}

// handleProbeRevoke 作废探针凭证（探针下次启动需重新接入）。
func (m *mcpCapture) handleProbeRevoke(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	grpcReq := &pb.ProbeRevokeRequest{ProbeId: req.GetString("probe_id", "")}
	fillOwner(ctx, func(o string, a bool) { grpcReq.Owner, grpcReq.AllOwners = o, a })
	resp, err := m.pipelineClient.ProbeRevoke(ctx, grpcReq)
	if err != nil {
		return errorResult(fmt.Errorf("probe revoke: %w", err)), nil
	}
	b, _ := json.MarshalIndent(map[string]any{"ok": resp.GetOk()}, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}

// handleProbeListArchive 查询探针本地归档段（refresh=true 时探针在线则实时查询）。
func (m *mcpCapture) handleProbeListArchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	grpcReq := &pb.ProbeListArchiveRequest{
		ProbeId:   req.GetString("probe_id", ""),
		FromUnix:  int64(req.GetFloat("from_unix", 0)),
		ToUnix:    int64(req.GetFloat("to_unix", 0)),
		Refresh:   req.GetBool("refresh", false),
	}
	fillOwner(ctx, func(o string, a bool) { grpcReq.Owner, grpcReq.AllOwners = o, a })
	resp, err := m.pipelineClient.ProbeListArchive(ctx, grpcReq)
	if err != nil {
		return errorResult(fmt.Errorf("probe list archive: %w", err)), nil
	}
	segments := make([]map[string]any, 0, len(resp.GetSegments()))
	for _, s := range resp.GetSegments() {
		segments = append(segments, map[string]any{
			"seg_id":     s.GetSegId(),
			"first_unix": s.GetFirstUnix(),
			"last_unix":  s.GetLastUnix(),
			"packets":    s.GetPackets(),
			"bytes":      s.GetBytes(),
			"link_type":  s.GetLinkType(),
		})
	}
	b, _ := json.MarshalIndent(map[string]any{
		"ok": true, "segments": segments, "from_cache": resp.GetFromCache(),
	}, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}

// handleProbeImportArchive 把探针本地归档按时间窗回放导入为新会话。
func (m *mcpCapture) handleProbeImportArchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	grpcReq := &pb.ProbeImportArchiveRequest{
		ProbeId:   req.GetString("probe_id", ""),
		FromUnix:  int64(req.GetFloat("from_unix", 0)),
		ToUnix:    int64(req.GetFloat("to_unix", 0)),
		ProjectId: req.GetString("project_id", ""),
	}
	fillOwner(ctx, func(o string, a bool) { grpcReq.Owner, grpcReq.AllOwners = o, a })
	resp, err := m.pipelineClient.ProbeImportArchive(ctx, grpcReq)
	if err != nil {
		return errorResult(fmt.Errorf("probe import archive: %w", err)), nil
	}
	// source=probe-archive：与 pipeline 侧 sessions.extra 的标记一致（溯源用）。
	m.writeProbeSessionMeta(ctx, resp.GetSessionId(), resp.GetDbPath(), "probe-archive",
		"", 0, map[string]any{
			"probe_id":  grpcReq.ProbeId,
			"from_unix": grpcReq.FromUnix,
			"to_unix":   grpcReq.ToUnix,
		}, false)
	b, _ := json.MarshalIndent(map[string]any{
		"ok": true, "session_id": resp.GetSessionId(), "probe_id": grpcReq.ProbeId,
	}, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}
