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
	bk           *backoff

	mu   sync.Mutex
	pids map[string]int // plugin name -> 当前 pid（0 表示未运行）
}

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

// run 阻塞直到 ctx 取消。返回发现并拉起的插件数。
func (s *pluginSupervisor) run(ctx context.Context) int {
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
		go s.supervise(ctx, p)
	}
	return len(plugins)
}

// supervise 单个插件的生命周期：启动→等待退出→退避后重启，直到 ctx 取消。
func (s *pluginSupervisor) supervise(ctx context.Context, p *plugindev.DiscoveredPlugin) {
	bk := newBackoff()
	for ctx.Err() == nil {
		startErr := s.startAndWait(ctx, p)
		if ctx.Err() != nil {
			return
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

// startAndWait 启动一次插件进程并阻塞等待其退出；返回启动/退出错误（可为 nil）。
// stdout/stderr 追加写入 <dir>/<name>.agent.log。
func (s *pluginSupervisor) startAndWait(ctx context.Context, p *plugindev.DiscoveredPlugin) error {
	logPath := filepath.Join(p.Dir, p.Name+".agent.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		logFile = nil
		slog.Warn("cannot open plugin log file, output will be discarded", "path", logPath, "error", err)
	}

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
		// agent 停机：先杀插件再等收尾。
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		waitErr = <-waitCh
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
