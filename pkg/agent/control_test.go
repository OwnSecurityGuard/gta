package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"gta/pkg/capture"
	"gta/pkg/capture/mobile"
	"gta/pkg/event"
)

// pktCollector 后台收集一个 source 的全部 packet，供断言"收到了什么/没收到什么"。
type pktCollector struct {
	mu   sync.Mutex
	pkts []event.Packet
}

func (c *pktCollector) run(ch <-chan event.Packet) {
	for p := range ch {
		c.mu.Lock()
		c.pkts = append(c.pkts, p)
		c.mu.Unlock()
	}
}

func (c *pktCollector) snapshot() []event.Packet {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]event.Packet(nil), c.pkts...)
}

// waitFor 轮询等待收集到至少 n 个包，超时则失败。
func (c *pktCollector) waitFor(t *testing.T, n int, d time.Duration) []event.Packet {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if got := c.snapshot(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %d packets, got %d", n, len(c.snapshot()))
	return nil
}

// openMobileSource 起一个 mobile capture source 并返回其监听地址。
func openMobileSource(t *testing.T) (capture.Source, string, *pktCollector) {
	t.Helper()
	src, err := capture.Open(context.Background(), "mobile", mobile.MobileConfig{ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("open mobile source: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })
	addr := src.(addrSource).Addr().String()
	col := &pktCollector{}
	go col.run(src.Packets())
	return src, addr, col
}

// relayEnv 是常驻 agent 的测试环境：idle 启动的 relay + 上游 echo server。
type relayEnv struct {
	relayAddr string
	echoAddr  string
	gate      *CaptureGate
	cancel    context.CancelFunc
}

// startIdleRelay 以「不抓包」状态启动一个常驻 relay（模拟刚创建的租约）。
func startIdleRelay(t *testing.T, logger *slog.Logger) *relayEnv {
	t.Helper()
	echoLis, err := EchoServer("127.0.0.1:0", logger)
	if err != nil {
		t.Fatalf("start echo server: %v", err)
	}
	t.Cleanup(func() { _ = echoLis.Close() })

	cfg := RelayConfig{ListenAddr: "127.0.0.1:0"}
	gate := NewCaptureGate(cfg, logger)
	t.Cleanup(func() { _ = gate.Close() })
	relay := NewRelay(cfg, gate, logger)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = relay.Serve(ctx) }()

	var relayAddr string
	deadline := time.Now().Add(3 * time.Second)
	for relayAddr == "" {
		if a := relay.Addr(); a != nil {
			relayAddr = a.String()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("relay not ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return &relayEnv{relayAddr: relayAddr, echoAddr: echoLis.Addr().String(), gate: gate, cancel: cancel}
}

// tunnel 经 relay 建立一条 CONNECT 隧道（模拟手机代理软件的一条连接）。
func (e *relayEnv) tunnel(t *testing.T, target string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", e.relayAddr)
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := io.WriteString(conn, fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	resp := make([]byte, 128)
	if _, err := conn.Read(resp); err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	return conn
}

// roundTrip 在已建立的隧道上发一帧并读回声，返回本轮写出的完整帧字节。
// 它同时证明「中继仍然通畅」——抓包开关不应影响手机侧连接。
func roundTrip(t *testing.T, conn net.Conn, payload []byte) {
	t.Helper()
	if err := WriteFrame(conn, payload); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	got, err := ReadFrame(conn)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("empty echo response")
	}
}

// TestCaptureGateToggle 是本次常驻化改造的核心回归：
//
//	手机连接（跨多次 start/stop 不中断）
//	  ├─ idle       → 只中继，零上报
//	  ├─ start → A  → 数据进会话 A
//	  ├─ start → B  → 数据改道进会话 B，A 立刻收不到新数据（会话隔离）
//	  └─ stop       → 又回到零上报，但中继照常
//
// 每一步都断言中继仍然通畅：抓包开关绝不能影响手机侧的 VPN 连接。
func TestCaptureGateToggle(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(testWriter{t}, nil))

	_, srcAAddr, colA := openMobileSource(t)
	_, srcBAddr, colB := openMobileSource(t)

	env := startIdleRelay(t, logger)
	conn := env.tunnel(t, env.echoAddr)

	// 1. idle：中继正常，两个会话都不该有任何数据。
	roundTrip(t, conn, []byte(`{"seq":0}`))
	if n := len(colA.snapshot()); n != 0 {
		t.Fatalf("idle: session A got %d packets, want 0", n)
	}
	if n := len(colB.snapshot()); n != 0 {
		t.Fatalf("idle: session B got %d packets, want 0", n)
	}
	if got := env.gate.State(); got != "idle" {
		t.Fatalf("gate state = %s, want idle", got)
	}

	// 2. start → A：已建立（早于 capture）的连接必须能进入会话 A。
	if err := env.gate.Start("session-A", srcAAddr, nil, nil); err != nil {
		t.Fatalf("start capture A: %v", err)
	}
	roundTrip(t, conn, []byte(`{"seq":1}`))
	pktsA := colA.waitFor(t, 1, 3*time.Second)
	if got := connIDOf(t, pktsA[0]); got == "" {
		t.Fatal("session A packet missing conn_id")
	}
	epochA := epochSuffix(t, pktsA[0])

	// 3. start → B：数据改道；会话 A 不得再收到新数据（新旧会话完全隔离）。
	if err := env.gate.Start("session-B", srcBAddr, nil, nil); err != nil {
		t.Fatalf("start capture B: %v", err)
	}
	beforeA := len(colA.snapshot())
	roundTrip(t, conn, []byte(`{"seq":2}`))
	pktsB := colB.waitFor(t, 1, 3*time.Second)
	epochB := epochSuffix(t, pktsB[0])
	if epochA == epochB {
		t.Fatalf("conn_id epoch not bumped across captures: %q", epochA)
	}
	// A 侧不应再增长（留一点时间让在途数据落定）。
	time.Sleep(200 * time.Millisecond)
	if after := len(colA.snapshot()); after != beforeA {
		t.Fatalf("session A leaked %d packets after switching to B (before=%d after=%d)",
			after-beforeA, beforeA, after)
	}

	// 4. stop：回到零上报，但中继仍然通畅（VPN 不需要重连）。
	if err := env.gate.Stop(); err != nil {
		t.Fatalf("stop capture: %v", err)
	}
	beforeB := len(colB.snapshot())
	roundTrip(t, conn, []byte(`{"seq":3}`))
	time.Sleep(200 * time.Millisecond)
	if after := len(colB.snapshot()); after != beforeB {
		t.Fatalf("session B got %d packets after stop, want 0", after-beforeB)
	}
	if got := env.gate.State(); got != "idle" {
		t.Fatalf("gate state = %s, want idle", got)
	}

	// 5. 反复 start/stop：第二轮必须再次正常抓包（支持反复开始/停止）。
	if err := env.gate.Start("session-C", srcAAddr, nil, nil); err != nil {
		t.Fatalf("restart capture: %v", err)
	}
	roundTrip(t, conn, []byte(`{"seq":4}`))
	all := colA.waitFor(t, beforeA+1, 3*time.Second)
	last := all[len(all)-1]
	if got := epochSuffix(t, last); got == epochA {
		t.Fatalf("second capture reused epoch %q, want a fresh one", got)
	}

	// 中继统计与抓包统计必须能区分「代理通了但没在抓」。
	st := env.gate.Snapshot(env.relayAddr, "")
	if st.RelayBytes == 0 {
		t.Fatal("relay_bytes = 0, want >0 (relay should count all proxied traffic)")
	}
	if st.CapturedBytes == 0 || st.CapturedBytes > st.RelayBytes {
		t.Fatalf("captured_bytes = %d out of relay_bytes = %d, want 0 < captured <= relay",
			st.CapturedBytes, st.RelayBytes)
	}
	if st.TotalConns != 1 {
		t.Fatalf("total_conns = %d, want 1 (single phone connection reused across captures)", st.TotalConns)
	}
}

// TestControlServerRoundTrip 验证本地控制接口：HTTP start/stop/status 能
// 改变 agent 的抓包状态，且未抓包时确实零上报。
func TestControlServerRoundTrip(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(testWriter{t}, nil))

	_, srcAddr, col := openMobileSource(t)
	env := startIdleRelay(t, logger)

	ctrl := NewControlServer("127.0.0.1:0", env.gate, nil, logger)
	ctrlCtx, ctrlCancel := context.WithCancel(context.Background())
	t.Cleanup(ctrlCancel)
	ctrlErr := make(chan error, 1)
	go func() { ctrlErr <- ctrl.Serve(ctrlCtx) }()

	// 等控制端口就绪（端口为 0 时由内核分配）。
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, _, err := net.SplitHostPort(ctrl.Addr()); err == nil && ctrl.Addr() != "127.0.0.1:0" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("control server not ready")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cli := NewControlClient(ctrl.Addr())

	st, err := cli.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.State != "idle" {
		t.Fatalf("initial state = %s, want idle", st.State)
	}

	if err := cli.StartCapture(context.Background(), StartRequest{
		CaptureID:  "ctrl-session",
		ServerAddr: srcAddr,
	}); err != nil {
		t.Fatalf("start capture via control: %v", err)
	}
	st, err = cli.Status(context.Background())
	if err != nil {
		t.Fatalf("status after start: %v", err)
	}
	if st.State != "capturing" || st.CaptureID != "ctrl-session" {
		t.Fatalf("state after start = %+v, want capturing/ctrl-session", st)
	}

	conn := env.tunnel(t, env.echoAddr)
	roundTrip(t, conn, []byte(`{"via":"control"}`))
	col.waitFor(t, 1, 3*time.Second)

	if err := cli.StopCapture(context.Background()); err != nil {
		t.Fatalf("stop capture via control: %v", err)
	}
	st, err = cli.Status(context.Background())
	if err != nil {
		t.Fatalf("status after stop: %v", err)
	}
	if st.State != "idle" {
		t.Fatalf("state after stop = %s, want idle", st.State)
	}

	// 停止后连接仍然可用（VPN 不重连）。
	roundTrip(t, conn, []byte(`{"via":"control","after":"stop"}`))

	ctrlCancel()
	if err := <-ctrlErr; err != nil {
		t.Fatalf("control server: %v", err)
	}
}

// TestCaptureStartRequiresServerAddr 校验控制接口的入参：缺 server_addr 必须报错，
// 而不是静默进入一个「看起来在抓包其实没处推」的状态。
func TestCaptureStartRequiresServerAddr(t *testing.T) {
	gate := NewCaptureGate(RelayConfig{}, slog.Default())
	if err := gate.Start("x", "", nil, nil); err == nil {
		t.Fatal("Start with empty server_addr should fail")
	}
	if gate.State() != "idle" {
		t.Fatalf("state = %s after failed start, want idle", gate.State())
	}
}

// connIDOf 取出 packet 上报的 conn_id（mobile source 写入 Metadata）。
func connIDOf(t *testing.T, pkt event.Packet) string {
	t.Helper()
	id, ok := pkt.Metadata["conn_id"].(string)
	if !ok {
		t.Fatalf("packet missing conn_id metadata: %v", pkt.Metadata)
	}
	return id
}

// epochSuffix 从 conn_id 中取出 "-eN" 后缀（capture epoch 标识）。
func epochSuffix(t *testing.T, pkt event.Packet) string {
	t.Helper()
	id := connIDOf(t, pkt)
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '-' {
			return id[i:]
		}
	}
	t.Fatalf("conn_id %q has no epoch suffix", id)
	return ""
}
