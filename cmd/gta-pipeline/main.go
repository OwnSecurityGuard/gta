// Command gta-pipeline 是抓包+解码+分析+落库进程。
// 持有 in-process Capture Source、Dispatcher、Analyze Engine，
// 写 SQLite via EventWriter/ProjectionWriter，暴露 CaptureControl gRPC server。
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	pluginpb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
	"gta/pkg/analyze"
	"gta/pkg/config"
	"gta/pkg/internalipc"
	"gta/pkg/internalipc/capturecontrol"
	pb "gta/pkg/internalipc/proto"
	"gta/pkg/logging"
	"gta/pkg/plugin"
	protocolconfig "gta/pkg/protocol/config"
	"gta/pkg/store"

	"google.golang.org/grpc"
)

func main() {
	workDir := flag.String("workdir", ".", "working directory")
	rulesPath := flag.String("rules", "", "rules.yaml path")
	protocolPath := flag.String("protocol", "", "protocol.yaml path (Protocol Behavior Resolver)")
	controlPath := flag.String("control", "", "control.sqlite path (default: <workdir>/control.sqlite)")
	// 默认走 TCP 端口（:9091 注册, :9888 控制），兼容 Windows / 跨机器。
	controlAddr := flag.String("control-addr", ":9888", "CaptureControl gRPC 监听地址（默认 :9888）")
	registryAddr := flag.String("registry-addr", ":9091", "PluginRegistry gRPC 监听地址（默认 :9091）")
	debug := flag.Bool("debug", false, "enable debug logging")
	logFormat := flag.String("log-format", "json", "log format: json | text")
	logFile := flag.String("log-file", "", "log file path (default: <workdir>/logs/gta-pipeline.log)")
	// sing-box server（gta-singbox-agent）自动拉起：默认随 pipeline 启动，常驻等待手机代理连接。
	spawnAgent := flag.Bool("spawn-agent", true, "spawn gta-singbox-agent at startup (always-on proxy listener, disabled with -spawn-agent=false)")
	agentBin := flag.String("agent-bin", "", "path to gta-singbox-agent binary (default: <workdir>/bin/gta-singbox-agent[.exe])")
	flag.Parse()

	absWorkDir, _ := filepath.Abs(*workDir)

	// 统一日志初始化：文件落盘 + stderr 双写 + 按大小轮转
	logCfg := logging.DefaultConfig()
	if *debug {
		logCfg.Level = slog.LevelDebug
	}
	logCfg.Format = logging.Format(*logFormat)
	if *logFile == "" {
		*logFile = filepath.Join(absWorkDir, "logs", "gta-pipeline.log")
	}
	logCfg.FilePath = *logFile
	logging.MustInit(logCfg)

	if *controlPath == "" {
		*controlPath = filepath.Join(absWorkDir, "control.sqlite")
	}

	controlStore, err := store.NewControlStore(*controlPath)
	if err != nil {
		slog.Error("open control store", "error", err)
		os.Exit(1)
	}
	defer controlStore.Close()

	// 启动兜底：上一进程若未优雅退出（崩溃 / SIGKILL / 断电），ControlStore 中可能残留
	// status='running' 的会话，导致重启后前端持续显示"运行中"。此处统一标记为 stopped；
	// 正常退出则由下方信号处理中的 engine.StopAll 主动置 stopped，二者共同保证状态一致。
	reconcileCtx, reconcileCancel := context.WithTimeout(context.Background(), 10*time.Second)
	reconciled, rerr := controlStore.ReconcileRunningSessions(reconcileCtx, time.Now())
	reconcileCancel()
	if rerr != nil {
		slog.Warn("reconcile stale running sessions", "error", rerr)
	} else if reconciled > 0 {
		slog.Info("reconciled stale running sessions from previous run", "count", reconciled)
	}

	var rules []*analyze.CompiledRule
	if *rulesPath != "" {
		rules, err = config.LoadRules(*rulesPath)
		if err != nil {
			slog.Error("load rules", "error", err)
			os.Exit(1)
		}
	}

	// Protocol Behavior Resolver：可选。未配置 --protocol 时默认为空，事件不做语义富化。
	var protocolCfg *protocolconfig.File
	if *protocolPath != "" {
		protocolCfg, err = protocolconfig.Load(*protocolPath)
		if err != nil {
			slog.Error("load protocol config", "error", err)
			os.Exit(1)
		}
	}

	// RegistryServer：被动接受插件注册，插件进程由外部编排（systemd/脚本）独立启动。
	registry := plugin.NewRegistryServer(10)
	defer registry.Close()

	// registry socket：插件通过此端点调用 PluginRegistry RPC 注册自身。
	// 默认 :9091（TCP），可通过 -registry-addr 覆盖。
	var registryLis net.Listener
	registryLis, err = internalipc.ListenAddr(*registryAddr)
	if err != nil {
		slog.Error("listen registry", "error", err)
		os.Exit(1)
	}
	defer registryLis.Close()
	registryGrpc := grpc.NewServer()
	pluginpb.RegisterPluginRegistryServer(registryGrpc, registry)

	// 心跳检查：每秒扫描注册表，30 秒未心跳的插件移除。
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for range ticker.C {
			registry.CheckOffline(30 * time.Second)
		}
	}()

	engine := newPipelineService(absWorkDir, controlStore, registry, rules, protocolCfg, *registryAddr)

	// CaptureControl gRPC server：供 gta-mcp / gta-trace 调用。
	// 默认 :8088（TCP），可通过 -control-addr 覆盖。
	var listener net.Listener
	listener, err = internalipc.ListenAddr(*controlAddr)
	if err != nil {
		slog.Error("listen", "error", err)
		os.Exit(1)
	}
	defer listener.Close()

	grpcSrv := grpc.NewServer()
	pb.RegisterCaptureControlServer(grpcSrv, capturecontrol.NewServer(engine))

	// registry gRPC server 在独立 goroutine 中 serve。
	go func() {
		if err := registryGrpc.Serve(registryLis); err != nil {
			slog.Error("serve registry", "error", err)
			os.Exit(1)
		}
	}()

	// 默认自动拉起 gta-singbox-agent（sing-box server 常驻，等待手机代理软件连接）。
	// 同时启动常驻代理抓包会话；二进制缺失时告警但不阻断 pipeline 启动。
	// 可用 -spawn-agent=false 关闭 agent 自动拉起（常驻会话仍启动）。
	engine.StartAlwaysOnProxy(*spawnAgent, *agentBin)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", sig)
		// 先终止 sing-box agent 子进程与常驻代理会话（幂等，重复调用无副作用）。
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutdownCancel()
		engine.StopProxyServer(shutdownCtx)
		// 再优雅停止所有抓包会话：cancel + 等待各自 finalize 写库（running→stopped），
		// 确保退出前本进程的活跃会话状态已落库。超时由 shutdownCtx 控制，不阻塞退出。
		engine.StopAll(shutdownCtx)
		grpcSrv.GracefulStop()
		registryGrpc.GracefulStop()
	}()

	// 插件应使用的 registry 端点，即 -registry-addr 的值（默认 :9091）。
	regEndpoint := *registryAddr

	slog.Info("gta-pipeline starting",
		"workdir", absWorkDir,
		"socket", listener.Addr(),
		"registry_socket", registryLis.Addr(),
		"GTA_REGISTRY_ADDR", regEndpoint,
	)
	if err := grpcSrv.Serve(listener); err != nil {
		slog.Error("serve", "error", err)
		os.Exit(1)
	}
}
