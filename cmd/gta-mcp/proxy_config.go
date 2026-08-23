// proxy_config.go — 代理抓包服务器配置的 MCP 工具。
//
// 新架构下代理抓包不再需要手动"开始抓包"：pipeline 启动即拉起常驻代理会话与
// gta-singbox-agent（sing-box server 常驻等待手机代理连接）。本文件提供
// get_proxy_server_config / update_proxy_server_config 两个工具，供前端"服务器配置"
// 页面读取/修改配置并生成二维码。get 额外返回 LAN IP 与 connect_addr，用于二维码内容。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

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

// proxyStateJSON 是返回给前端的状态快照（含 LAN IP 与二维码连接地址）。
type proxyStateJSON struct {
	ListenAddr     string `json:"listen_addr"`
	ServerAddr     string `json:"server_addr"`
	FrameStyle     string `json:"frame_style"`
	PrefixLen      int32  `json:"prefix_len"`
	LittleEndian   bool   `json:"little_endian"`
	AgentRunning   bool   `json:"agent_running"`
	AgentPID       int32  `json:"agent_pid"`
	SessionRunning bool   `json:"session_running"`
	SessionID      string `json:"session_id"`
	ConfigPath     string `json:"config_path"`
	Plugin         string `json:"plugin"`
	IncludeHosts   []string `json:"include_hosts"`
	IncludePorts   []int32  `json:"include_ports"`
	// LANIP 是手机所在局域网内可达的代理地址；connect_addr 为二维码内容
	// （手机代理软件填写的 HTTP CONNECT 代理地址）。
	LANIP       string `json:"lan_ip"`
	ConnectAddr string `json:"connect_addr"`
	// SingboxURI 是手机 sing-box 客户端（SFA）可直接扫码导入的远程 profile
	// URI（sing-box://import-remote-profile?url=...#...）。为空时前端回退
	// 到 ConnectAddr 提示手动填写。
	SingboxURI string `json:"singbox_uri"`
}

// httpPortOf 从监听地址（如 ":8781" / "0.0.0.0:8781"）提取端口号。
func httpPortOf(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	return strings.TrimPrefix(strings.TrimSpace(addr), ":")
}

// singboxProfileURI 构造手机 sing-box 客户端可扫码导入的远程 profile URI。
// profileURL 指向本进程的 /singbox/profile 端点，SFA 拉取后作为完整配置运行，
// 使手机以 TUN 模式把流量经 HTTP CONNECT 代理转发到 agent。
func (m *mcpCapture) singboxProfileURI(lan string) string {
	httpPort := httpPortOf(m.httpAddr)
	if httpPort == "" {
		return ""
	}
	profileURL := fmt.Sprintf("http://%s:%s/singbox/profile", lan, httpPort)
	return "sing-box://import-remote-profile?url=" +
		url.QueryEscape(profileURL) + "#" + url.QueryEscape("GTA 代理抓包")
}

func (m *mcpCapture) stateToJSON(st *pb.ProxyConfigState) proxyStateJSON {
	lan := lanIP()
	connectAddr := ""
	if st.GetListenAddr() != "" {
		port := ""
		if _, p, err := net.SplitHostPort(st.GetListenAddr()); err == nil {
			port = p
		}
		if lan != "" {
			connectAddr = net.JoinHostPort(lan, port)
		} else {
			connectAddr = st.GetListenAddr()
		}
	}
	singboxURI := ""
	if lan != "" {
		singboxURI = m.singboxProfileURI(lan)
	}
	return proxyStateJSON{
		ListenAddr:     st.GetListenAddr(),
		ServerAddr:     st.GetServerAddr(),
		FrameStyle:     st.GetFrameStyle(),
		PrefixLen:      st.GetPrefixLen(),
		LittleEndian:   st.GetLittleEndian(),
		AgentRunning:   st.GetAgentRunning(),
		AgentPID:       st.GetAgentPid(),
		SessionRunning: st.GetSessionRunning(),
		SessionID:      st.GetSessionId(),
		ConfigPath:     st.GetConfigPath(),
		Plugin:         st.GetPlugin(),
		IncludeHosts:   st.GetIncludeHosts(),
		IncludePorts:   st.GetIncludePorts(),
		LANIP:          lan,
		ConnectAddr:    connectAddr,
		SingboxURI:     singboxURI,
	}
}

// handleGetProxyServerConfig 返回当前代理抓包服务器配置 + 运行时状态 + LAN IP + 二维码连接地址。
func (m *mcpCapture) handleGetProxyServerConfig(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	resp, err := m.pipelineClient.GetProxyConfig(ctx, &pb.GetProxyConfigRequest{})
	if err != nil {
		return errorResult(fmt.Errorf("get proxy config: %w", err)), nil
	}
	st := m.stateToJSON(resp.GetState())
	b, _ := json.MarshalIndent(map[string]any{"ok": true, "state": st}, "", "  ")
	slog.Info("get_proxy_server_config", "state", b)
	return mcp.NewToolResultText(string(b)), nil
}

// handleUpdateProxyServerConfig 应用新的代理抓包服务器配置。
// 空字段表示不修改；listen_addr 为空时保留现有值。
// include_hosts/include_ports 未传表示不修改；传空数组表示清空筛选。
func (m *mcpCapture) handleUpdateProxyServerConfig(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	listenAddr := req.GetString("listen_addr", "")
	serverAddr := req.GetString("server_addr", "")
	frameStyle := req.GetString("frame_style", "")
	prefixLen, _ := req.RequireInt("prefix_len")
	littleEndian := strings.EqualFold(req.GetString("little_endian", "false"), "true")
	plugin := req.GetString("plugin", "")

	// 区分"未传"（nil，不修改）与"传空数组"（空 slice，清空筛选）。
	var includeHosts []string
	if argHas(req, "include_hosts") {
		includeHosts = req.GetStringSlice("include_hosts", nil)
	}
	var includePorts []int32
	if argHas(req, "include_ports") {
		for _, p := range req.GetIntSlice("include_ports", nil) {
			includePorts = append(includePorts, int32(p))
		}
	}

	resp, err := m.pipelineClient.UpdateProxyConfig(ctx, &pb.UpdateProxyConfigRequest{
		ListenAddr:   listenAddr,
		ServerAddr:   serverAddr,
		FrameStyle:   frameStyle,
		PrefixLen:    int32(prefixLen),
		LittleEndian: littleEndian,
		Plugin:       plugin,
		IncludeHosts: includeHosts,
		IncludePorts: includePorts,
	})
	if err != nil {
		return errorResult(fmt.Errorf("update proxy config: %w", err)), nil
	}
	st := m.stateToJSON(resp.GetState())
	b, _ := json.MarshalIndent(map[string]any{
		"ok":      resp.GetOk(),
		"message": resp.GetMessage(),
		"state":   st,
	}, "", "  ")
	slog.Info("update_proxy_server_config",
		"ok", resp.GetOk(), "message", resp.GetMessage(),
		"listen_addr", listenAddr, "server_addr", serverAddr, "frame_style", frameStyle,
		"plugin", plugin, "include_hosts", includeHosts, "include_ports", includePorts)
	return mcp.NewToolResultText(string(b)), nil
}

// argHas 判断请求参数中是否显式提供了 key（用于区分"未传"与"传空数组"）。
func argHas(req mcp.CallToolRequest, key string) bool {
	args := req.GetArguments()
	_, ok := args[key]
	return ok
}
