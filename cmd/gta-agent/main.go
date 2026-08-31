// gta-agent 是团队成员本机一键启动的单二进制 agent：
//
//  1. 抓包推流：本机网卡抓包（需 -tags pcap 编译），按批推送到
//     gta-pipeline 的 AgentIngest server（默认 :9092），保留完整帧与 link_type；
//  2. 托管本地插件：发现本机插件进程并以隧道模式拉起
//     （GTA_TUNNEL=1 + GTA_REGISTRY_ADDR + GTA_AUTH_TOKEN 注入），
//     插件经 SDK RunRegisterLoopWithOptions 自动注册到共享服务端。
//
// 用法：gta-agent --token gta_xxx --server host[:port] [--session <id> --iface <name>]
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"gta/pkg/capture/agent/proto"
	"gta/pkg/version"
)

// embeddedAgentConfig 是下载形态 agent 经由 go:embed 烧进二进制的固化配置。
// 其中 Server 取其 registry 端口（host:9091），ingest 由 deriveAddrs 自动取 port+1。
// Iface 通常为空——目标机器网卡名无法预知，运行时自动探测默认网卡。
type embeddedAgentConfig struct {
	Server       string   `json:"server,omitempty"`        // 回连服务端 host:port（port 为 registry 端口）
	RegistryAddr string   `json:"registry_addr,omitempty"` // 可选：显式覆盖 registry 地址
	IngestAddr   string   `json:"ingest_addr,omitempty"`   // 可选：显式覆盖 ingest 推流地址
	Token        string   `json:"token,omitempty"`         // 团队 token（gta_xxx）；空为匿名
	SessionID    string   `json:"session,omitempty"`       // 目标抓包会话 id（服务端接收端关联）
	Iface        string   `json:"iface,omitempty"`         // 预留：抓包网卡名（默认自动探测）
	BPF          string   `json:"bpf,omitempty"`           // BPF 过滤表达式（端口已换算）
	PluginDir    string   `json:"plugin_dir,omitempty"`    // 本地插件发现根目录
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
		snapLen       int
		promisc       bool
		accessCode    string
		accessHost    string
	)
	fs := flag.NewFlagSet("gta-agent", flag.ExitOnError)
	fs.StringVar(&server, "server", "", "pipeline 服务端基址 host 或 host:port（port 为 registry 端口，ingest 自动取 port+1）")
	fs.StringVar(&registryAddr, "registry-addr", "", "插件注册地址覆盖（默认由 --server 推导，如 host:9091）")
	fs.StringVar(&ingestAddr, "ingest-addr", "", "AgentIngest 推流地址覆盖（默认由 --server 推导，如 host:9092）")
	fs.StringVar(&token, "token", "", "团队 token（gta_xxx）；留空为匿名模式（服务端 owner=local）")
	fs.StringVar(&sessionID, "session", "", "目标抓包会话 id；留空禁用抓包（仅托管插件）")
	fs.StringVar(&iface, "iface", "", "抓包网卡名；--session 留空时忽略")
	fs.StringVar(&bpf, "filter", "", "BPF 过滤表达式（控制上行带宽）")
	fs.StringVar(&pluginDir, "plugin-dir", "plugins", "本地插件发现根目录")
	fs.IntVar(&batchSize, "batch-size", 128, "推流批大小（包数阈值）")
	fs.DurationVar(&batchInterval, "batch-interval", 200*time.Millisecond, "推流批时间阈值（低流量兜底刷批间隔）")
	fs.IntVar(&snapLen, "snaplen", 1600, "pcap snaplen")
	fs.BoolVar(&promisc, "promisc", true, "混杂模式")
	fs.StringVar(&accessCode, "code", "", "启动码 GTA-XXXX-XXXX：无 server/token 时用它自动领取配置并回连")
	fs.StringVar(&accessHost, "mcp", "127.0.0.1:8781", "服务端 MCP HTTP 地址（启动码领取用）")
	showVersion := fs.Bool("version", false, "print version and exit")
	_ = fs.Parse(os.Args[1:])
	if *showVersion {
		fmt.Println("gta-agent " + version.String())
		return
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// 固化配置（-tags embedded 下载形态）：任何命令行参数为空时以其作为默认值。
	// 这样下载回来的 agent 无需填任何参数（含 token）即可回连、托管插件并抓包。
	embedded, hasEmbedded := loadEmbeddedConfig()
	// 预置（多平台下载）产物未带 embedded 标签：运行时从可执行文件同目录载入
	// config.embedded.json（download zip 里同放的 sidecar 配置），保持免参数行为。
	if !hasEmbedded {
		if sc, ok := loadSidecarConfig(); ok {
			embedded, hasEmbedded = sc, true
		}
	}
	if hasEmbedded && embedded != nil {
		if server == "" {
			server = embedded.Server
		}
		if registryAddr == "" {
			registryAddr = embedded.RegistryAddr
		}
		if ingestAddr == "" {
			ingestAddr = embedded.IngestAddr
		}
		if token == "" {
			token = embedded.Token
		}
		if sessionID == "" {
			sessionID = embedded.SessionID
		}
		if iface == "" {
			iface = embedded.Iface
		}
		if bpf == "" {
			bpf = embedded.BPF
		}
		if pluginDir == "" {
			pluginDir = embedded.PluginDir
		}
	}

	// 启动码：显式传 --code，或既无 server/token 又无固化配置（首启引导）时，
	// 用码自动领取 server/token/session 等作为默认配置。领取到的配置优先级最
	// 低于命令行参数（命令行以空串判断覆盖），避免显式传入被启动码覆盖。
	hasClaimed := false
	var bindFromClaim []string
	if accessCode != "" || (server == "" && token == "" && !hasEmbedded) {
		if accessCode == "" {
			// 交互式：stdin 读一行（首启引导）。非 TTY 下读 os.Stdin。
			fmt.Print("请输入启动码 GTA-XXXX-XXXX: ")
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			accessCode = strings.ToUpper(strings.TrimSpace(line))
		}
		accessCode = strings.ToUpper(strings.TrimSpace(accessCode))
		if accessCode == "" {
			slog.Error("启动码不能为空；用 --code GTA-XXXX-XXXX 或输入启动码")
			os.Exit(1)
		}
		if !strings.HasPrefix(accessCode, "GTA-") {
			slog.Error("启动码格式应为 GTA-XXXX-XXXX", "code", accessCode)
			os.Exit(1)
		}
		claimed, err := claimAccessCode(context.Background(), accessHost, accessCode)
		if err != nil {
			slog.Error("领取启动码失败（请确认 --mcp <host:8781> 可达且码有效）", "error", err)
			os.Exit(1)
		}
		if server == "" {
			server = claimed.Server
		}
		if registryAddr == "" {
			registryAddr = claimed.RegistryAddr
		}
		if ingestAddr == "" {
			ingestAddr = claimed.IngestAddr
		}
		if token == "" {
			token = claimed.Token
		}
		if sessionID == "" {
			sessionID = claimed.SessionID
		}
		if bpf == "" {
			bpf = claimed.BPF
		}
		bindFromClaim = claimed.BindPlugins
		hasClaimed = true
		slog.Info("access code claimed", "code", accessCode, "session", claimed.SessionID)
	}

	// 参数校验：非法值 fail-fast，避免静默错误行为。
	// 固化模式下 session 自带、网卡可自动探测，故不再强制 --iface。
	if sessionID != "" && iface == "" && !hasEmbedded && !hasClaimed {
		slog.Error("--iface is required when --session is set (capture target)")
		os.Exit(1)
	}
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

	registry, ingest, err := deriveAddrs(server, registryAddr, ingestAddr)
	if err != nil {
		slog.Error("address configuration error", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, osKillSignal)
	defer stop()

	slog.Info("gta-agent starting",
		"registry", registry, "ingest", ingest,
		"token_set", token != "", "session", sessionID, "iface", iface,
		"plugin_dir", pluginDir, "batch_size", batchSize, "batch_interval", batchInterval,
	)

	var wg sync.WaitGroup

	// 1) 插件托管（无需抓包即可工作）。固化/启动码模式下仅托管白名单内的插件。
	var bind []string
	switch {
	case hasClaimed:
		bind = bindFromClaim
	case hasEmbedded && embedded != nil:
		bind = embedded.BindPlugins
	}
	sup := &pluginSupervisor{dir: pluginDir, registryAddr: registry, token: token, bind: bind}
	sup.run(ctx, &wg)
	slog.Info("plugin supervisor configured", "bind_plugins", bind)

	// 2) 抓包推流（可选）。固化模式未固化网卡名时自动探测默认网卡。
	if sessionID != "" {
		if iface == "" {
			iface, err = resolveDefaultIface()
			if err != nil {
				slog.Error("capture requires a network interface, but none could be resolved", "error", err)
				os.Exit(1)
			}
		}
		capCfg := captureConfig{Iface: iface, BPF: bpf, SnapLen: int32(snapLen), Promisc: promisc}
		packets := make(chan *proto.RawPacket, 1024)
		capEnded := make(chan error, 1)
		if err := runCapture(ctx, capCfg, packets, capEnded); err != nil {
			slog.Error("capture unavailable, exiting (plugin hosting only mode requires dropping --session/--iface)", "error", err)
			os.Exit(1)
		}
		ic := &ingestClient{
			addr:          ingest,
			token:         token,
			sessionID:     sessionID,
			iface:         iface,
			batchSize:     batchSize,
			batchInterval: batchInterval,
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ic.run(ctx, packets)
		}()
		// 抓包 goroutine 意外死亡（网卡关闭/pcap 错误）时不能静默挂着：
		// 记录错误并触发整体停机，让用户看到原因。
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-ctx.Done():
			case err := <-capEnded:
				if err == nil {
					return
				}
				slog.Error("capture ended unexpectedly, stopping agent", "error", err)
				stop()
			}
		}()
	} else {
		slog.Info("capture disabled (start with --session and --iface to capture)")
	}

	<-ctx.Done()
	slog.Info("gta-agent shutting down")
	// 等待插件监督与推流 goroutine 收尾（插件 kill、尾批 flush、PushAck 汇总），
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
	slog.Info("gta-agent stopped")
}
