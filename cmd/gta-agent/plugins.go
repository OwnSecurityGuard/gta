package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"gta/pkg/plugindev"
)

// pluginEnvVarName 是 agent 注入给插件的环境变量名集合（模板约定见
// pkg/plugindev/templates/create_plugin/main.go.tmpl）：
//   - GTA_REGISTRY_ADDR：registry 端点（SDK 原生读取）；
//   - GTA_TUNNEL=1：走隧道注册模式（Register(tunnel=true) + Connect 流）；
//   - GTA_AUTH_TOKEN：Bearer token（为空时不注入 = 匿名）。
const (
	envRegistryAddr = "GTA_REGISTRY_ADDR"
	envTunnel       = "GTA_TUNNEL"
	envAuthToken    = "GTA_AUTH_TOKEN"
)

// spawnEnv 组装插件子进程的环境变量。
// token 为空时不注入 GTA_AUTH_TOKEN（匿名模式）。
func spawnEnv(registryAddr, token string) []string {
	env := append(os.Environ(),
		envRegistryAddr+"="+registryAddr,
		envTunnel+"=1",
	)
	if token != "" {
		env = append(env, envAuthToken+"="+token)
	}
	return env
}

// pluginSupervisor 发现本机插件并拉起，进程退出后按指数退避重启，
// agent 退出时随 ctx 一起停止。
type pluginSupervisor struct {
	dir          string
	registryAddr string
	token        string

	mu   sync.Mutex
	pids map[string]int // plugin name -> 当前 pid（0 表示未运行）
}

// stableRunThreshold 是插件进程连续存活多久后视为稳定运行，
// 下次崩溃前重启退避归位（与 ingest 重连同策略）。
const stableRunThreshold = 30 * time.Second

// pluginLogMaxBytes 是单个插件日志文件的大小上限，超过后下次启动从头截断。
const pluginLogMaxBytes = 8 << 20

// gracefulStopTimeout 是插件进程优雅退出（SIGINT/Interrupt）的等待时长，
// 超时后强杀（Kill）。
const gracefulStopTimeout = 3 * time.Second

func (s *pluginSupervisor) runningCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, pid := range s.pids {
		if pid != 0 {
			n++
		}
	}
	return n
}

func (s *pluginSupervisor) setPID(name string, pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pids == nil {
		s.pids = map[string]int{}
	}
	s.pids[name] = pid
}

// run 发现插件并为每个插件启动一个 supervise goroutine；
// 各 goroutine 的收尾通过 wg 跟踪（agent 停机时 join）。返回发现的插件数。
func (s *pluginSupervisor) run(ctx context.Context, wg *sync.WaitGroup) int {
	plugins, err := plugindev.ListPlugins(s.dir)
	if err != nil {
		slog.Warn("plugin discovery failed", "dir", s.dir, "error", err)
		return 0
	}
	if len(plugins) == 0 {
		slog.Info("no local plugins discovered", "dir", s.dir)
		return 0
	}
	for _, p := range plugins {
		slog.Info("hosting local plugin", "name", p.Name, "binary", p.Binary)
		wg.Add(1)
		go func(p *plugindev.DiscoveredPlugin) {
			defer wg.Done()
			s.supervise(ctx, p)
		}(p)
	}
	return len(plugins)
}

// supervise 单个插件的生命周期：启动→等待退出→退避后重启，直到 ctx 取消。
func (s *pluginSupervisor) supervise(ctx context.Context, p *plugindev.DiscoveredPlugin) {
	bk := newBackoff()
	for ctx.Err() == nil {
		started := time.Now()
		startErr := s.startAndWait(ctx, p)
		if ctx.Err() != nil {
			return
		}
		// 上一次运行足够长说明稳定过，退避归位（避免长期运行后
		// 偶发崩溃也要承受 30s 重启延迟）。
		if time.Since(started) > stableRunThreshold {
			bk.Reset()
		}
		wait := bk.Next()
		slog.Warn("plugin exited, restarting", "plugin", p.Name, "error", startErr, "backoff", wait)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}
}

// openPluginLog 打开插件日志文件（追加写）；超过大小上限时从头截断，
// 避免无限增长。
func (s *pluginSupervisor) openPluginLog(p *plugindev.DiscoveredPlugin) (*os.File, string) {
	logPath := filepath.Join(p.Dir, p.Name+".agent.log")
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > pluginLogMaxBytes {
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		slog.Info("plugin log file exceeds size cap, truncating", "path", logPath, "size", fi.Size())
	}
	logFile, err := os.OpenFile(logPath, flags, 0o644)
	if err != nil {
		slog.Warn("cannot open plugin log file, output will be discarded", "path", logPath, "error", err)
		return nil, logPath
	}
	return logFile, logPath
}

// startAndWait 启动一次插件进程并阻塞等待其退出；返回启动/退出错误（可为 nil）。
// stdout/stderr 写入 <dir>/<name>.agent.log（超上限时截断）。
func (s *pluginSupervisor) startAndWait(ctx context.Context, p *plugindev.DiscoveredPlugin) error {
	logFile, logPath := s.openPluginLog(p)

	cmd := exec.Command(p.Binary)
	cmd.Dir = p.Dir
	cmd.Env = spawnEnv(s.registryAddr, s.token)
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		// 阻断插件继承 agent 的 stdio（Windows 下 cmd.SysProcAttr 不需要额外处理）。
		cmd.Stdin = nil
	}
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return fmt.Errorf("start: %w", err)
	}
	slog.Info("plugin started", "plugin", p.Name, "pid", cmd.Process.Pid, "log", logPath)
	s.setPID(p.Name, cmd.Process.Pid)

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-waitCh:
	case <-ctx.Done():
		// agent 停机：先尝试优雅中断，超时强杀，再等收尾，避免孤儿进程。
		waitErr = stopPlugin(cmd, waitCh)
	}
	if logFile != nil {
		_ = logFile.Close()
	}
	s.setPID(p.Name, 0)
	if ctx.Err() != nil {
		return nil
	}
	if waitErr == nil {
		return nil
	}
	if _, ok := waitErr.(*exec.ExitError); ok {
		return waitErr // 正常崩溃路径，交给 supervise 重启
	}
	return waitErr
}

// stopPlugin 停机路径：向插件进程发送 Interrupt（Windows 不支持时静默退回
// 强杀），等待 gracefulStopTimeout 后仍存活则 Kill；返回 Wait 结果
// （waitCh 恰好被消费一次）。
func stopPlugin(cmd *exec.Cmd, waitCh <-chan error) error {
	if cmd.Process != nil {
		if err := cmd.Process.Signal(os.Interrupt); err == nil {
			select {
			case err := <-waitCh:
				return err
			case <-time.After(gracefulStopTimeout):
			}
		}
		_ = cmd.Process.Kill()
	}
	return <-waitCh
}
