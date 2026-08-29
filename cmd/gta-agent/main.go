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
	"log/slog"
	"os"
	"os/signal"
	"time"

	"gta/pkg/capture/agent/proto"

	"flag"
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
	_ = fs.Parse(os.Args[1:])

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

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

	// 1) 插件托管（无需抓包即可工作）。
	sup := &pluginSupervisor{dir: pluginDir, registryAddr: registry, token: token, bk: newBackoff()}
	sup.run(ctx)

	// 2) 抓包推流（可选）。
	if sessionID != "" && iface != "" {
		capCfg := captureConfig{Iface: iface, BPF: bpf, SnapLen: int32(snapLen), Promisc: promisc}
		packets := make(chan *proto.RawPacket, 1024)
		if err := runCapture(ctx, capCfg, packets); err != nil {
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
		go ic.run(ctx, packets)
	} else {
		slog.Info("capture disabled (start with --session and --iface to capture)")
	}

	<-ctx.Done()
	slog.Info("gta-agent shutting down")
	// 给子进程/流一小段收尾时间后退出（supervise/ingest 随 ctx 结束）。
	time.Sleep(300 * time.Millisecond)
	slog.Info("gta-agent stopped")
}
