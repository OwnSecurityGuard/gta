// agent_download.go — 远程 agent 下载端点与选项查询。
//
// 让不在同一网络环境的成员也能抓包上报：用户在前端指定「抓包端口 + 解码插件」，
// 服务端为归属当前用户打开一个 agent 接收会话，把回连地址 / token / 会话 id /
// 端口 BPF 烧进二进制（go:embed），再服务端即时编译（-tags "embedded pcap"）下发。
// 终端用户拿到产物直接运行，无需任何命令行参数。
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"gta/pkg/auth"
	pb "gta/pkg/internalipc/proto"
)

// registryIngest 读取 pipeline 实际监听的 registry 地址并推导 ingest（registry 端口 +1，
// 与 gta-agent deriveAddrs 约定一致）。pipeline 不可达时回退默认端口段 :9091/:9092。
func (m *mcpCapture) registryIngest(ctx context.Context) (registry, ingest string) {
	registry = ":9091"
	if m.pipelineClient != nil {
		if resp, err := m.pipelineClient.GetRegistryAddr(ctx, &pb.GetRegistryAddrRequest{}); err == nil && resp.GetRegistryAddr() != "" {
			registry = resp.GetRegistryAddr()
		}
	}
	if host, port := splitHostPort(registry); port != "" && port != "0" {
		ingest = net.JoinHostPort(host, nextPort(port))
	}
	return registry, ingest
}

// splitHostPort 拆 host:port；缺 host 时补回环，缺 port 时返回空 port。
func splitHostPort(addr string) (host, port string) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		if h := strings.Trim(addr, "[]"); h != "" {
			return h, ""
		}
		return "127.0.0.1", ""
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return host, port
}

func nextPort(port string) string {
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n >= 65535 {
		return ""
	}
	return strconv.Itoa(n + 1)
}

// agentBinDir 定位「多平台预置 agent」目录。
// 解析顺序：GTA_AGENT_BIN_DIR 环境变量 → 仓库根的 build/agents。
func (m *mcpCapture) agentBinDir() (string, error) {
	for _, guess := range []string{os.Getenv("GTA_AGENT_BIN_DIR"), filepath.Join(".", "build", "agents")} {
		if guess == "" {
			continue
		}
		if abs, err := filepath.Abs(guess); err == nil {
			if st, err := os.Stat(abs); err == nil && st.IsDir() {
				return abs, nil
			}
		}
	}
	// 目录不存在也返回默认路径（供下载时给出明确“该平台未预置”错误）。
	return func() string {
		abs, _ := filepath.Abs(filepath.Join(".", "build", "agents"))
		return abs
	}(), nil
}

// prebuiltAgentPlatform 是一份已预置（或缺失）的 agent 平台产物。
type prebuiltAgentPlatform struct {
	OS        string `json:"os"`        // windows / linux / darwin
	Arch      string `json:"arch"`      // amd64 / arm64
	Label     string `json:"label"`     // 展示名，如 "Windows x64"
	ExeSuffix bool   `json:"exe"`       // 是否需要 .exe 后缀
	Available bool   `json:"available"` // 该平台产物是否已预置
	Filename  string `json:"filename"`  // 磁盘文件名（含 .exe 时）
}

// prebuiltplatforms 定义下载 agent 支持的目标平台矩阵（按公开顺序）。
func prebuiltPlatforms() []struct {
	OS        string
	Arch      string
	Label     string
	ExeSuffix bool
} {
	return []struct {
		OS        string
		Arch      string
		Label     string
		ExeSuffix bool
	}{
		{"windows", "amd64", "Windows x64", true},
		{"linux", "amd64", "Linux x64", false},
		{"windows", "arm64", "Windows ARM64", true},
		{"linux", "arm64", "Linux ARM64", false},
	}
}

// availableAgentPlatforms 扫描 agentBinDir，返回每份产物及其可用性。
func (m *mcpCapture) availableAgentPlatforms() []prebuiltAgentPlatform {
	binDir, _ := m.agentBinDir()
	out := make([]prebuiltAgentPlatform, 0, 4)
	for _, p := range prebuiltPlatforms() {
		fn := "gta-agent-" + p.OS + "-" + p.Arch
		if p.ExeSuffix {
			fn += ".exe"
		}
		avail := false
		if st, err := os.Stat(filepath.Join(binDir, fn)); err == nil && !st.IsDir() {
			avail = true
		}
		out = append(out, prebuiltAgentPlatform{
			OS:        p.OS,
			Arch:      p.Arch,
			Label:     p.Label,
			ExeSuffix: p.ExeSuffix,
			Available: avail,
			Filename:  fn,
		})
	}
	return out
}

