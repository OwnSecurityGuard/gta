package main

// control_local.go 是探针的本地控制面：回环 HTTP（127.0.0.1:19500，被占自动 +1），
// 给"坐在那台机器前的人/脚本"用。写操作需 Bearer control.token（首启生成，0600）。
//
// 接口与 gta-singbox-agent 的 /v1 语义对齐（docs/plans/2026-09-05 §4.2）：
//
//	GET  /v1/status      三维度状态
//	GET  /v1/health      存活
//	GET  /v1/config      当前配置（token 脱敏）
//	PUT  /v1/config      部分更新（name / server / token / archive.*）
//	GET  /v1/interfaces  pcap 设备清单
//	POST /v1/capture/start  {session_id, iface?, ports?[], hosts?[], bpf?, snaplen?, promisc?}
//	POST /v1/capture/stop
//	POST /v1/capture/filter {ports?[], hosts?[], bpf?}   热更新，不断流
//
// 不监听非回环地址：需要跨机控制走远端控制通道，不放监听。

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gta/pkg/version"
)

// localControl 是本地控制面服务。
type localControl struct {
	runner    *captureRunner
	cfg       *agentConfig
	tokenFile string // control.token 路径
	token     string
	ingest    string // 生效的 ingest 地址（start 用）
	srv       *http.Server
	addr      string
}

func newLocalControl(runner *captureRunner, cfg *agentConfig, ingest string) *localControl {
	lc := &localControl{runner: runner, cfg: cfg, ingest: ingest}
	lc.tokenFile = filepath.Join(configDir(), "control.token")
	if b, err := os.ReadFile(lc.tokenFile); err == nil && len(b) >= 32 {
		lc.token = strings.TrimSpace(string(b))
	} else {
		lc.token = randomHex(24)
		_ = os.MkdirAll(configDir(), 0o700)
		_ = os.WriteFile(lc.tokenFile, []byte(lc.token), 0o600)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", lc.handleStatus)
	mux.HandleFunc("/v1/health", lc.handleHealth)
	mux.HandleFunc("/v1/config", lc.handleConfig)
	mux.HandleFunc("/v1/interfaces", lc.handleInterfaces)
	mux.HandleFunc("/v1/capture/start", lc.handleStart)
	mux.HandleFunc("/v1/capture/stop", lc.handleStop)
	mux.HandleFunc("/v1/capture/filter", lc.handleFilter)
	lc.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return lc
}

// Serve 监听（默认 127.0.0.1:19500，被占自动 +1，最多试 20 次）。
func (lc *localControl) Serve(ctx context.Context, addr string) error {
	var lis net.Listener
	var err error
	for i := 0; i < 20; i++ {
		lis, err = net.Listen("tcp", addr)
		if err == nil {
			break
		}
		host, port, perr := net.SplitHostPort(addr)
		if perr != nil {
			break
		}
		n, _ := strconv.Atoi(port)
		addr = net.JoinHostPort(host, strconv.Itoa(n+1))
	}
	if err != nil {
		return fmt.Errorf("listen local control: %w", err)
	}
	lc.addr = lis.Addr().String()
	// 端口文件：本地脚本/运维从固定位置取真实端口。
	_ = os.WriteFile(filepath.Join(configDir(), "control.port"), []byte(lc.addr), 0o600)
	slog.Info("local control listening", "addr", lc.addr)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = lc.srv.Shutdown(shutdownCtx)
	}()
	if err := lc.srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// authorize 写操作校验：回环端口对本机任意进程可达，没有 token 的控制面等于裸奔。
func (lc *localControl) authorize(r *http.Request) bool {
	if !isLoopback(r.RemoteAddr) {
		return false
	}
	v := r.Header.Get("Authorization")
	v = strings.TrimPrefix(v, "Bearer ")
	return v != "" && v == lc.token
}

func isLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (lc *localControl) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSONLocal(w, http.StatusOK, map[string]any{"ok": true})
}

func (lc *localControl) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSONLocal(w, http.StatusOK, map[string]any{"ok": true, "status": lc.statusSnapshot()})
}

// statusSnapshot 组装三维度状态（本地 /v1/status 与写操作回显共用）。
func (lc *localControl) statusSnapshot() map[string]any {
	state, sessionID, iface, portsCSV, lastErr, updatedMs := lc.runner.State()
	d := lc.runner.Data()
	return map[string]any{
			"probe_id":    lc.cfg.ProbeID,
			"name":        lc.cfg.Name,
			"version":     version.String(),
			"control_addr": lc.addr,
			"ingest_addr": lc.ingest,
			"registered":  lc.cfg.ProbeID != "" && lc.cfg.ProbeToken != "",
			"capture": map[string]any{
				"state": state, "session_id": sessionID, "iface": iface,
				"ports": portsCSV, "error": lastErr, "updated_unix_ms": updatedMs,
			},
		"data": map[string]any{
			"last_packet_unix_ms": d.LastPacketMs, "last_upload_unix_ms": d.LastUploadMs,
			"packets_captured": d.PacketsCaptured, "packets_acked": d.PacketsAcked,
			"spool_depth": d.SpoolDepth, "dropped": d.Dropped,
		},
	}
}

