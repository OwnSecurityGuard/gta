// Command gta-singbox-agent 是移动端流量入口（sing-box 侧）到 GTA 的桥接程序。
//
// 用法：
//
//	gta-singbox-agent \
//	  --server 127.0.0.1:9090 \   # GTA mobile capture source 的 gRPC 地址
//	  --listen 127.0.0.1:12000 \  # 本机监听地址（手机代理软件连这里）
//	  --target 1.2.3.4:443         # 上游服务器（可省略，见下）
//
// --target 可省略：省略时进入纯 HTTP CONNECT 代理模式，只监听并等待手机代理软件
// 发来 CONNECT 请求，动态解析目标地址后建立隧道。适合"常驻等待代理连接"的部署，
// GTA 未运行时不影响监听，GTA 上线后自动恢复推送。
//
// 联调模式（无 sing-box，本地模拟游戏流量）：
//
//	gta-singbox-agent --sim --target 127.0.0.1:19000
//
// 它只负责"中继 + 推送连接级数据"，不解码、不存储。
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gta/pkg/agent"
)

func main() {
	var (
		server      string
		listen      string
		target      string
		app         string
		device      string
		process     string
		filterHosts string
		filterPorts string
		sim         bool
	)
	flag.StringVar(&server, "server", "127.0.0.1:9090", "GTA mobile capture source gRPC address")
	flag.StringVar(&listen, "listen", "127.0.0.1:12000", "local address to accept game connections")
	flag.StringVar(&target, "target", "", "upstream game server address (optional: omit for pure HTTP CONNECT proxy mode)")
	flag.StringVar(&app, "app", "", "app/package name (optional)")
	flag.StringVar(&device, "device", "", "device id (optional)")
	flag.StringVar(&process, "process", "", "process name (optional)")
	flag.StringVar(&filterHosts, "filter-hosts", "", "comma-separated target hosts to capture (empty = all)")
	flag.StringVar(&filterPorts, "filter-ports", "", "comma-separated target ports to capture (empty = all)")
	flag.BoolVar(&sim, "sim", false, "run with a simulated game server+client (no sing-box needed)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --sim 且未给 --target 时，本地起一个模拟游戏服务端作为上游。
	simTarget := target
	if sim && simTarget == "" {
		lis, err := agent.EchoServer("127.0.0.1:0", logger)
		if err != nil {
			logger.Error("start sim game server failed", "error", err)
			os.Exit(1)
		}
		defer lis.Close()
		simTarget = lis.Addr().String()
		logger.Info("sim mode: upstream is simulated game server", "target", simTarget)
	}
	// --target 可省略：省略时进入纯 HTTP CONNECT 代理模式，常驻等待手机代理软件的
	// CONNECT 请求并动态解析目标（Relay 的 TargetAddr 为空即进入该模式）。
	if simTarget == "" {
		logger.Info("pure HTTP CONNECT proxy mode: listening, waiting for phone proxy connections (target resolved per CONNECT)")
	}

	client, err := agent.NewPushClient(server, logger)
	if err != nil {
		logger.Error("connect to GTA mobile source failed", "error", err)
		os.Exit(1)
	}
	defer client.Close()

	fh := splitCSV(filterHosts)
	fp := parsePortList(filterPorts)
	relay := agent.NewRelay(agent.RelayConfig{
		ListenAddr:  listen,
		TargetAddr:  simTarget,
		App:         app,
		Device:      device,
		Process:     process,
		FilterHosts: fh,
		FilterPorts: fp,
	}, client, logger)
	if len(fh) > 0 || len(fp) > 0 {
		logger.Info("connection filter enabled (unmatched connections relay-only, not captured)",
			"filter_hosts", filterHosts, "filter_ports", filterPorts)
	}

	go func() {
		if err := relay.Serve(ctx); err != nil && ctx.Err() == nil {
			logger.Error("relay serve failed", "error", err)
			os.Exit(1)
		}
	}()

	if sim {
		go runSimClientLoop(ctx, logger, relay)
	}

	<-ctx.Done()
	logger.Info("gta-singbox-agent shutting down")
}

// runSimClientLoop 周期性让模拟客户端走一遍中继链路（每条连接 4 条消息）。
func runSimClientLoop(ctx context.Context, logger *slog.Logger, relay *agent.Relay) {
	messages := [][]byte{
		[]byte(`{"msg":"login","user":"alice"}`),
		[]byte(`{"msg":"move","x":1,"y":2}`),
		[]byte(`{"msg":"attack","target":3}`),
		[]byte(`{"msg":"logout"}`),
	}
	relayAddr := listenAddr(relay, logger)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		if err := agent.RunSimClient(relayAddr, messages, logger); err != nil {
			logger.Warn("sim client round failed", "error", err)
			continue
		}
		logger.Info("sim client round completed", "relay", relayAddr)
	}
}

func listenAddr(relay *agent.Relay, logger *slog.Logger) string {
	for i := 0; i < 100; i++ {
		if a := relay.Addr(); a != nil {
			return a.String()
		}
		time.Sleep(50 * time.Millisecond)
	}
	logger.Warn("relay addr not ready, falling back to configured listen addr")
	return ""
}

// splitCSV 将逗号分隔的字符串拆分为去空白、去空项的切片。
func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// parsePortList 解析逗号分隔的端口列表；非法项忽略。
func parsePortList(s string) []int {
	parts := splitCSV(s)
	if len(parts) == 0 {
		return nil
	}
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			continue
		}
		out = append(out, n)
	}
	return out
}