// handleGetAgentDownloadOptions 返回下载 Agent 页面需要的服务端信息：
// 本机可达 IP、registry/ingest 地址与端口，以及可下载的目标平台矩阵。
// 平台可用性以「预置产物是否存在」为准，绝不回落到服务端平台（消除旧方案
// "服务端 Linux → 用户拿到 Linux 二进制" 的缺陷）。
func (m *mcpCapture) handleGetAgentDownloadOptions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	registry, ingest := m.registryIngest(ctx)
	_, registryPort := splitHostPort(registry)
	_, ingestPort := splitHostPort(ingest)
	out := map[string]any{
		"host":           lanIP(),
		"registry_addr":  registry,
		"ingest_addr":    ingest,
		"registry_port":  registryPort,
		"ingest_port":    ingestPort,
		"platforms":      m.availableAgentPlatforms(),
		"message":        "选择目标机器的操作系统下载 Agent。产物按「平台 + 运行时 sidecar 配置」打包为 zip：解压后双击运行 gta-agent(.exe) 即可免参数抓包上报。回连地址端口用 registry 端口（" + registryPort + "）；Agent 会自动把推流口取为 ingest 端口（" + ingestPort + "）。host 需为远端 Agent 可达的网段地址。",
	}
	return successResult(out), nil
}

// serviceBearerToken 从请求中提取当前调用者凭证，用于一并烧进 agent sidecar 配置。
// 优先 Authorization: Bearer，其次 X-GTA-Token，最后兼容回退 query `token`
// （后者会进访问日志，仅作过渡保留）。
func serviceBearerToken(r *http.Request, q url.Values) string {
	if h := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(h, "Bearer ") {
		if t := strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")); t != "" {
			return t
		}
	}
	if h := strings.TrimSpace(r.Header.Get("X-GTA-Token")); h != "" {
		return h
	}
	return strings.TrimSpace(q.Get("token"))
}

