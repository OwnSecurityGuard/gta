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

// lanIPOverride 由 main() 注入：显式指定本机的 LAN IP，绕过启发式探测。
// 推荐用法：手机所在局域网内的宿主机 IPv4，例如 192.168.1.10。
// 缺省时 lanIP() 走启发式，跳过 docker bridge / Hyper-V / WSL 等虚拟网卡的 IP。
var lanIPOverride string

// lanIP 返回本机局域网 IPv4（用于二维码里的 host:port）。
//
// 顺序：
//  1. 显式覆盖（-lan-ip flag / GTA_LAN_IP env）——避免启发式在 docker / WSL
//     等场景给出不可达的虚拟网卡 IP；
//  2. UDP 拨号到公网地址取出口 IP（最可靠，无需真实发包）；
//     —— 若命中 docker bridge / cgnat / link-local 等内嵌虚拟网段则丢弃，继续 3；
//  3. 枚举网卡按 InterfaceAlias 过滤 docker/hyperv/wsl/vmware/virtual 后
//     取首个私有 IPv4；
//  4. 全部失败返回空（前端回退显示 connect_addr 仅带端口）。
func lanIP() string {
	if v := strings.TrimSpace(lanIPOverride); v != "" {
		if ip := net.ParseIP(v); ip != nil {
			if ip4 := ip.To4(); ip4 != nil && !ip4.IsLoopback() {
				return ip4.String()
			}
		}
		slog.Warn("invalid -lan-ip override, falling back to heuristic",
			"value", lanIPOverride)
	}
	if conn, err := net.Dial("udp", "8.8.8.8:80"); err == nil {
		defer conn.Close()
		if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok && addr.IP != nil {
			if ip := addr.IP.To4(); ip != nil && isReachableHostLANIP(ip) {
				return ip.String()
			}
		}
	}
	if ip := lanIPFromInterfaces(); ip != "" {
		return ip
	}
	return ""
}

// isReachableHostLANIP 判定一个 IPv4 是不是手机所在的 LAN 范围内可达的地址。
// 拒绝 docker bridge / cgnat / link-local 等内嵌虚拟网段；这些地址通常
// 属于容器网络或私有中转，手机根本够不到，却常被简单启发式错选。
func isReachableHostLANIP(ip net.IP) bool {
	// 注意：net.ParseIP 返回的 IP 可能是 16-byte（IPv4-mapped IPv6）的形式。
	// 直接读 ip[0..3] 在该形态下读到的是 ::ffff:0:0 段，永远命中不到 172.x，
	// 这里必须先 To4() 归一到 4-byte 才能用 RFC1918 / CGNAT 段判断。
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	if v4.IsLoopback() || v4.IsLinkLocalUnicast() || v4.IsLinkLocalMulticast() {
		return false
	}
	// 172.16.0.0/12 是 RFC1918，但 docker 默认（docker0/172.17/172.18）
	// 与 hyper-v internal switch 常落此段；从启发式选择里剔除。
	// 若用户宿主机 LAN 段恰好也是 172.16/12 私网地址，请显式 -lan-ip 覆盖。
	if v4[0] == 172 && v4[1]&0xf0 == 16 {
		return false
	}
	// 100.64.0.0/10 是运营商 CGNAT 段，一般不是宿主机 LAN。
	if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return false
	}
	return true
}

