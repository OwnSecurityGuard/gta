// proxy_lease.go — 代理抓包租约的 MCP 工具。
//
// 租约模式下移动代理抓包按用户/设备创建独立会话：每个租约 = 独立 mobile 抓包会话 +
// 独立 gta-singbox-agent 进程 + 私有 plugin/筛选配置，多用户互不串流、互不抢配置。
// 本文件提供 create_proxy_lease / list_proxy_leases / get_proxy_lease /
// release_proxy_lease 四个工具。返回体额外携带 lan_ip / connect_addr / singbox_uri，
// 供前端渲染扫码连接二维码（手机代理软件填写的 HTTP CONNECT 地址）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"gta/pkg/auth"
	pb "gta/pkg/internalipc/proto"
)

// lanIP 探测本机局域网 IP：
//  1. UDP 拨号到公网地址，取出口网卡 IP（最可靠，无需真实发包）；
//  2. 失败则遍历网卡枚举私有 IPv4。
func lanIP() string {
	if conn, err := net.Dial("udp", "8.8.8.8:80"); err == nil {
		defer conn.Close()
		if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok && addr.IP != nil {
			if ip := addr.IP.To4(); ip != nil {
				return ip.String()
			}
		}
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || !ip4.IsPrivate() {
				continue
			}
			return ip4.String()
		}
	}
	return ""
}

// httpPortOf 从监听地址（如 ":8781" / "0.0.0.0:8781"）提取端口号。
func httpPortOf(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	return strings.TrimPrefix(strings.TrimSpace(addr), ":")
}

// leaseJSON 是返回给前端的租约快照（含 LAN IP 与二维码连接地址）。
type leaseJSON struct {
	LeaseID         string   `json:"lease_id"`
	Owner           string   `json:"owner"`
	ProjectID       string   `json:"project_id"`
	Plugin          string   `json:"plugin"`
	IncludeHosts    []string `json:"include_hosts"`
	IncludePorts    []int32  `json:"include_ports"`
	Device          string   `json:"device"`
	ListenAddr      string   `json:"listen_addr"`
	AgentListenPort int32    `json:"agent_listen_port"`
	MobileGRPCPort  int32    `json:"mobile_grpc_port"`
	AgentRunning    bool     `json:"agent_running"`
	AgentPID        int32    `json:"agent_pid"`
	SessionRunning  bool     `json:"session_running"`
	SessionID       string   `json:"session_id"`
	CreatedAtUnix   int64    `json:"created_at_unix"`
	ActiveConns     int64    `json:"active_conns"`
	TotalConns      uint64   `json:"total_conns"`
	LastDataUnix    int64    `json:"last_data_unix"`
	TotalBytes      uint64   `json:"total_bytes"`
	// LANIP 是手机所在局域网内可达的本机地址；ConnectAddr 为二维码内容
	// （手机代理软件填写的 HTTP CONNECT 代理地址 = LANIP:agent_listen_port）。
	LANIP       string `json:"lan_ip"`
	ConnectAddr string `json:"connect_addr"`
	// SingboxURI 是手机 sing-box 客户端（SFA）可直接扫码导入的远程 profile
	// URI（sing-box://import-remote-profile?url=...#...）。为空时前端回退
	// 到 ConnectAddr 提示手动填写。
	SingboxURI string `json:"singbox_uri"`
}

// singboxProfileURI 构造手机 sing-box 客户端可扫码导入的远程 profile URI。
// agentPort 是该租约 gta-singbox-agent 的 HTTP CONNECT 监听端口，profileURL
// 携带 ?port= 由 /singbox/profile 端点校验租约仍活跃（防陈旧二维码）。
func (m *mcpCapture) singboxProfileURI(lan string, agentPort int32) string {
	httpPort := httpPortOf(m.httpAddr)
	if httpPort == "" {
		return ""
	}
	profileURL := fmt.Sprintf("http://%s:%s/singbox/profile?port=%d", lan, httpPort, agentPort)
	return "sing-box://import-remote-profile?url=" +
		url.QueryEscape(profileURL) + "#" + url.QueryEscape("GTA 代理抓包")
}

// leaseToJSON 把 pipeline 返回的租约状态转为前端 JSON（补充 LAN IP / 连接地址 / 二维码 URI）。
func (m *mcpCapture) leaseToJSON(l *pb.ProxyLeaseState) leaseJSON {
	lan := lanIP()
	connectAddr := ""
	if lan != "" && l.GetAgentListenPort() > 0 {
		connectAddr = net.JoinHostPort(lan, strconv.Itoa(int(l.GetAgentListenPort())))
	}
	singboxURI := ""
	if connectAddr != "" {
		singboxURI = m.singboxProfileURI(lan, l.GetAgentListenPort())
	}
	return leaseJSON{
		LeaseID:         l.GetLeaseId(),
		Owner:           l.GetOwner(),
		ProjectID:       l.GetProjectId(),
		Plugin:          l.GetPlugin(),
		IncludeHosts:    l.GetIncludeHosts(),
		IncludePorts:    l.GetIncludePorts(),
		Device:          l.GetDevice(),
		ListenAddr:      l.GetListenAddr(),
		AgentListenPort: l.GetAgentListenPort(),
		MobileGRPCPort:  l.GetMobileGrpcPort(),
		AgentRunning:    l.GetAgentRunning(),
		AgentPID:        l.GetAgentPid(),
		SessionRunning:  l.GetSessionRunning(),
		SessionID:       l.GetSessionId(),
		CreatedAtUnix:   l.GetCreatedAtUnix(),
		ActiveConns:     l.GetActiveConns(),
		TotalConns:      l.GetTotalConns(),
		LastDataUnix:    l.GetLastDataUnix(),
		TotalBytes:      l.GetTotalBytes(),
		LANIP:           lan,
		ConnectAddr:     connectAddr,
		SingboxURI:      singboxURI,
	}
}

