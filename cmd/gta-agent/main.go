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
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"time"

	"gta/pkg/capture/agent/proto"
	"gta/pkg/version"
)

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
	showVersion := fs.Bool("version", false, "print version and exit")
	_ = fs.Parse(os.Args[1:])
	if *showVersion {
		fmt.Println("gta-agent " + version.String())
		return
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// 参数校验：非法值 fail-fast，避免静默错误行为。
	if sessionID != "" && iface == "" {
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

	// 1) 插件托管（无需抓包即可工作）。
	sup := &pluginSupervisor{dir: pluginDir, registryAddr: registry, token: token}
	sup.run(ctx, &wg)

	// 2) 抓包推流（可选）。
	if sessionID != "" && iface != "" {
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