// lanIPFromInterfaces 枚举网卡，按 InterfaceAlias 排除虚拟适配器后
// 取首个私有 IPv4（10/8、192.168/16、172.16/12，含 docker 这类内嵌设备）。
// 仅在 UDP 拨号给的 IP 被过滤掉时调用——意味着宿主 LAN 段必须经这里认领。
func lanIPFromInterfaces() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if !isWindowsHostInterfaceName(iface.Name) {
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

// isWindowsHostInterfaceName 判定一个 net.Interface 是否像宿主物理网卡 /
// 默认 Hyper-V switch（Windows 上 InterfaceAlias 会进 Name）。
// 用于 LAN 探测的二次过滤——把 docker bridge / WSL / VMware 这些一看就不是
// 宿主 LAN 的虚拟设备剔除。
//
// 返回 true 即视为「可能是宿主 LAN」，需要继续往下抓私有 IPv4。
// 默认实现不过滤（Windows 解析失败时也能跑）。
var isWindowsHostInterfaceName = func(ifaceName string) bool {
	n := strings.ToLower(ifaceName)
	// 明显的虚拟/容器/虚拟化设备关键字：跳过。
	// 命中其一即剔除（避免 docker bridge / WSL / Hyper-V 等被启发式错选）。
	bads := []string{
		"docker",     // DockerNAT, vEthernet (DockerNAT), Docker Host
		"hyper-v",
		"hyperv",
		"wsl",        // WSL 桥接
		"vmware",     // VMware Network Adapter
		"virtualbox", // VirtualBox Host-Only
		"vbox",
		"hns",        // Windows Host Network Service（容器网络）
		"vethernet",  // Hyper-V 虚拟交换机（HNS）—— 一律剔除避免误选
		"tailscale",
		"zerotier",
	}
	for _, b := range bads {
		if strings.Contains(n, b) {
			return false
		}
	}
	return true
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
	ControlPort     int32    `json:"control_port"`
	AgentRunning    bool     `json:"agent_running"`
	AgentPID        int32    `json:"agent_pid"`
	SessionRunning  bool     `json:"session_running"`
	SessionID       string   `json:"session_id"`
	CreatedAtUnix   int64    `json:"created_at_unix"`
	ActiveConns     int64    `json:"active_conns"`
	TotalConns      uint64   `json:"total_conns"`
	LastDataUnix    int64    `json:"last_data_unix"`
	TotalBytes      uint64   `json:"total_bytes"`
	// CaptureRunning 是 agent 当前是否在推数据。idle 时 false，
	// StartLeaseCapture 后转 true，StopLeaseCapture 后回到 false。
	CaptureRunning bool `json:"capture_running"`
	// CaptureCount 是本租约累计开始过多少次抓包（用于 UI 显示「开了 N 次」）。
	CaptureCount int32 `json:"capture_count"`
	// LastCaptureAtUnix 最近一次 start/stop 的 unix 秒，0 = 从未。
	LastCaptureAtUnix int64 `json:"last_capture_at_unix"`
	// StickyPort 为 true 时本端口是 (owner, device) 复用端口——二维码长期有效。
	StickyPort bool `json:"sticky_port"`
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
		LeaseID:           l.GetLeaseId(),
		Owner:             l.GetOwner(),
		ProjectID:         l.GetProjectId(),
		Plugin:            l.GetPlugin(),
		IncludeHosts:      l.GetIncludeHosts(),
		IncludePorts:      l.GetIncludePorts(),
		Device:            l.GetDevice(),
		ListenAddr:        l.GetListenAddr(),
		AgentListenPort:   l.GetAgentListenPort(),
		MobileGRPCPort:    l.GetMobileGrpcPort(),
		ControlPort:       l.GetControlPort(),
		AgentRunning:      l.GetAgentRunning(),
		AgentPID:          l.GetAgentPid(),
		SessionRunning:    l.GetSessionRunning(),
		SessionID:         l.GetSessionId(),
		CreatedAtUnix:     l.GetCreatedAtUnix(),
		ActiveConns:       l.GetActiveConns(),
		TotalConns:        l.GetTotalConns(),
		LastDataUnix:      l.GetLastDataUnix(),
		TotalBytes:        l.GetTotalBytes(),
		CaptureRunning:    l.GetCaptureRunning(),
		CaptureCount:      l.GetCaptureCount(),
		LastCaptureAtUnix: l.GetLastCaptureAtUnix(),
		StickyPort:        l.GetStickyPort(),
		LANIP:             lan,
		ConnectAddr:       connectAddr,
		SingboxURI:        singboxURI,
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

// handleStartLeaseCapture 在已有租约上开启新一轮抓包：分配新的 mobile gRPC 端口、
// 启动独立的 mobile 会话、通过 agent 控制接口让它把数据推到本会话。
// 手机 QR/代理连接全程不动——出口与手机 VPN 始终保持。
func (m *mcpCapture) handleStartLeaseCapture(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	leaseID := req.GetString("lease_id", "")
	if strings.TrimSpace(leaseID) == "" {
		return errorResult(fmt.Errorf("lease_id is required")), nil
	}
	grpcReq := &pb.StartLeaseCaptureRequest{
		LeaseId: leaseID,
		Plugin:  req.GetString("plugin", ""),
	}
	grpcReq.IncludeHosts = req.GetStringSlice("include_hosts", nil)
	for _, p := range req.GetIntSlice("include_ports", nil) {
		grpcReq.IncludePorts = append(grpcReq.IncludePorts, int32(p))
	}
	if p, ok := auth.PrincipalFrom(ctx); ok {
		grpcReq.Owner = p.Owner
		grpcReq.AllOwners = p.IsAdmin
	}

	resp, err := m.pipelineClient.StartLeaseCapture(ctx, grpcReq)
	if err != nil {
		return errorResult(fmt.Errorf("start lease capture: %w", err)), nil
	}
	out := map[string]any{
		"ok":         resp.GetOk(),
		"message":    resp.GetMessage(),
		"session_id": resp.GetSessionId(),
		"lease":      m.leaseToJSON(resp.GetLease()),
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	slog.Info("start_lease_capture",
		"lease_id", leaseID, "session_id", resp.GetSessionId(),
		"mobile_grpc", resp.GetLease().GetMobileGrpcPort())
	return mcp.NewToolResultText(string(b)), nil
}

// handleStopLeaseCapture 停止租约当前的抓包会话并让 lease 回到 idle。
// 出口与手机连接全保留，后续可再次 start_lease_capture。
func (m *mcpCapture) handleStopLeaseCapture(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	leaseID := req.GetString("lease_id", "")
	if strings.TrimSpace(leaseID) == "" {
		return errorResult(fmt.Errorf("lease_id is required")), nil
	}
	grpcReq := &pb.StopLeaseCaptureRequest{LeaseId: leaseID}
	if p, ok := auth.PrincipalFrom(ctx); ok {
		grpcReq.Owner = p.Owner
		grpcReq.AllOwners = p.IsAdmin
	}

	resp, err := m.pipelineClient.StopLeaseCapture(ctx, grpcReq)
	if err != nil {
		return errorResult(fmt.Errorf("stop lease capture: %w", err)), nil
	}
	out := map[string]any{
		"ok":          resp.GetOk(),
		"message":     resp.GetMessage(),
		"session_id":  resp.GetSessionId(),
		"raw_packets": resp.GetRawPackets(),
		"events":      resp.GetEvents(),
		"duration_s":  resp.GetDurationSec(),
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	slog.Info("stop_lease_capture",
		"lease_id", leaseID, "session_id", resp.GetSessionId(),
		"raw", resp.GetRawPackets(), "events", resp.GetEvents())
	return mcp.NewToolResultText(string(b)), nil
}
