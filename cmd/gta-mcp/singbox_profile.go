// singbox_profile.go — 手机 sing-box 客户端远程 profile 配置端点。
//
// 手机端扫码导入的二维码内容是 sing-box URI（sing-box://import-remote-profile?url=...），
// 其中 url 指向本端点 GET /singbox/profile。SFA 拉取后把返回的完整 sing-box JSON
// 作为 profile 直接运行：手机以 TUN 模式接管流量，TCP 经 HTTP CONNECT 代理转发到
// gta-singbox-agent 监听端口，从而把手机端 tun 流量送进 GTA 代理抓包链路。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	pb "gta/pkg/internalipc/proto"
)

// defaultProxyPort 是 pipeline 不可达时代理监听端口的兜底值，
// 与 cmd/gta-pipeline 的默认 listen 端口保持一致。
const defaultProxyPort = "12000"

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

// proxyListenPort 查询当前代理监听地址的端口；pipeline 不可达时用默认值。
func (m *mcpCapture) proxyListenPort() string {
	if m.pipelineClient == nil {
		return defaultProxyPort
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := m.pipelineClient.GetProxyConfig(ctx, &pb.GetProxyConfigRequest{})
	if err != nil {
		slog.Debug("proxy listen port fallback", "error", err)
		return defaultProxyPort
	}
	if _, p, err := net.SplitHostPort(resp.GetState().GetListenAddr()); err == nil && p != "" {
		return p
	}
	return defaultProxyPort
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
// GET /singbox/profile
func (m *mcpCapture) handleSingboxProfile(w http.ResponseWriter, r *http.Request) {
	server, ok := lanFromHost(r.Host)
	if !ok {
		// 本地/回环访问时回退到探测的局域网 IP，便于浏览器直接测试。
		server = lanIP()
	}
	port, err := strconv.Atoi(m.proxyListenPort())
	if err != nil || server == "" {
		http.Error(w, "cannot determine proxy server address", http.StatusServiceUnavailable)
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
