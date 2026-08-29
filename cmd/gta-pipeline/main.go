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
	"gta/pkg/auth"
	"gta/pkg/capture/agent"
	"gta/pkg/capture/agent/proto"
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
	// 统一配置（T10）：-config 指向 gta.yaml（可选）。每个设置项的优先级：
	// flag（显式传入） > 环境变量 GTA_* > gta.yaml > 默认值（即各 flag 默认值）。
	// 不传 -config 且未设置环境变量时，行为与历史版本完全一致。
	cfgPath := flag.String("config", "", "统一配置文件 gta.yaml 路径（可选；优先级 flag > 环境变量 GTA_* > 配置文件 > 默认值）")
	// 工作目录解析规则（T10）：显式 -workdir > GTA_HOME > gta.yaml workdir >
	// CWD 既有数据探测（存在 control.sqlite/sessions/runs 时沿用 CWD，避免破坏
	// 老用户数据发现）> ~/.gta。GTA_HOME 显式设置时始终优先于 ~/.gta。
	workDir := flag.String("workdir", ".", "working directory（显式传参优先；否则 GTA_HOME > gta.yaml workdir > CWD 既有数据沿用 > ~/.gta）")
	rulesPath := flag.String("rules", "", "rules.yaml path")
	protocolPath := flag.String("protocol", "", "protocol.yaml path (Protocol Behavior Resolver)")
	controlPath := flag.String("control", "", "control.sqlite path (default: <workdir>/control.sqlite)")
	// 默认走 TCP 端口（:9091 注册, :9888 控制），兼容 Windows / 跨机器。
	controlAddr := flag.String("control-addr", ":9888", "CaptureControl gRPC 监听地址（默认 :9888）")
	registryAddr := flag.String("registry-addr", ":9091", "PluginRegistry gRPC 监听地址（默认 :9091）")
	agentIngestAddr := flag.String("agent-ingest-addr", ":9092", "AgentIngest gRPC 监听地址（gta-agent 推送原始帧入口，默认 :9092；传空字符串禁用）")
	debug := flag.Bool("debug", false, "enable debug logging")
	logFormat := flag.String("log-format", "json", "log format: json | text")
	logFile := flag.String("log-file", "", "log file path (default: <workdir>/logs/gta-pipeline.log)")
	// sing-box server（gta-singbox-agent）自动拉起：默认随 pipeline 启动，常驻等待手机代理连接。
	spawnAgent := flag.Bool("spawn-agent", true, "spawn gta-singbox-agent at startup (always-on proxy listener, disabled with -spawn-agent=false)")
	agentBin := flag.String("agent-bin", "", "path to gta-singbox-agent binary (default: <workdir>/bin/gta-singbox-agent[.exe])")
	flag.Parse()

	// 加载统一配置并按优先级合并（flag 显式 > 环境变量 > 文件 > 默认值）。
	// 环境变量兜底已在 config.Load 内应用；cfg 字段为空表示"未配置"。
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("load config", "path", *cfgPath, "error", err)
		os.Exit(1)
	}
	flagSet := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { flagSet[f.Name] = true })
	// eff 返回某设置项的最终值：flag 显式传入 > 配置（文件/环境变量）> flag 默认值。
	eff := func(name, flagVal, cfgVal, def string) string {
		if flagSet[name] {
			return flagVal
		}
		if cfgVal != "" {
			return cfgVal
		}
		return def
	}
	controlAddrFlag := *controlAddr
	registryAddrFlag := *registryAddr
	agentIngestAddrFlag := *agentIngestAddr
	*controlAddr = eff("control-addr", controlAddrFlag, cfg.Pipeline.ControlAddr, config.DefaultControlAddr)
	*registryAddr = eff("registry-addr", registryAddrFlag, cfg.Pipeline.RegistryAddr, config.DefaultRegistryAddr)
	*agentIngestAddr = eff("agent-ingest-addr", agentIngestAddrFlag, cfg.Pipeline.AgentIngestAddr, config.DefaultAgentIngestAddr)

	absWorkDir, err := config.ResolveWorkDir(*workDir, flagSet["workdir"], cfg.WorkDir)
	if err != nil {
		slog.Error("resolve workdir", "error", err)
		os.Exit(1)
	}

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
	// 实际监听地址回写（:0 动态端口时外部从此文件读取真实地址，同机可跑多套）。
	config.WriteAddrFile(absWorkDir, "registry", registryLis.Addr().String())
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
	// T11：gta.yaml 的 proxy.server_addr（含 GTA_PROXY_SERVER_ADDR 兜底）作为
	// proxy.json 未指定 server_addr 时的兜底值。
	engine.proxyServerAddrOverride = cfg.Proxy.ServerAddr

	// AgentIngest gRPC server：gta-agent 推送本机原始帧的入口（团队协作模式）。
	// 默认 :9092；-agent-ingest-addr 传空字符串禁用。鉴权复用 pkg/auth：
	// GTA_AUTH_TOKENS 未配置时为匿名模式，owner 统一为 "local"（auth.AnonymousOwner），
	// 与本地单机创建的会话归属一致。会话归属校验用 ControlStore 查 sessions.owner，
	// owner 不匹配的 batch 以 PermissionDenied 拒绝。
	var agentIngestGrpc *grpc.Server
	if *agentIngestAddr != "" {
		authResolver, err := auth.LoadFromEnv()
		if err != nil {
			slog.Error("load auth tokens", "error", err)
			os.Exit(1)
		}
		agentHub := agent.NewHub()
		engine.SetAgentHub(agentHub)

		agentLis, err := internalipc.ListenAddr(*agentIngestAddr)
		if err != nil {
			slog.Error("listen agent ingest", "error", err)
			os.Exit(1)
		}
		config.WriteAddrFile(absWorkDir, "agent-ingest", agentLis.Addr().String())
		defer agentLis.Close()
		agentIngestGrpc = grpc.NewServer(
			grpc.ChainStreamInterceptor(auth.StreamInterceptor(authResolver)),
		)
		proto.RegisterAgentIngestServer(agentIngestGrpc,
			agent.NewIngestServer(agentHub, controlStoreSessionOwners{store: controlStore}))
		go func() {
			if err := agentIngestGrpc.Serve(agentLis); err != nil {
				slog.Error("serve agent ingest", "error", err)
			}
		}()
		slog.Info("agent ingest listening", "addr", agentLis.Addr())
	}

	// CaptureControl gRPC server：供 gta-mcp / gta-trace 调用。
	// 默认 :8088（TCP），可通过 -control-addr 覆盖。
	//
	// 信任边界：本 server 的鉴权语义假设唯一客户端是同机 gta-mcp（其 HTTP 层
	// 已做 Bearer 鉴权）。StartCapture/ListPlugins 等请求里的 owner/all_owners
	// 字段不做 gRPC 层校验——能直连本端口的进程可伪造任意身份。当前默认绑到
	// 全接口（":9888"），把控制面暴露到局域网属于已知风险；收紧为回环监听或
	// 接入 pkg/auth.UnaryInterceptor 是后续加固项（见 T12/T13 评审记录）。
	var listener net.Listener
	listener, err = internalipc.ListenAddr(*controlAddr)
	if err != nil {
		slog.Error("listen", "error", err)
		os.Exit(1)
	}
	config.WriteAddrFile(absWorkDir, "control", listener.Addr().String())
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
		if agentIngestGrpc != nil {
			// GracefulStop 会等所有活跃 Push 流结束——gta-agent 的流是长连接，
			// 不限时会卡住整个退出流程。限时 5s，超时硬停（未投递包按约定丢弃）。
			stopped := make(chan struct{})
			go func() {
				agentIngestGrpc.GracefulStop()
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				slog.Warn("agent ingest graceful stop timed out, forcing stop")
				agentIngestGrpc.Stop()
			}
		}
	}()

	// 插件应使用的 registry 端点：默认取 -registry-addr 的值（默认 :9091）；
	// 配置为 ":0" 动态端口时改用监听器解析出的实际地址（否则插件无法连接）。
	regEndpoint := *registryAddr
	if _, port, perr := net.SplitHostPort(*registryAddr); perr == nil && port == "0" {
		regEndpoint = registryLis.Addr().String()
	}

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

// controlStoreSessionOwners 用 ControlStore 实现 agent.SessionOwnerChecker：
// 查 sessions.owner 做会话归属校验，避免 pkg/capture 反向依赖 pkg/store。
type controlStoreSessionOwners struct {
	store *store.ControlStore
}

func (o controlStoreSessionOwners) SessionOwner(sessionID string) (string, bool) {
	meta, err := o.store.GetSession(context.Background(), sessionID)
	if err != nil || meta == nil {
		return "", false
	}
	return meta.Owner, true
}
