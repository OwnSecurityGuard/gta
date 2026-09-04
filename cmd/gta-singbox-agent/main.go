// Command gta-singbox-agent 是移动端流量入口（sing-box 侧）到 GTA 的桥接程序。
//
// 它是一个**常驻的手机出口**：监听地址在进程生命周期内固定不变，抓包与否
// 由 gta-pipeline 通过本地控制接口（--control）随时切换。手机配好一次代理
// （二维码）之后，反复开始/停止抓包都不需要重新扫码、不需要重连 VPN。
//
// 常驻模式（推荐，由 gta-pipeline 的代理租约拉起）：
//
//	gta-singbox-agent --listen 0.0.0.0:12100 --control 127.0.0.1:19500
//
//	启动后处于 idle：手机流量照常中继，但不抓包、不上报、无落盘。
//	pipeline 通过控制接口切到 capturing：
//	  POST /v1/capture/start {"capture_id":"<session>","server_addr":"127.0.0.1:19100"}
//	  POST /v1/capture/stop
//	  GET  /v1/status
//
// 直连模式（向后兼容，手动联调用）：给 --server 时启动即开始抓包，
// 等价于启动后立刻调一次 capture/start。
//
//	gta-singbox-agent --server 127.0.0.1:9090 --listen 127.0.0.1:12000
//
// 纯代理模式下 --target 省略：动态解析手机 CONNECT 请求中的目标地址，
// 适合"常驻等待代理连接"，GTA 未运行或未抓包时不影响监听。
//
// 联调模式（无 sing-box，本地模拟游戏流量）：
//
//	gta-singbox-agent --sim --target 127.0.0.1:19000
//
// 它只负责"中继 + 按需推送连接级数据"，不解码、不存储。
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
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
		listenFile  string
		control     string
		controlFile string
	)
	flag.StringVar(&server, "server", "", "GTA mobile capture source gRPC address (empty = start idle, wait for capture/start via --control)")
	flag.StringVar(&listen, "listen", "127.0.0.1:12000", "local address to accept game connections")
	flag.StringVar(&target, "target", "", "upstream game server address (optional: omit for pure HTTP CONNECT proxy mode)")
	flag.StringVar(&app, "app", "", "app/package name (optional)")
	flag.StringVar(&device, "device", "", "device id (optional)")
	flag.StringVar(&process, "process", "", "process name (optional)")
	flag.StringVar(&filterHosts, "filter-hosts", "", "comma-separated target hosts to capture (empty = all)")
	flag.StringVar(&filterPorts, "filter-ports", "", "comma-separated target ports to capture (empty = all)")
	flag.BoolVar(&sim, "sim", false, "run with a simulated game server+client (no sing-box needed)")
	flag.StringVar(&listenFile, "listen-file", "", "write the actual listen address to this file once bound (required when --listen uses port 0)")
	flag.StringVar(&control, "control", "", "local control API listen address (e.g. 127.0.0.1:19500); enables start/stop capture without restarting")
	flag.StringVar(&controlFile, "control-file", "", "write the actual control address to this file once bound (required when --control uses port 0)")
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

	fh := splitCSV(filterHosts)
	fp := parsePortList(filterPorts)

	cfg := agent.RelayConfig{
		ListenAddr:  listen,
		TargetAddr:  simTarget,
		App:         app,
		Device:      device,
		Process:     process,
		FilterHosts: fh,
		FilterPorts: fp,
	}

	// 抓包闸门：idle 起步，数据上报目标由控制接口切换（--server 给定时立即开启）。
	gate := agent.NewCaptureGate(cfg, logger)
	defer gate.Close()

	relay := agent.NewRelay(cfg, gate, logger)
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

	// 监听地址回写：--listen 用端口 0 时端口由内核分配，拉起本进程的 pipeline
	// 租约管理器无法预知，只能等绑定成功后回写文件再读回。
	if listenFile != "" {
		addr := listenAddr(relay, logger)
		if addr == "" {
			logger.Error("relay listen address unavailable", "listen_file", listenFile)
			os.Exit(1)
		}
		if err := writeFile(listenFile, addr); err != nil {
			logger.Error("write listen file failed", "path", listenFile, "error", err)
			os.Exit(1)
		}
	}

	// 控制接口：管道两侧（pipeline ↔ agent）都按固定地址通信，端口由 pipeline
	// 从端口段分配后经 --control 传入；端口为 0 时回写 --control-file 供外部读取。
	var ctrl *agent.ControlServer
	if control != "" {
		ctrl = agent.NewControlServer(control, gate, relay, logger)
		go func() {
			if err := ctrl.Serve(ctx); err != nil && ctx.Err() == nil {
				logger.Error("control server failed", "error", err)
				os.Exit(1)
			}
		}()
		// 控制端口是 pipeline 调 capture/start 的唯一入口，必须等到真正就绪
		// 再回写文件，否则调用方会拿到一个还连不上的地址。
		if controlFile != "" {
			if !waitFor(func() bool { return ctrl.Addr() != control && ctrl.Addr() != "" }) {
				logger.Error("control address unavailable", "control_file", controlFile)
				os.Exit(1)
			}
			if err := writeFile(controlFile, ctrl.Addr()); err != nil {
				logger.Error("write control file failed", "path", controlFile, "error", err)
				os.Exit(1)
			}
		}
	}

	// --server 显式给定时启动即抓包（直连模式，向后兼容手动联调用法）。
	// 常驻模式（由 pipeline 拉起）不给 --server，等控制指令。
	if server != "" {
		if err := gate.Start("", server, fh, fp); err != nil {
			logger.Error("start capture failed", "server", server, "error", err)
			os.Exit(1)
		}
		logger.Info("capture enabled at startup (direct mode)", "server", server)
	} else {
		logger.Info("started idle: relaying without capture; use the control API to start a capture session")
	}

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

// writeFile 原子写入文件内容（临时文件 + rename），避免读取方读到半截内容。
func writeFile(path, addr string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(addr+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// waitFor 轮询等待条件成立（最多 5 秒）。
func waitFor(cond func() bool) bool {
	for i := 0; i < 100; i++ {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
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
