// Command gt-pipeline 是抓包+解码+分析+落库进程。
// 持有 in-process Capture Source、Dispatcher、Analyze Engine，
// 写 SQLite via EventWriter/ProjectionWriter，暴露 CaptureControl gRPC server。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"database/sql"

	pluginpb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
	"gametrace/pkg/analyze"
	"gametrace/pkg/auth"
	"gametrace/pkg/capture/agent"
	"gametrace/pkg/capture/agent/proto"
	"gametrace/pkg/config"
	"gametrace/pkg/internalipc"
	"gametrace/pkg/internalipc/capturecontrol"
	pb "gametrace/pkg/internalipc/proto"
	"gametrace/pkg/logging"
	"gametrace/pkg/plugin"
	"gametrace/pkg/probe"
	protocolconfig "gametrace/pkg/protocol/config"
	"gametrace/pkg/store"
	"gametrace/pkg/version"

	"google.golang.org/grpc"
)

func main() {
	// 统一配置（T10）：-config 指向 gametrace.yaml（可选）。每个设置项的优先级：
	// flag（显式传入） > 环境变量 GT_* > gametrace.yaml > 默认值（即各 flag 默认值）。
	// 不传 -config 且未设置环境变量时，行为与历史版本完全一致。
	cfgPath := flag.String("config", "", "统一配置文件 gametrace.yaml 路径（可选；优先级 flag > 环境变量 GT_* > 配置文件 > 默认值）")
	// 工作目录解析规则（T10）：显式 -workdir > GT_HOME > gametrace.yaml workdir >
	// CWD 既有数据探测（存在 control.sqlite/sessions/runs 时沿用 CWD，避免破坏
	// 老用户数据发现）> ~/.gametrace。GT_HOME 显式设置时始终优先于 ~/.gametrace。
	workDir := flag.String("workdir", ".", "working directory（显式传参优先；否则 GT_HOME > gametrace.yaml workdir > CWD 既有数据沿用 > ~/.gametrace）")
	rulesPath := flag.String("rules", "", "rules.yaml path")
	protocolPath := flag.String("protocol", "", "protocol.yaml path (Protocol Behavior Resolver)")
	controlPath := flag.String("control", "", "control.sqlite path (default: <workdir>/control.sqlite)")
	// 默认走 TCP 端口（:9091 注册, :9888 控制），兼容 Windows / 跨机器。
	controlAddr := flag.String("control-addr", ":9888", "CaptureControl gRPC 监听地址（默认 :9888）")
	registryAddr := flag.String("registry-addr", ":9091", "PluginRegistry gRPC 监听地址（默认 :9091）")
	agentIngestAddr := flag.String("agent-ingest-addr", ":9092", "AgentIngest gRPC 监听地址（gt-agent 推送原始帧入口，默认 :9092；传空字符串禁用）")
	debug := flag.Bool("debug", false, "enable debug logging")
	logFormat := flag.String("log-format", "json", "log format: json | text")
	logFile := flag.String("log-file", "", "log file path (default: <workdir>/logs/gt-pipeline.log)")
	// gt-singbox-agent 二进制路径：代理抓包租约（CreateProxyLease）创建时按租约拉起。
	agentBin := flag.String("agent-bin", "", "path to gt-singbox-agent binary (default: <workdir>/bin/gt-singbox-agent[.exe])")
	showVersion := flag.Bool("version", false, "print version and exit")
	// 存储驱动：默认 sqlite（每会话一个 capture.sqlite + 全局 control.sqlite）；
	// 设 "postgres" 时启用 PostgreSQL，DSN 由 -db-dsn / 环境变量 GT_DB_DSN 提供。
	// 优先级：flag 显式 > 环境变量 GT_DB_* > 默认 sqlite。
	dbDriver := flag.String("db-driver", "", "storage driver: sqlite (default) | postgres")
	dbDSN := flag.String("db-dsn", "", "postgres DSN (required when -db-driver=postgres); env GT_DB_DSN")
	flag.Parse()
	if *showVersion {
		fmt.Println("gt-pipeline " + version.String())
		return
	}

	flagSet := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { flagSet[f.Name] = true })
	// 加载统一配置并按优先级合并（flag 显式 > 环境变量 > 文件 > 默认值）。
	// 环境变量兜底已在 config.Load 内应用；cfg 字段为空表示"未配置"。
	// -config 显式指定的路径不存在时硬错误（防止拼错路径静默退回默认配置）。
	cfg, err := config.Load(*cfgPath, flagSet["config"])
	if err != nil {
		slog.Error("load config", "path", *cfgPath, "error", err)
		os.Exit(1)
	}
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

	// 存储驱动解析（flag 显式 > 环境变量 GT_DB_* > 默认 sqlite）。
	if !flagSet["db-driver"] && *dbDriver == "" {
		*dbDriver = os.Getenv("GT_DB_DRIVER")
	}
	if *dbDriver == "" {
		*dbDriver = "sqlite"
	}
	if !flagSet["db-dsn"] && *dbDSN == "" {
		*dbDSN = os.Getenv("GT_DB_DSN")
	}
	if store.IsPostgres(*dbDriver) && *dbDSN == "" {
		slog.Error("storage driver=postgres requires -db-dsn (or GT_DB_DSN)")
		os.Exit(1)
	}

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
		*logFile = filepath.Join(absWorkDir, "logs", "gt-pipeline.log")
	}
	logCfg.FilePath = *logFile
	// T17：GT_LOG_FILE_DISABLED / GT_LOG_STDERR_DISABLED 可关闭文件落盘或 stderr 双写（容器部署用）。
	logCfg = logging.FromEnv(logCfg)
	logging.MustInit(logCfg)

	if *controlPath == "" {
		*controlPath = filepath.Join(absWorkDir, "control.sqlite")
	}

	// 控制元数据后端：sqlite 走 control.sqlite 文件路径；postgres 走共享 PG DSN。
	controlDSNOrPath := *controlPath
	if store.IsPostgres(*dbDriver) {
		controlDSNOrPath = *dbDSN
	}
	controlStore, err := store.OpenControlStore(*dbDriver, controlDSNOrPath)
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

	// authResolver：env bootstrap（GT_AUTH_TOKENS）+ users 表（自助注册/邀请用户）
	// 组合的 Bearer 鉴权，供 PluginRegistry 与 AgentIngest 两个 gRPC server 共用。
	// 两者皆空时为匿名模式（拦截器放行、不注入 Principal），本地单机用法行为不变。
	// users 表让插件凭自助注册 token 注册即归属 owner（否则一律匿名、对所有人可见）。
	envResolver, err := auth.LoadFromEnv()
	if err != nil {
		slog.Error("load auth tokens", "error", err)
		os.Exit(1)
	}
	// users/projects 等辅助表始终在本地 sqlite（与 gt-mcp 的 auxDB 同一文件）：
	// sqlite 模式复用 control.sqlite；postgres 模式用独立 control-aux.sqlite。
	usersDB := controlStore.DB()
	if store.IsPostgres(*dbDriver) {
		usersDB, err = sql.Open("sqlite", filepath.Join(absWorkDir, "control-aux.sqlite"))
		if err != nil {
			slog.Error("open aux sqlite for users", "error", err)
			os.Exit(1)
		}
		defer usersDB.Close()
	}
	authResolver := auth.NewFirstResolver(envResolver, auth.NewDBResolver(usersDB), probe.NewAuthResolver(controlStore))

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
	// 鉴权：metadata `authorization: Bearer <token>` → Principal。owner 随 ctx
	// 进入 Register/Connect（TunnelHub），决定插件键 owner/name 与会话归属。
	// 仅在配置了 token 时挂拦截器：匿名模式注入的 "local" owner 会让插件键
	// 变成 local/name，破坏空 owner=裸 name 的单机回归语义（见 pluginKey）。
	registryGrpc := grpc.NewServer()
	if authResolver.Required() {
		registryGrpc = grpc.NewServer(
			grpc.ChainUnaryInterceptor(auth.UnaryInterceptor(authResolver)),
			grpc.ChainStreamInterceptor(auth.StreamInterceptor(authResolver)),
		)
	}
	pluginpb.RegisterPluginRegistryServer(registryGrpc, registry)

	// 心跳检查：每秒扫描注册表，30 秒未心跳的插件移除。
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for range ticker.C {
			registry.CheckOffline(30 * time.Second)
		}
	}()

	engine := newPipelineService(absWorkDir, controlStore, registry, rules, protocolCfg, *registryAddr, *dbDriver, *dbDSN)
	// 代理抓包租约：gt-singbox-agent 不再随 pipeline 常驻拉起，
	// 由 CreateProxyLease 按用户/设备租约独立启动（见 proxy_lease.go）。
	engine.agentBin = *agentBin
	// 存量全局配置提示：proxy.json 已废弃（租约化改造），仅提示可删除，不加载。
	if _, err := os.Stat(filepath.Join(absWorkDir, "proxy.json")); err == nil {
		slog.Info("proxy.json is deprecated (mobile proxy now uses per-user leases); the file is no longer read and can be removed")
	}

	// AgentIngest gRPC server：gt-agent 推送本机原始帧的入口（团队协作模式）。
	// 默认 :9092；-agent-ingest-addr 传空字符串禁用。鉴权复用 pkg/auth：
	// GT_AUTH_TOKENS 未配置时为匿名模式，owner 统一为 "local"（auth.AnonymousOwner），
	// 与本地单机创建的会话归属一致。会话归属校验用 ControlStore 查 sessions.owner，
	// owner 不匹配的 batch 以 PermissionDenied 拒绝。
	var agentIngestGrpc *grpc.Server
	// probeAdmin 非 nil 时启用 CaptureControl 的探针管理 RPC（见下方注入）。
	var probeAdmin capturecontrol.ProbeAdmin
	if *agentIngestAddr != "" {
		agentHub := agent.NewHub()
		engine.SetAgentHub(agentHub)
		// 连接活性源与 hub 同源注入：有了它 GetStatus 才能区分
		//「agent 已连上但目标端口没流量」与「agent 压根没连上」。
		agentIngest := agent.NewIngestServer(agentHub, controlStoreSessionOwners{store: controlStore})
		engine.SetAgentLiveness(agentIngest)

		// 探针管理面（v2 探针优化）：注册 / 控制通道 / 三维度状态。
		// Manager 复用 ControlStore（probes 表）与 AgentIngest 的会话归属校验。
		probeMgr := probe.NewManager(controlStore, agentHub, controlStoreSessionOwners{store: controlStore},
			logging.With("component", "probe_manager"))
		engine.SetProbeManager(probeMgr)
		agentIngest.SetProbeAssignChecker(probeMgr)
		probeSrv := probe.NewServer(probeMgr, logging.With("component", "probe_server"))
		probeAdmin = probeMgr

		agentLis, err := internalipc.ListenAddr(*agentIngestAddr)
		if err != nil {
			slog.Error("listen agent ingest", "error", err)
			os.Exit(1)
		}
		config.WriteAddrFile(absWorkDir, "agent-ingest", agentLis.Addr().String())
		defer agentLis.Close()
		agentIngestGrpc = grpc.NewServer(
			grpc.ChainStreamInterceptor(auth.StreamInterceptor(authResolver)),
			// RegisterProbe 是 unary（探针 claim 后换发长期凭证），同样走 Bearer 鉴权。
			grpc.ChainUnaryInterceptor(auth.UnaryInterceptor(authResolver)),
		)
		proto.RegisterAgentIngestServer(agentIngestGrpc, agentIngest)
		// AgentControl 与 AgentIngest 同端口：探针控制通道不需要额外入站端口，
		// 凭证体系（probe_token）与数据面（Push）共用同一套 resolver 链。
		proto.RegisterAgentControlServer(agentIngestGrpc, probeSrv)
		go func() {
			if err := agentIngestGrpc.Serve(agentLis); err != nil {
				slog.Error("serve agent ingest", "error", err)
			}
		}()
		slog.Info("agent ingest listening", "addr", agentLis.Addr())
	}

	// CaptureControl gRPC server：供 gt-mcp / gt-trace 调用。
	// 默认 :8088（TCP），可通过 -control-addr 覆盖。
	//
	// 信任边界：本 server 的鉴权语义假设唯一客户端是同机 gt-mcp（其 HTTP 层
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
	ccServer := capturecontrol.NewServer(engine)
	if probeAdmin != nil {
		ccServer.SetProbeAdmin(probeAdmin)
	}
	pb.RegisterCaptureControlServer(grpcSrv, ccServer)

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
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutdownCancel()
		// 优雅停止所有抓包会话：cancel + 等待各自 finalize 写库（running→stopped），
		// 确保退出前本进程的活跃会话状态已落库。超时由 shutdownCtx 控制，不阻塞退出。
		// 代理租约会随各自会话的 finalize 自动回收（杀 agent + 释放端口）。
		engine.StopAll(shutdownCtx)
		// 租约关停兜底清扫：回收 finalize 未及处理的残留租约（幂等）。
		engine.CleanupProxyLeases()
		grpcSrv.GracefulStop()
		registryGrpc.GracefulStop()
		if agentIngestGrpc != nil {
			// GracefulStop 会等所有活跃 Push 流结束——gt-agent 的流是长连接，
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

	slog.Info("gt-pipeline starting",
		"workdir", absWorkDir,
		"socket", listener.Addr(),
		"registry_socket", registryLis.Addr(),
		"GT_REGISTRY_ADDR", regEndpoint,
	)
	if err := grpcSrv.Serve(listener); err != nil {
		slog.Error("serve", "error", err)
		os.Exit(1)
	}
}

// controlStoreSessionOwners 用 ControlStore 实现 agent.SessionOwnerChecker：
// 查 sessions.owner 做会话归属校验，避免 pkg/capture 反向依赖 pkg/store。
type controlStoreSessionOwners struct {
	store store.ControlStoreBackend
}

func (o controlStoreSessionOwners) SessionOwner(sessionID string) (string, bool) {
	meta, err := o.store.GetSession(context.Background(), sessionID)
	if err != nil || meta == nil {
		return "", false
	}
	return meta.Owner, true
}
