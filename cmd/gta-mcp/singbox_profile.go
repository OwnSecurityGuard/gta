// singbox_profile.go — 手机 sing-box 客户端远程 profile 配置端点。
//
// 手机端扫码导入的二维码内容是 sing-box URI（sing-box://import-remote-profile?url=...），
// 其中 url 指向本端点 GET /singbox/profile?port=<租约的 agent 监听端口>。SFA 拉取后
// 把返回的完整 sing-box JSON 作为 profile 直接运行：手机以 TUN 模式接管流量，
// TCP 经 HTTP CONNECT 代理转发到该租约 gta-singbox-agent 的监听端口，从而把手机端
// tun 流量送进 GTA 租约会话（按用户/设备隔离，互不串流）。
//
// port 参数必填且必须对应一个活跃租约：租约已释放时返回 404（防陈旧二维码），
// pipeline 不可达时返回 503。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	pb "gta/pkg/internalipc/proto"
)

// lanFromHost 从 Host 头提取可达的 IPv4 主机地址（排除端口）。
// 手机访问本端点时 Host 即二维码中填写的 LAN IP。回环地址视为不可达。
func lanFromHost(host string) (string, bool) {
	h := host
	if hp, _, err := net.SplitHostPort(host); err == nil {
		h = hp
	}
	ip := net.ParseIP(h)
	if ip == nil || ip.IsLoopback() || ip.To4() == nil {
		return "", false
	}
	return ip.String(), true
}

// leasePortActive 校验 port 是否对应一个活跃租约的 agent 监听端口。
// pipeline 不可达返回 err（调用方回 503）；无匹配租约返回 found=false（调用方回 404）。
func (m *mcpCapture) leasePortActive(ctx context.Context, port int) (found bool, err error) {
	if m.pipelineClient == nil {
		return false, fmt.Errorf("pipeline client not available")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := m.pipelineClient.ListProxyLeases(ctx, &pb.ListProxyLeasesRequest{AllOwners: true})
	if err != nil {
		return false, err
	}
	for _, l := range resp.GetLeases() {
		if int(l.GetAgentListenPort()) == port {
			return true, nil
		}
	}
	return false, nil
}

// buildSingboxConfig 生成手机 sing-box 客户端可运行的完整配置：
// TUN 入站接管手机流量，出站经 HTTP CONNECT 代理转发到 agent，
// DNS 直连本地，最终路由走代理。
func buildSingboxConfig(server string, port int) map[string]any {
	return map[string]any{
		"log": map[string]any{
			"level":     "info",
			"timestamp": true,
		},
		"dns": map[string]any{
			"servers": []any{
				// sing-box 1.12+ 移除了 legacy 格式（"address": "local"），
				// 必须使用 type 字段声明 DNS 服务器类型。
				map[string]any{"type": "local", "tag": "local"},
			},
			"final": "local",
		},
		"inbounds": []any{
			map[string]any{
				"type":           "tun",
				"tag":            "tun-in",
				"interface_name": "gta0",
				"mtu":            9000,
				"address":        []string{"172.19.0.1/30"},
				"auto_route":     true,
				"strict_route":   false,
				"stack":          "gvisor",
			},
		},
		"outbounds": []any{
			map[string]any{
				"type":        "http",
				"tag":         "proxy",
				"server":      server,
				"server_port": port,
			},
			map[string]any{"type": "direct", "tag": "direct"},
		},
		"route": map[string]any{
			"final":                 "proxy",
			"auto_detect_interface": true,
			"rules": []any{
				// sniff / hijack-dns 是 sing-box 1.11+ 的 rule action，
				// 替代 legacy 的 inbound.sniff 与 dns 特殊出站（1.13.0 移除）。
				map[string]any{"action": "sniff"},
				map[string]any{"protocol": "dns", "action": "hijack-dns"},
			},
		},
	}
}

// handleSingboxProfile 输出手机 sing-box 客户端可导入的远程 profile 配置。
// GET /singbox/profile?port=<agent_listen_port>
// port 必填：租约二维码携带的 agent 监听端口；对应租约不活跃时 404（防陈旧二维码）。
func (m *mcpCapture) handleSingboxProfile(w http.ResponseWriter, r *http.Request) {
	portStr := strings.TrimSpace(r.URL.Query().Get("port"))
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		http.Error(w, "missing or invalid required query param: port (the proxy lease's agent listen port)", http.StatusBadRequest)
		return
	}
	server, ok := lanFromHost(r.Host)
	if !ok {
		// 本地/回环访问时回退到探测的局域网 IP，便于浏览器直接测试。
		server = lanIP()
	}
	if server == "" {
		http.Error(w, "cannot determine proxy server address", http.StatusServiceUnavailable)
		return
	}
	found, err := m.leasePortActive(r.Context(), port)
	if err != nil {
		slog.Warn("singbox profile: lease lookup failed", "port", port, "error", err)
		http.Error(w, "pipeline unavailable", http.StatusServiceUnavailable)
		return
	}
	if !found {
		http.Error(w, fmt.Sprintf("no active proxy lease listening on port %d (released?)", port), http.StatusNotFound)
		return
	}
	cfg := buildSingboxConfig(server, port)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		slog.Error("encode singbox profile", "error", err)
		http.Error(w, fmt.Sprintf("encode failed: %v", err), http.StatusInternalServerError)
		return
	}
	slog.Info("served singbox profile", "server", server, "port", port, "remote", r.RemoteAddr)
}
