// proxy_server.go — 代理抓包服务器常驻管理。
//
// 架构：手机代理软件 ── HTTP CONNECT ──▶ gta-singbox-agent ── gRPC ──▶ mobile Source（常驻会话）
//
// pipeline 启动即拉起常驻代理抓包会话（mobile Source，监听 ServerAddr），并按需自动拉起
// gta-singbox-agent（--spawn-agent，默认 true），使 sing-box server 常驻等待手机代理连接。
// 配置经 GetProxyConfig/UpdateProxyConfig 查询与热更新：持久化到 <workdir>/proxy.json，
// 随后热重启 agent + 重启常驻会话，使新配置即时生效。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"gta/pkg/capture"
	"gta/pkg/config"
	"gta/pkg/internalipc/capturecontrol"
)

// agentProcess 管理自动拉起的 gta-singbox-agent 子进程生命周期。
// 默认在 pipeline 启动时拉起（--spawn-agent），使 sing-box server 常驻等待手机代理连接；
// 配置更新时热重启（kill + respawn），pipeline 退出时同步终止。
type agentProcess struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
}

// running 返回子进程是否存活（未启动/已退出返回 false）。
func (ap *agentProcess) running() bool {
	if ap == nil {
		return false
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	return ap.cmd != nil && ap.cmd.Process != nil && ap.cmd.ProcessState == nil
}

// pid 返回子进程 PID（未启动返回 0）。
func (ap *agentProcess) pid() int {
	if ap == nil {
		return 0
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	if ap.cmd != nil && ap.cmd.Process != nil {
		return ap.cmd.Process.Pid
	}
	return 0
}

// stop 终止子进程（幂等）。
func (ap *agentProcess) stop() {
	if ap == nil {
		return
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	if ap.cmd != nil && ap.cmd.Process != nil {
		_ = ap.cmd.Process.Kill()
	}
}

// spawnSingboxAgent 以纯 HTTP CONNECT 代理模式拉起 gta-singbox-agent。
// bin 为空时在 <workdir>/bin 下查找默认二进制；二进制缺失或启动失败时告警并返回 nil。
// cfg 的筛选列表（include_hosts/include_ports）非空时透传 --filter-hosts/--filter-ports。
func spawnSingboxAgent(workDir, bin string, cfg config.ProxyServerConfig) *agentProcess {
	if strings.TrimSpace(bin) == "" {
		exe := "gta-singbox-agent"
		if runtime.GOOS == "windows" {
			exe += ".exe"
		}
		bin = filepath.Join(workDir, "bin", exe)
	}
	if _, err := os.Stat(bin); err != nil {
		slog.Warn("singbox agent binary not found, skip auto-spawn (install via `make build-agent`)", "bin", bin, "error", err)
		return nil
	}
	args := []string{"--server", cfg.ServerAddr, "--listen", cfg.ListenAddr}
	if hosts := cfg.FilterHostList(); hosts != "" {
		args = append(args, "--filter-hosts", hosts)
	}
	if ports := cfg.FilterPortList(); ports != "" {
		args = append(args, "--filter-ports", ports)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		slog.Warn("spawn singbox agent failed", "bin", bin, "error", err)
		return nil
	}
	ap := &agentProcess{cmd: cmd, done: make(chan struct{})}
	slog.Info("singbox agent spawned (always-on proxy listener)",
		"bin", bin, "listen", cfg.ListenAddr, "server", cfg.ServerAddr,
		"filter_hosts", cfg.FilterHostList(), "filter_ports", cfg.FilterPortList(),
		"pid", cmd.Process.Pid)
	go func() {
		err := cmd.Wait()
		if err != nil {
			slog.Warn("singbox agent exited", "pid", cmd.Process.Pid, "error", err)
		}
		close(ap.done)
	}()
	return ap
}

// StartAlwaysOnProxy 在 pipeline 启动时调用：加载持久化配置、按需拉起 agent、
// 启动常驻代理抓包会话。任一失败只告警，不阻断 pipeline 启动。
func (s *pipelineService) StartAlwaysOnProxy(spawnAgent bool, agentBin string) {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()

	s.proxyPath = config.ProxyConfigPath(s.workDir)
	s.spawnAgent = spawnAgent
	s.agentBin = agentBin

	// T11：proxy.json 未指定 server_addr 时，回退到统一配置（gta.yaml
	// proxy.server_addr / GTA_PROXY_SERVER_ADDR），最后才是硬编码默认值。
	cfg, err := config.LoadProxyServerConfigWithDefault(s.proxyPath, s.proxyServerAddrOverride)
	if err != nil {
		s.logger.Warn("load proxy config failed, using defaults", "error", err)
	} else {
		s.proxyCfg = cfg
	}

	if spawnAgent {
		s.spawnAgentLocked()
	}
	if err := s.startProxySessionLocked(); err != nil {
		s.logger.Warn("start always-on proxy session failed", "error", err)
	}
}

// StopProxyServer 停止常驻代理会话与 agent 子进程（pipeline 退出时调用）。
func (s *pipelineService) StopProxyServer(ctx context.Context) {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	if s.proxySessionID != "" {
		if task, ok := s.getTask(s.proxySessionID); ok {
			if _, err := task.Stop(ctx); err != nil {
				s.logger.Warn("stop proxy session failed", "session_id", s.proxySessionID, "error", err)
			}
		}
		s.proxySessionID = ""
	}
	s.stopAgentLocked()
}

// GetProxyConfig 返回当前代理抓包服务器配置与运行时状态。
func (s *pipelineService) GetProxyConfig(_ context.Context) (capturecontrol.ProxyConfigState, error) {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	return s.buildProxyStateLocked(), nil
}

// UpdateProxyConfig 应用新的代理抓包服务器配置：
//  1. 合并 + 校验 + 持久化到 proxy.json
//  2. 热重启 agent（kill + respawn，新 listen/server/筛选生效）
//  3. 重启常驻代理会话（新 server_addr/frame_style/prefix_len/plugin 生效）
func (s *pipelineService) UpdateProxyConfig(ctx context.Context, req capturecontrol.ProxyConfigUpdate) (capturecontrol.ProxyConfigState, error) {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()

	next := s.proxyCfg
	if req.ListenAddr != "" {
		next.ListenAddr = req.ListenAddr
	}
	if req.ServerAddr != "" {
		next.ServerAddr = req.ServerAddr
	}
	if req.FrameStyle != "" {
		next.FrameStyle = req.FrameStyle
	}
	if req.PrefixLen > 0 {
		next.PrefixLen = int(req.PrefixLen)
	}
	next.LittleEndian = req.LittleEndian
	if req.Plugin != "" {
		next.Plugin = req.Plugin
	}
	// 筛选列表：请求携带非 nil 才覆盖（保留空列表=清空筛选）。
	if req.IncludeHosts != nil {
		next.IncludeHosts = req.IncludeHosts
	}
	if req.IncludePorts != nil {
		next.IncludePorts = toIntList(req.IncludePorts)
	}

	norm, err := next.Normalize()
	if err != nil {
		return capturecontrol.ProxyConfigState{}, fmt.Errorf("invalid proxy config: %w", err)
	}
	if err := config.SaveProxyServerConfig(s.proxyPath, norm); err != nil {
		return capturecontrol.ProxyConfigState{}, fmt.Errorf("save proxy config: %w", err)
	}
	s.proxyCfg = norm

	if s.spawnAgent {
		s.stopAgentLocked()
		s.spawnAgentLocked()
	}
	if err := s.restartProxySessionLocked(ctx); err != nil {
		s.logger.Warn("restart proxy session failed (old session stopped)", "error", err)
	}
	return s.buildProxyStateLocked(), nil
}

// spawnAgentLocked 拉起 agent（需持 proxyMu）。
func (s *pipelineService) spawnAgentLocked() {
	s.agentProc = spawnSingboxAgent(s.workDir, s.agentBin, s.proxyCfg)
}

// stopAgentLocked 终止 agent 子进程（需持 proxyMu，幂等）。
func (s *pipelineService) stopAgentLocked() {
	if s.agentProc != nil {
		s.agentProc.stop()
		s.agentProc = nil
	}
}

// startProxySessionLocked 启动常驻代理抓包会话（mobile Source，监听 ServerAddr）。
// 已存在常驻会话时直接返回 nil（幂等）。
func (s *pipelineService) startProxySessionLocked() error {
	if s.proxySessionID != "" {
		return nil
	}
	res, err := s.StartSession(context.Background(), capturecontrol.StartSessionRequest{
		Plugin: s.proxyCfg.Plugin,
		Mobile: &capturecontrol.MobileConfig{
			ListenAddr:   s.proxyCfg.ServerAddr,
			FrameStyle:   s.proxyCfg.FrameStyle,
			PrefixLen:    s.proxyCfg.PrefixLen,
			LittleEndian: s.proxyCfg.LittleEndian,
		},
	})
	if err != nil {
		return err
	}
	s.proxySessionID = res.SessionID
	s.logger.Info("always-on proxy session started",
		"session_id", res.SessionID, "server_addr", s.proxyCfg.ServerAddr,
		"frame_style", s.proxyCfg.FrameStyle, "plugin", s.proxyCfg.Plugin)
	return nil
}

// restartProxySessionLocked 停止旧常驻会话并按最新配置重启（需持 proxyMu）。
func (s *pipelineService) restartProxySessionLocked(ctx context.Context) error {
	if s.proxySessionID != "" {
		if task, ok := s.getTask(s.proxySessionID); ok {
			if _, err := task.Stop(ctx); err != nil {
				s.logger.Warn("stop old proxy session failed", "session_id", s.proxySessionID, "error", err)
			}
		}
		s.proxySessionID = ""
	}
	return s.startProxySessionLocked()
}

// buildProxyStateLocked 组装配置 + 运行时状态快照（需持 proxyMu）。
func (s *pipelineService) buildProxyStateLocked() capturecontrol.ProxyConfigState {
	st := capturecontrol.ProxyConfigState{
		ListenAddr:   s.proxyCfg.ListenAddr,
		ServerAddr:   s.proxyCfg.ServerAddr,
		FrameStyle:   s.proxyCfg.FrameStyle,
		PrefixLen:    int32(s.proxyCfg.PrefixLen),
		LittleEndian: s.proxyCfg.LittleEndian,
		ConfigPath:   s.proxyPath,
		Plugin:       s.proxyCfg.Plugin,
		IncludeHosts: s.proxyCfg.IncludeHosts,
		IncludePorts: toInt32List(s.proxyCfg.IncludePorts),
	}
	if s.agentProc.running() {
		st.AgentRunning = true
		st.AgentPID = int32(s.agentProc.pid())
	}
	if s.proxySessionID != "" {
		if task, ok := s.getTask(s.proxySessionID); ok {
			st.SessionRunning = task.State() == capture.StateRunning
			st.SessionID = s.proxySessionID
		}
	}
	return st
}

// toIntList 将 []int32 转换为 []int。
func toIntList(in []int32) []int {
	if in == nil {
		return nil
	}
	out := make([]int, len(in))
	for i, v := range in {
		out[i] = int(v)
	}
	return out
}

// toInt32List 将 []int 转换为 []int32。
func toInt32List(in []int) []int32 {
	if in == nil {
		return nil
	}
	out := make([]int32, len(in))
	for i, v := range in {
		out[i] = int32(v)
	}
	return out
}
