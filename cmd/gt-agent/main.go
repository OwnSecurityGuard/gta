// gt-agent 是成员机上的常驻探针（v2 探针优化，docs/plans/2026-09-05）：
//
//  1. 抓包推流：本机网卡抓包（需 -tags pcap 编译），经 spool 落盘后推送到
//     gt-pipeline 的 AgentIngest server（默认 :9092），归档模式下按留存策略
//     在本机长期保留（Ack 不删段），支持按时间窗回放导入平台；
//  2. 控制面：本地回环 HTTP（127.0.0.1:19500，给坐在机器前的人/脚本）+
//     远端 AgentControl 双向流（desired-state 对齐，平台页面直接操控）；
//  3. 托管本地插件：发现本机插件进程并以隧道模式拉起。
//
// 身份与回连存 probe.json（首启引导：命令行 flag / 固化配置 / 启动码）；
// 抓包参数是会话级配置，由平台指派或本地控制面临时给定，不落 probe.json。
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gametrace/pkg/version"
)

// embeddedAgentConfig 是下载形态 agent 经由 go:embed 烧进二进制的固化配置。
// 其中 Server 取其 registry 端口（host:9091），ingest 由 deriveAddrs 自动取 port+1。
// Iface 通常为空——目标机器网卡名无法预知，运行时自动探测默认网卡。
type embeddedAgentConfig struct {
	Server       string   `json:"server,omitempty"`        // 回连服务端 host:port（port 为 registry 端口）
	RegistryAddr string   `json:"registry_addr,omitempty"` // 可选：显式覆盖 registry 地址
	IngestAddr   string   `json:"ingest_addr,omitempty"`   // 可选：显式覆盖 ingest 推流地址
	Token        string   `json:"token,omitempty"`         // 团队 token（gt_xxx）；空为匿名
	SessionID    string   `json:"session,omitempty"`       // 目标抓包会话 id（服务端接收端关联）
	Iface        string   `json:"iface,omitempty"`         // 预留：抓包网卡名（默认自动探测）
	BPF          string   `json:"bpf,omitempty"`           // BPF 过滤表达式（端口已换算）
	PluginDir    string   `json:"plugin_dir,omitempty"`    // 本地插件发现根目录
	SpoolDir     string   `json:"spool_dir,omitempty"`     // 上行链路磁盘缓冲目录（断电续传）；空=自动取用户缓存目录
	BindPlugins  []string `json:"plugin_names,omitempty"`  // 仅托管这些名字的本地插件（空=托管全部）
}

