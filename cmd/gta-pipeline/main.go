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
	"gta/pkg/store"

	"google.golang.org/grpc"
)

func main() {
	workDir := flag.String("workdir", ".", "working directory")
	rulesPath := flag.String("rules", "", "rules.yaml path")
	controlPath := flag.String("control", "", "control.sqlite path (default: <workdir>/control.sqlite)")
	// 默认走 TCP 端口（:9091 注册, :9888 控制），兼容 Windows / 跨机器。
	controlAddr := flag.String("control-addr", ":9888", "CaptureControl gRPC 监听地址（默认 :9888）")
	registryAddr := flag.String("registry-addr", ":9091", "PluginRegistry gRPC 监听地址（默认 :9091）")
	debug := flag.Bool("debug", false, "enable debug logging")
	logFormat := flag.String("log-format", "json", "log format: json | text")
	logFile := flag.String("log-file", "", "log file path (default: <workdir>/logs/gta-pipeline.log)")
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

	engine := newPipelineService(absWorkDir, controlStore, registry, rules, *registryAddr)

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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", sig)
		// 先优雅停止所有抓包会话：cancel + 等待各自 finalize 写库（running→stopped），
		// 确保退出前本进程的活跃会话状态已落库。超时由 shutdownCtx 控制，不阻塞退出。
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutdownCancel()
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