// handleCreateProxyLease 创建一个代理抓包租约：pipeline 分配独立端口、启动独立
// mobile 抓包会话并拉起独立 gta-singbox-agent。返回体含扫码连接地址。
func (m *mcpCapture) handleCreateProxyLease(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	grpcReq := &pb.CreateProxyLeaseRequest{
		Plugin:    req.GetString("plugin", ""),
		Device:    req.GetString("device", ""),
		ProjectId: req.GetString("project_id", ""),
	}
	grpcReq.IncludeHosts = req.GetStringSlice("include_hosts", nil)
	for _, p := range req.GetIntSlice("include_ports", nil) {
		grpcReq.IncludePorts = append(grpcReq.IncludePorts, int32(p))
	}
	// 透传调用方身份：pipeline 记录租约归属并做 owner 作用域过滤。
	if p, ok := auth.PrincipalFrom(ctx); ok {
		grpcReq.Owner = p.Owner
		grpcReq.AllOwners = p.IsAdmin
	}

	resp, err := m.pipelineClient.CreateProxyLease(ctx, grpcReq)
	if err != nil {
		return errorResult(fmt.Errorf("create proxy lease: %w", err)), nil
	}
	lease := m.leaseToJSON(resp.GetLease())
	b, _ := json.MarshalIndent(map[string]any{"ok": true, "lease": lease}, "", "  ")
	slog.Info("create_proxy_lease",
		"lease_id", lease.LeaseID, "owner", lease.Owner, "device", lease.Device,
		"plugin", lease.Plugin, "agent_listen_port", lease.AgentListenPort)
	return mcp.NewToolResultText(string(b)), nil
}

// handleListProxyLeases 列出当前调用方可见的租约（admin 全可见）。
func (m *mcpCapture) handleListProxyLeases(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	grpcReq := &pb.ListProxyLeasesRequest{}
	if p, ok := auth.PrincipalFrom(ctx); ok {
		grpcReq.Owner = p.Owner
		grpcReq.AllOwners = p.IsAdmin
	}

	resp, err := m.pipelineClient.ListProxyLeases(ctx, grpcReq)
	if err != nil {
		return errorResult(fmt.Errorf("list proxy leases: %w", err)), nil
	}
	leases := make([]leaseJSON, 0, len(resp.GetLeases()))
	for _, l := range resp.GetLeases() {
		leases = append(leases, m.leaseToJSON(l))
	}
	b, _ := json.MarshalIndent(map[string]any{"ok": true, "leases": leases}, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}

// handleGetProxyLease 查询单个租约的状态快照（owner 校验，不匹配按不存在处理）。
func (m *mcpCapture) handleGetProxyLease(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	leaseID := req.GetString("lease_id", "")
	if strings.TrimSpace(leaseID) == "" {
		return errorResult(fmt.Errorf("lease_id is required")), nil
	}
	grpcReq := &pb.GetProxyLeaseRequest{LeaseId: leaseID}
	if p, ok := auth.PrincipalFrom(ctx); ok {
		grpcReq.Owner = p.Owner
		grpcReq.AllOwners = p.IsAdmin
	}

	resp, err := m.pipelineClient.GetProxyLease(ctx, grpcReq)
	if err != nil {
		return errorResult(fmt.Errorf("get proxy lease: %w", err)), nil
	}
	lease := m.leaseToJSON(resp.GetLease())
	b, _ := json.MarshalIndent(map[string]any{"ok": true, "lease": lease}, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}

// handleReleaseProxyLease 释放租约：停止会话、终止 agent、回收端口（幂等）。
// 释放后二维码立即失效（/singbox/profile 对该端口返回 404）。
func (m *mcpCapture) handleReleaseProxyLease(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	leaseID := req.GetString("lease_id", "")
	if strings.TrimSpace(leaseID) == "" {
		return errorResult(fmt.Errorf("lease_id is required")), nil
	}
	grpcReq := &pb.ReleaseProxyLeaseRequest{LeaseId: leaseID}
	if p, ok := auth.PrincipalFrom(ctx); ok {
		grpcReq.Owner = p.Owner
		grpcReq.AllOwners = p.IsAdmin
	}

	resp, err := m.pipelineClient.ReleaseProxyLease(ctx, grpcReq)
	if err != nil {
		return errorResult(fmt.Errorf("release proxy lease: %w", err)), nil
	}
	b, _ := json.MarshalIndent(map[string]any{
		"ok":         resp.GetOk(),
		"message":    resp.GetMessage(),
		"session_id": resp.GetSessionId(),
	}, "", "  ")
	slog.Info("release_proxy_lease", "lease_id", leaseID, "ok", resp.GetOk())
	return mcp.NewToolResultText(string(b)), nil
}