func main() {
	var (
		server        string
		registryAddr  string
		ingestAddr    string
		token         string
		sessionID     string
		iface         string
		bpf           string
		pluginDir     string
		batchSize     int
		batchInterval time.Duration
		spoolDir      string
		snapLen       int
		promisc       bool
		accessCode    string
		accessHost    string
	)
	fs := flag.NewFlagSet("gt-agent", flag.ExitOnError)
	fs.StringVar(&server, "server", "", "pipeline 服务端基址 host 或 host:port（port 为 registry 端口，ingest 自动取 port+1）")
	fs.StringVar(&registryAddr, "registry-addr", "", "插件注册地址覆盖（默认由 --server 推导，如 host:9091）")
	fs.StringVar(&ingestAddr, "ingest-addr", "", "AgentIngest 推流地址覆盖（默认由 --server 推导，如 host:9092）")
	fs.StringVar(&token, "token", "", "团队 token（gt_xxx）；留空为匿名模式（服务端 owner=local）")
	fs.StringVar(&sessionID, "session", "", "目标抓包会话 id；留空则抓包由控制面/平台指令启动")
	fs.StringVar(&iface, "iface", "", "抓包网卡名；--session 留空时忽略")
	fs.StringVar(&bpf, "filter", "", "BPF 过滤表达式（控制上行带宽）")
	fs.StringVar(&pluginDir, "plugin-dir", "plugins", "本地插件发现根目录")
	fs.IntVar(&batchSize, "batch-size", 128, "推流批大小（包数阈值）")
	fs.DurationVar(&batchInterval, "batch-interval", 200*time.Millisecond, "推流批时间阈值（低流量兜底刷批间隔）")
	fs.StringVar(&spoolDir, "spool-dir", "", "上行链路磁盘缓冲目录根（断电续传+留存）；留空自动取 <用户缓存>/gt-agent/spool")
	fs.IntVar(&snapLen, "snaplen", 1600, "pcap snaplen")
	fs.BoolVar(&promisc, "promisc", true, "混杂模式")
	fs.StringVar(&accessCode, "code", "", "启动码 GT-XXXX-XXXX：无 server/token 时用它自动领取配置并回连")
	fs.StringVar(&accessHost, "mcp", "127.0.0.1:8781", "服务端 MCP HTTP 地址（启动码领取用）")
	showVersion := fs.Bool("version", false, "print version and exit")
	_ = fs.Parse(os.Args[1:])
	if *showVersion {
		fmt.Println("gt-agent " + version.String())
		return
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// ---- 身份与回连的装配（优先级：flag > probe.json > 固化配置 > 启动码）----
	cfg, cfgLoaded := loadAgentConfig()
	if !cfgLoaded {
		// 首启：注册探针默认开启归档留存（24h / 4GB，可用控制面/远端指令调整）。
		cfg.Archive.Enabled = true
	}

	// 固化配置（-tags embedded 下载形态）或 sidecar config.embedded.json：
	// 仅当更高优先级来源没有给值时作为默认值。
	embedded, hasEmbedded := loadEmbeddedConfig()
	if !hasEmbedded {
		if sc, ok := loadSidecarConfig(); ok {
			embedded, hasEmbedded = sc, true
		}
	}

	// 启动码：显式传 --code，或既无 server/token 又无固化配置（首启引导）时，
	// 用码自动领取 server/token/session 等作为默认配置。
	hasClaimed := false
	var bindFromClaim []string
	effServer := firstNonEmpty(server, cfg.Server, embeddedStr(embedded, "server"))
	effToken := firstNonEmpty(token, cfg.UserToken, embeddedStr(embedded, "token"))
	if accessCode != "" || (effServer == "" && effToken == "" && !hasEmbedded) {
		if accessCode == "" {
			// 交互式：stdin 读一行（首启引导）。非 TTY 下读 os.Stdin。
			fmt.Print("请输入启动码 GT-XXXX-XXXX: ")
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			accessCode = strings.ToUpper(strings.TrimSpace(line))
		}
		accessCode = strings.ToUpper(strings.TrimSpace(accessCode))
		if accessCode == "" {
			slog.Error("启动码不能为空；用 --code GT-XXXX-XXXX 或输入启动码")
			os.Exit(1)
		}
		if !strings.HasPrefix(accessCode, "GT-") {
			slog.Error("启动码格式应为 GT-XXXX-XXXX", "code", accessCode)
			os.Exit(1)
		}
		claimed, err := claimAccessCode(context.Background(), accessHost, accessCode)
		if err != nil {
			slog.Error("领取启动码失败（请确认 --mcp <host:8781> 可达且码有效）", "error", err)
			os.Exit(1)
		}
		bindFromClaim = claimed.BindPlugins
		if sessionID == "" {
			sessionID = claimed.SessionID
		}
		if bpf == "" {
			bpf = claimed.BPF
		}
		// 领取到的身份与回连落 probe.json（此后改参走控制面，不再依赖启动码）。
		if cfg.Server == "" {
			cfg.Server = claimed.Server
		}
		if cfg.UserToken == "" {
			cfg.UserToken = claimed.Token
		}
		if cfg.RegistryAddr == "" {
			cfg.RegistryAddr = claimed.RegistryAddr
		}
		if cfg.IngestAddr == "" {
			cfg.IngestAddr = claimed.IngestAddr
		}
		hasClaimed = true
		slog.Info("access code claimed", "code", accessCode, "session", claimed.SessionID)
	}

	// 命令行 flag 非空时覆盖 probe.json 并写回（首启引导一次性生效；
	// 此后一切改参走本地控制面 / 远端指令，不再需要命令行）。
	dirty := false
	mergeFlag(&cfg.Server, server, &dirty)
	mergeFlag(&cfg.UserToken, token, &dirty)
	mergeFlag(&cfg.RegistryAddr, registryAddr, &dirty)
	mergeFlag(&cfg.IngestAddr, ingestAddr, &dirty)
	// 固化配置仍是最初的兜底（仅当 cfg 仍为空）。
	if hasEmbedded && embedded != nil {
		if cfg.Server == "" {
			cfg.Server = embedded.Server
			dirty = true
		}
		if cfg.UserToken == "" {
			cfg.UserToken = embedded.Token
			dirty = true
		}
		if cfg.RegistryAddr == "" {
			cfg.RegistryAddr = embedded.RegistryAddr
			dirty = true
		}
		if cfg.IngestAddr == "" {
			cfg.IngestAddr = embedded.IngestAddr
			dirty = true
		}
		if pluginDir == "plugins" && embedded.PluginDir != "" {
			pluginDir = embedded.PluginDir
		}
		if spoolDir == "" {
			spoolDir = embedded.SpoolDir
		}
	}
	if dirty {
		if err := saveAgentConfig(cfg); err != nil {
			slog.Warn("save probe.json failed (continuing with in-memory config)", "error", err)
		}
	}

	// 参数校验：非法值 fail-fast，避免静默错误行为。
	if batchSize <= 0 {
		slog.Error("--batch-size must be positive", "value", batchSize)
		os.Exit(1)
	}
	if batchInterval <= 0 {
		slog.Error("--batch-interval must be positive", "value", batchInterval)
		os.Exit(1)
	}
	if snapLen <= 0 {
		slog.Error("--snaplen must be positive", "value", snapLen)
		os.Exit(1)
	}

	if spoolDir == "" {
		spoolDir = spoolBase()
	}
	spoolDir = filepath.Clean(spoolDir)
	spoolBaseCustom = spoolDir

	// 无任何回连目标：本地单机模式（本地控制面可用，插件托管与远端控制不启用）。
	localOnly := cfg.Server == "" && cfg.RegistryAddr == "" && cfg.IngestAddr == ""
	var registry, ingest string
	var err error
	if !localOnly {
		var derr error
		if registry, ingest, derr = deriveAddrs(cfg.Server, cfg.RegistryAddr, cfg.IngestAddr); derr != nil {
			slog.Error("address configuration error", "error", derr)
			os.Exit(1)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, osKillSignal)
	defer stop()

	slog.Info("gt-agent starting",
		"registry", registry, "ingest", ingest,
		"user_token_set", cfg.UserToken != "", "probe_id", cfg.ProbeID,
		"session", sessionID, "iface", iface,
		"plugin_dir", pluginDir, "spool", spoolDir,
		"archive", cfg.Archive.Enabled, "batch_size", batchSize,
	)

	var wg sync.WaitGroup

	// 1) 抓包状态机 + 归档器（无论是否立即抓包都常驻，等控制面/平台指令）。
	runner := newCaptureRunner()
	runner.batchSize = batchSize
	runner.batchInterval = batchInterval
	runner.setRetention(retentionFrom(cfg))

	arch := newArchiver(runner, cfg)
	wg.Add(1)
	go func() {
		defer wg.Done()
		arch.Run(ctx)
	}()

	// 2) 本地控制面（回环 HTTP；坐在这台机器前的人/脚本用）。
	lc := newLocalControl(runner, cfg, ingest)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := lc.Serve(ctx, "127.0.0.1:19500"); err != nil {
			slog.Warn("local control stopped", "error", err)
		}
	}()

	// 3) 插件托管（无需抓包即可工作）。固化/启动码模式下仅托管白名单内的插件。
	// localOnly 时没有 registry 可注册，跳过。
	var bind []string
	switch {
	case hasClaimed:
		bind = bindFromClaim
	case hasEmbedded && embedded != nil:
		bind = embedded.BindPlugins
	}
	if !localOnly {
		sup := &pluginSupervisor{dir: pluginDir, registryAddr: registry, token: cfg.UserToken, bind: bind}
		sup.run(ctx, &wg)
		slog.Info("plugin supervisor configured", "bind_plugins", bind)
	}

	// 4) 探针注册 + 远端控制通道。匿名（无凭证也注册不了）时跳过：
	// 本地控制面照常可用，等带 token 重启后再接入平台。
	registered := false
	if !localOnly {
		registered, err = ensureRegistered(ctx, cfg, ingest)
		if err != nil {
			slog.Warn("probe registration failed; remote control disabled for now", "error", err)
			registered = false
		}
	}
	if registered {
		ca := NewControlAgent(ingest, cfg.ProbeID, cfg.ProbeToken, runner, cfg, arch)
		ca.onCfgSaved = arch.applyRetention // archive_* 配置变更立即生效
		wg.Add(1)
		go func() {
			defer wg.Done()
			ca.Run(ctx)
		}()
	} else {
		slog.Info("probe not registered (anonymous local mode): remote control disabled")
	}

	// 5) 命令行直启抓包（向后兼容一次性脚本用法；正常运行由控制面/平台启动）。
	// 固化模式的 session/iface/bpf 也走这里（下载形态免参数开机即抓）。
	if sessionID == "" && hasEmbedded && embedded != nil {
		sessionID = embedded.SessionID
		if iface == "" {
			iface = embedded.Iface
		}
		if bpf == "" {
			bpf = embedded.BPF
		}
	}
	if sessionID != "" {
		if iface == "" {
			iface, err = resolveDefaultIface()
			if err != nil {
				slog.Error("capture requires a network interface, but none could be resolved", "error", err)
				os.Exit(1)
			}
		}
		pushToken := cfg.UserToken
		if registered {
			pushToken = cfg.ProbeToken
		}
		if err := runner.Start(CaptureParams{
			SessionID: sessionID, Iface: iface, BPF: bpf,
			SnapLen: int32(snapLen), Promisc: promisc,
		}, ingest, pushToken); err != nil {
			slog.Error("initial capture failed to start (probe stays up; start via local control or platform)", "error", err)
		}
	}

	<-ctx.Done()
	slog.Info("gt-agent shutting down")
	// 等待归档器、本地控制面、插件监督与控制通道 goroutine 收尾，
	// 上限略大于 ackTimeout，超时则放弃等待直接退出。
	joined := make(chan struct{})
	go func() {
		wg.Wait()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(2 * ackTimeout):
		slog.Warn("shutdown timeout: some goroutines did not stop in time")
	}
	// 收尾刷盘并释放 spool 句柄；磁盘上的数据保留（未确认续传 / 归档留存）。
	if err := runner.Close(); err != nil {
		slog.Warn("close capture spool", "error", err)
	}
	slog.Info("gt-agent stopped")
}

// firstNonEmpty 返回第一个非空参数（全空返回空串）。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// defaultSpoolDir 返回某会话的 spool 目录（spoolBase 下按会话隔离，
// 归档扫描与断电续传共用同一目录布局）。
func defaultSpoolDir(sessionID string) string {
	return filepath.Join(spoolBase(), sessionID)
}

// embeddedStr 安全读取可能为 nil 的固化配置字段。
func embeddedStr(e *embeddedAgentConfig, field string) string {
	if e == nil {
		return ""
	}
	switch field {
	case "server":
		return e.Server
	case "token":
		return e.Token
	}
	return ""
}
