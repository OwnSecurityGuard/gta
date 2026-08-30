// agent_download.go — 远程 agent 下载端点与选项查询。
//
// 让不在同一网络环境的成员也能抓包上报：用户在前端指定「抓包端口 + 解码插件」，
// 服务端为归属当前用户打开一个 agent 接收会话，把回连地址 / token / 会话 id /
// 端口 BPF 烧进二进制（go:embed），再服务端即时编译（-tags "embedded pcap"）下发。
// 终端用户拿到产物直接运行，无需任何命令行参数。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// handleGetAgentDownloadOptions 返回下载 Agent 页面需要的服务端信息：
// 本机可达 IP、registry/ingest 地址与端口、服务端平台（仅支持服务端本机平台抓包）。
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
		"platform":       runtime.GOOS + "/" + runtime.GOARCH,
		"message":        "在下载页填写 Agent 回连地址时，端口用 registry 端口（" + registryPort + "）；Agent 会自动把推流口取为 ingest 端口（" + ingestPort + "）。host 需为远端 Agent 可达的网段地址（局域网 IP 或公网地址）。",
	}
	return successResult(out), nil
}

// agentSrcDir 定位 cmd/gta-agent 源码目录（编译下载产物用）。
// 解析顺序：GTA_AGENT_SRC_DIR 环境变量 → CWD 下 ./cmd/gta-agent。
func (m *mcpCapture) agentSrcDir() (string, error) {
	for _, guess := range []string{os.Getenv("GTA_AGENT_SRC_DIR"), filepath.Join(".", "cmd", "gta-agent")} {
		if guess == "" {
			continue
		}
		if abs, err := filepath.Abs(guess); err == nil {
			if st, err := os.Stat(abs); err == nil && st.IsDir() {
				return abs, nil
			}
		}
	}
	return "", errors.New("cannot locate gta-agent source directory (set GTA_AGENT_SRC_DIR to <repo>/cmd/gta-agent)")
}

// buildAgentBinary 在服务端即时编译下载形态的 gta-agent：把 cfgJSON 写入
// cmd/gta-agent/config.embedded.json，以 -tags "embedded pcap" 构建，返回二进制临时路径。
// 整段（写配置文件 + 编译 + 清理）用 m.buildMu 串行化，避免并发下载互相覆盖 go:embed 文件。
// 只支持服务端本机平台：pcap 依赖 cgo，跨系统交叉编译不可行。
func (m *mcpCapture) buildAgentBinary(buildCtx context.Context, cfgJSON []byte) (string, error) {
	m.buildMu.Lock()
	defer m.buildMu.Unlock()

	srcDir, err := m.agentSrcDir()
	if err != nil {
		return "", err
	}
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	outPath := filepath.Join(os.TempDir(), fmt.Sprintf("gta-agent-%s-%s-%d%s",
		runtime.GOOS, runtime.GOARCH, time.Now().UnixNano(), ext))

	cfgPath := filepath.Join(srcDir, "config.embedded.json")
	if err := os.WriteFile(cfgPath, cfgJSON, 0o644); err != nil {
		return "", fmt.Errorf("write embedded config: %w", err)
	}
	defer func() { _ = os.Remove(cfgPath) }()

	// srcDir 形如 <repo>/cmd/gta-agent；往上逐级找 go.mod 定位 repo 根，
	// 使 `go build ./cmd/gta-agent` 在该根下能解析 gta 模块。
	repoDir := filepath.Dir(srcDir)
	for {
		if _, err := os.Stat(filepath.Join(repoDir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(repoDir)
		if parent == repoDir {
			_ = os.Remove(cfgPath)
			return "", errors.New("cannot find repo root (go.mod) above gta-agent source dir")
		}
		repoDir = parent
	}

	cmd := exec.CommandContext(buildCtx, "go", "build", "-tags", "embedded pcap", "-o", outPath, "./cmd/gta-agent")
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(outPath)
		_ = os.Remove(cfgPath)
		return "", fmt.Errorf("compile agent failed (server requires Go toolchain + libpcap,pcap build tags): %v\n%s", err, out)
	}
	return outPath, nil
}

// handleAgentDownload 是下载 Agent 的 HTTP 端点：GET /download/agent?port=&plugin=&server=&token=
// 处于鉴权链内，访问者即会话 owner；token 由前端传入其当前凭证，一并烧进二进制。
func (m *mcpCapture) handleAgentDownload(w http.ResponseWriter, r *http.Request) {
	owner := auth.OwnerFrom(r.Context())
	q := r.URL.Query()

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
	token := strings.TrimSpace(q.Get("token"))

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

	// 2) 端口换算成 BPF，组装固化配置并编译。
	bpf := fmt.Sprintf("tcp port %d or udp port %d", port, port)
	// 插件按名字白名单烧入：agent 侧只托管用户选定的插件（空则不托管/不解码，由服务端会话负责解码处理）。
	cfg := map[string]any{
		"server":      server,
		"token":       token,
		"session":     sessionID,
		"bpf":         bpf,
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
	binPath, err := m.buildAgentBinary(r.Context(), cfgJSON)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.Remove(binPath)

	// 3) 流式下发二进制。
	f, err := os.Open(binPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	filename := filepath.Base(binPath)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, filename, time.Now(), f)
	slog.Info("agent downloaded", "owner", owner, "port", port, "plugin", plugin, "server", server, "session_id", sessionID)
}