func (lc *localControl) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cfg := *lc.cfg
		cfg.ProbeToken = maskSecret(cfg.ProbeToken)
		cfg.UserToken = maskSecret(cfg.UserToken)
		writeJSONLocal(w, http.StatusOK, map[string]any{"ok": true, "config": cfg})
		return
	}
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !lc.authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var patch map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if errStr := lc.applyConfigPatch(patch); errStr != "" {
		writeJSONLocal(w, http.StatusBadRequest, map[string]any{"ok": false, "error": errStr})
		return
	}
	writeJSONLocal(w, http.StatusOK, map[string]any{"ok": true})
}

// applyConfigPatch 应用白名单键（与远端 SetConfig 同一白名单，复用 ControlAgent 的校验逻辑）。
// 这里做的是本地等价实现（saveAgentConfig + 字段映射），避免为复用而引入循环依赖。
func (lc *localControl) applyConfigPatch(patch map[string]any) string {
	get := func(k string) (string, bool) {
		v, ok := patch[k]
		if !ok {
			return "", false
		}
		s, ok := v.(string)
		return s, ok
	}
	if v, ok := get("name"); ok {
		lc.cfg.Name = v
	}
	if v, ok := get("server"); ok {
		lc.cfg.Server = v
		// server 变更需要重启进程（回连地址是长连接的根基），只落盘不热生效。
	}
	if v, ok := get("archive_enabled"); ok {
		switch v {
		case "true", "1", "on":
			lc.cfg.Archive.Enabled = true
		case "false", "0", "off":
			lc.cfg.Archive.Enabled = false
		default:
			return "archive_enabled: invalid value"
		}
	}
	if v, ok := get("archive_max_age_hours"); ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return "archive_max_age_hours: invalid value"
		}
		lc.cfg.Archive.MaxAgeHrs = n
	}
	if v, ok := get("archive_max_bytes"); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return "archive_max_bytes: invalid value"
		}
		lc.cfg.Archive.MaxBytes = n
	}
	for k := range patch {
		switch k {
		case "name", "server", "archive_enabled", "archive_max_age_hours", "archive_max_bytes":
		default:
			return fmt.Sprintf("config key %q not allowed", k)
		}
	}
	if err := saveAgentConfig(lc.cfg); err != nil {
		return err.Error()
	}
	return ""
}

func (lc *localControl) handleInterfaces(w http.ResponseWriter, r *http.Request) {
	ifaces := listInterfacesLocal()
	writeJSONLocal(w, http.StatusOK, map[string]any{"ok": true, "interfaces": ifaces})
}

func (lc *localControl) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !lc.authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		SessionID string   `json:"session_id"`
		Iface     string   `json:"iface"`
		Ports     []int32  `json:"ports"`
		Hosts     []string `json:"hosts"`
		BPF       string   `json:"bpf"`
		SnapLen   int32    `json:"snaplen"`
		Promisc   bool     `json:"promisc"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Promisc == false && req.SnapLen == 0 && req.SessionID == "" {
		// 区分"没填"与"填了 false"：默认混杂开启（与命令行 --promisc 默认一致）。
		req.Promisc = true
	}
	iface := req.Iface
	if iface == "" {
		resolved, err := resolveDefaultIface()
		if err != nil {
			writeJSONLocal(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		iface = resolved
	}
	err := lc.runner.Start(CaptureParams{
		SessionID: req.SessionID, Iface: iface, Ports: req.Ports,
		Hosts: req.Hosts, BPF: req.BPF, SnapLen: req.SnapLen, Promisc: req.Promisc,
	}, lc.ingest, lc.cfg.ProbeToken)
	if err != nil {
		writeJSONLocal(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	lc.writeStatus(w)
}

func (lc *localControl) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !lc.authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := lc.runner.Stop(); err != nil {
		writeJSONLocal(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	lc.writeStatus(w)
}

func (lc *localControl) handleFilter(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !lc.authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Ports []int32  `json:"ports"`
		Hosts []string `json:"hosts"`
		BPF   string   `json:"bpf"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := lc.runner.UpdateFilter(req.Ports, req.Hosts, req.BPF); err != nil {
		writeJSONLocal(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	lc.writeStatus(w)
}

func (lc *localControl) writeStatus(w http.ResponseWriter) {
	writeJSONLocal(w, http.StatusOK, map[string]any{"ok": true, "status": lc.statusSnapshot()})
}

func maskSecret(s string) string {
	if len(s) <= 6 {
		return ""
	}
	return s[:6] + "..."
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x%d", b, time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}

func writeJSONLocal(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