// buildAgentZip 把选定的预置平台二进制与该下载对应的 sidecar 配置
// （config.embedded.json）打成 zip。产物解压后，通用 gta-agent 会在运行时
// 读取同目录 config.embedded.json，从而免参数回连服务端、托管插件并抓包。
func buildAgentZip(binPath string, cfgJSON []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	cfgW, err := zw.Create("config.embedded.json")
	if err != nil {
		return nil, err
	}
	if _, err := cfgW.Write(cfgJSON); err != nil {
		return nil, err
	}
	binData, err := os.ReadFile(binPath)
	if err != nil {
		return nil, err
	}
	binW, err := zw.Create(filepath.Base(binPath))
	if err != nil {
		return nil, err
	}
	if _, err := binW.Write(binData); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// handleAgentDownload 是下载 Agent 的 HTTP 端点：
//
//	GET /download/agent?platform=<os>/<arch>&port=&plugin=&server=
//	Authorization: Bearer <token>
//
// 平台取「预置产物」下发（见 availableAgentPlatforms），不再服务端即时编译；
// 会话在下载时创建，session_id 通过响应头 X-Session-Id 回传，供前端自动选中。
// serveAgentBinaryByPlatform 按平台下发预置二进制 zip（占位 config.embedded.json），
// 用于启动码接入：会话已由 GET /access/claim 建立，此处只给二进制，配置由调用方脚本
// 把 claim 返回值写入 config.embedded.json。返回 true 表示已写出响应。
func (m *mcpCapture) serveAgentBinaryByPlatform(w http.ResponseWriter, platform string) bool {
	binDir, _ := m.agentBinDir()
	var binPath string
	for _, p := range m.availableAgentPlatforms() {
		if p.OS+"/"+p.Arch != platform {
			continue
		}
		if !p.Available {
			http.Error(w, "platform "+platform+" is not available; run `make build-agents` on the server to prebuild it", http.StatusNotFound)
			return true
		}
		binPath = filepath.Join(binDir, p.Filename)
		break
	}
	if binPath == "" {
		http.Error(w, "unsupported platform: "+platform, http.StatusBadRequest)
		return true
	}
	zipData, err := buildAgentZip(binPath, []byte("{}"))
	if err != nil {
		http.Error(w, "package agent zip failed: "+err.Error(), http.StatusInternalServerError)
		return true
	}
	zipName := fmt.Sprintf("gta-agent-%s-%s.zip", platform, time.Now().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+zipName+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(zipData); err != nil {
		slog.Warn("stream agent zip failed (code mode)", "platform", platform, "error", err)
	}
	slog.Info("agent binary served (code mode)", "platform", platform)
	return true
}

func (m *mcpCapture) handleAgentDownload(w http.ResponseWriter, r *http.Request) {
	owner := auth.OwnerFrom(r.Context())
	q := r.URL.Query()

	// 启动码接入：只凭 code + platform 下发预置二进制；会话已由 claim 建立，
	// 不复用下方需要 port/server 的普通下载逻辑（避免重复开会话）。
	if code := strings.TrimSpace(q.Get("code")); code != "" {
		m.serveAgentBinaryByPlatform(w, strings.TrimSpace(q.Get("platform")))
		return
	}

	port, err := strconv.Atoi(strings.TrimSpace(q.Get("port")))
	if err != nil || port <= 0 || port > 65535 {
		http.Error(w, "port must be a valid TCP/UDP port", http.StatusBadRequest)
		return
	}
	plugin := strings.TrimSpace(q.Get("plugin"))
	server := strings.TrimSpace(q.Get("server"))
	if server == "" {
		http.Error(w, "server (host:port) is required; use the registry port exposed on this page", http.StatusBadRequest)
		return
	}
	if _, _, err := net.SplitHostPort(server); err != nil {
		http.Error(w, "server must be host:port, e.g. 192.168.1.10:9091", http.StatusBadRequest)
		return
	}
	platform := strings.TrimSpace(q.Get("platform"))
	if platform == "" {
		http.Error(w, "platform (os/arch) is required, e.g. windows/amd64", http.StatusBadRequest)
		return
	}
	token := serviceBearerToken(r, q)

	// 定位目标平台预置产物；缺失即报错，绝不上报服务端自身平台。
	binDir, _ := m.agentBinDir()
	var binPath string
	for _, p := range m.availableAgentPlatforms() {
		if p.OS+"/"+p.Arch != platform {
			continue
		}
		if !p.Available {
			http.Error(w, "platform "+platform+" is not available; run `make build-agents` on the server to prebuild it", http.StatusNotFound)
			return
		}
		binPath = filepath.Join(binDir, p.Filename)
		break
	}
	if binPath == "" {
		http.Error(w, "unsupported platform: "+platform, http.StatusBadRequest)
		return
	}

	if m.pipelineClient == nil {
		http.Error(w, "pipeline is not reachable; cannot open a receive session", http.StatusServiceUnavailable)
		return
	}

	// 1) 在服务端为该用户打开一个 agent 接收会话（包落库后可查询），并把解码插件绑定到会话。
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	grpcReq := &pb.StartCaptureRequest{Plugin: plugin, Agent: true}
	if p, ok := auth.PrincipalFrom(r.Context()); ok {
		grpcReq.Owner = p.Owner
		grpcReq.AllOwners = p.IsAdmin
	}
	resp, err := m.pipelineClient.StartCapture(ctx, grpcReq)
	if err != nil {
		http.Error(w, "open receive session failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	sessionID := resp.GetSessionId()
	meta := sessionMetadata{
		Owner:     owner,
		SessionID: sessionID,
		StartedAt: time.Now().Format(time.RFC3339),
		Status:    "running",
		Port:      port,
		Plugin:    plugin,
		Source:    "agent",
		DBPath:    resp.GetDbPath(),
	}
	if err := m.sessionMgr.writeSessionMetadata(sessionID, meta); err != nil {
		slog.Warn("write session metadata failed during agent download", "session_id", sessionID, "error", err)
	}
	m.sessionMgr.writeCurrent(meta)

	// 2) 端口换算成 BPF，组装 sidecar 配置。
	bpf := fmt.Sprintf("tcp port %d or udp port %d", port, port)
	cfg := map[string]any{
		"server":       server,
		"token":        token,
		"session":      sessionID,
		"bpf":          bpf,
		"plugin_names": []string{},
	}
	if plugin != "" {
		cfg["plugin_names"] = []string{plugin}
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 3) 打包 zip（通用二进制 + sidecar 配置）并下发；session_id 走响应头回传。
	zipData, err := buildAgentZip(binPath, cfgJSON)
	if err != nil {
		http.Error(w, "package agent zip failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	zipName := fmt.Sprintf("gta-agent-%s-%s.zip", platform, time.Now().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+zipName+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Session-Id", sessionID) // 供前端自动选中刚创建的会话
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(zipData); err != nil {
		slog.Warn("stream agent zip failed", "owner", owner, "session_id", sessionID, "error", err)
		return
	}
	slog.Info("agent downloaded", "owner", owner, "platform", platform, "port", port, "plugin", plugin, "server", server, "session_id", sessionID)
}