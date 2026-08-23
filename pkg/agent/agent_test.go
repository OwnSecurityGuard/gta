package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"gta/pkg/capture"
	"gta/pkg/capture/mobile"
	"gta/pkg/event"
)

// addrSource 兼容断言：mobileSource 实现 Addr() net.Addr。
type addrSource interface {
	Addr() net.Addr
}

// TestRelayEndToEnd 全链路：
//
//	模拟游戏客户端 ──▶ Relay(TCP 中继) ──▶ 模拟游戏服务端(EchoServer)
//	                          │
//	                          └── gRPC Push ──▶ mobile source ──▶ 流重组 ──▶ event.Packet
//
// 验证 4 条请求消息与 4 条应答消息各成为一个 packet，方向与五元组正确。
func TestRelayEndToEnd(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(testWriter{t}, nil))

	// 1. GTA mobile source（length_prefix 分帧）
	src, err := capture.Open(context.Background(), "mobile", mobile.MobileConfig{
		ListenAddr: "127.0.0.1:0",
		FrameStyle: mobile.FrameLengthPrefix,
		PrefixLen:  4,
	})
	if err != nil {
		t.Fatalf("open mobile source: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })
	srcAddr := src.(addrSource).Addr().String()
	if srcAddr == "" {
		t.Fatalf("mobile source addr is empty")
	}

	// 2. 模拟游戏服务端（作为 relay 的上游）
	echoLis, err := EchoServer("127.0.0.1:0", logger)
	if err != nil {
		t.Fatalf("start echo server: %v", err)
	}
	t.Cleanup(func() { _ = echoLis.Close() })
	echoAddr := echoLis.Addr().String()

	// 3. PushClient → mobile source
	client, err := NewPushClient(srcAddr, logger)
	if err != nil {
		t.Fatalf("new push client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// 4. TCP 中继：监听本地，转发到 echo server
	relay := NewRelay(RelayConfig{
		ListenAddr: "127.0.0.1:0",
		TargetAddr: echoAddr,
		App:        "com.game.demo",
		Device:     "pixel-8",
	}, client, logger)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	relayErr := make(chan error, 1)
	go func() { relayErr <- relay.Serve(ctx) }()

	// 等待 relay 就绪
	var relayAddr string
	deadline := time.Now().Add(3 * time.Second)
	for relayAddr == "" {
		if a := relay.Addr(); a != nil {
			relayAddr = a.String()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay not ready")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 5. 模拟游戏客户端走 4 条消息
	messages := [][]byte{
		[]byte(`{"msg":"login","user":"alice"}`),
		[]byte(`{"msg":"move","x":1,"y":2}`),
		[]byte(`{"msg":"attack","target":3}`),
		[]byte(`{"msg":"logout"}`),
	}
	if err := RunSimClient(relayAddr, messages, logger); err != nil {
		t.Fatalf("run sim client: %v", err)
	}

	// 6. 期望 4 request + 4 response = 8 个完整应用帧
	var reqs, resps int
	deadline = time.Now().Add(5 * time.Second)
	for reqs+resps < 8 {
		select {
		case pkt, ok := <-src.Packets():
			if !ok {
				t.Fatalf("packet channel closed early: reqs=%d resps=%d", reqs, resps)
			}
			if pkt.LinkType != event.LinkTypeProxyPayload || pkt.Protocol != "tcp" {
				t.Errorf("packet link_type/protocol = %d/%s", pkt.LinkType, pkt.Protocol)
			}
			switch pkt.Metadata["direction"] {
			case "request":
				if pkt.Dst.String() != echoAddr {
					t.Errorf("request dst = %s, want %s", pkt.Dst, echoAddr)
				}
				reqs++
			case "response":
				if pkt.Src.String() != echoAddr {
					t.Errorf("response src = %s, want %s", pkt.Src, echoAddr)
				}
				resps++
			default:
				t.Errorf("unexpected direction metadata: %v", pkt.Metadata["direction"])
			}
		case <-time.After(time.Until(deadline)):
			t.Fatalf("timeout: reqs=%d resps=%d", reqs, resps)
		}
	}
	if reqs != 4 || resps != 4 {
		t.Fatalf("reqs=%d resps=%d, want 4/4", reqs, resps)
	}
	// 停止 relay：cancel 会让 Serve 关闭监听并返回，确认其干净退出。
	cancel()
	if err := <-relayErr; err != nil {
		t.Fatalf("relay serve: %v", err)
	}
}

// testWriter 让 slog 输出接入 testing.T，失败时可见日志。
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

// TestRelayHTTPConnectProxy 纯 HTTP CONNECT 代理模式（TargetAddr 为空，agent 默认常驻模式）：
//
//	模拟游戏客户端 ── CONNECT ──▶ Relay ── 动态解析目标 ──▶ EchoServer
//	                          │
//	                          └── gRPC Push ──▶ mobile source
//
// 验证：CONNECT 返回 200；隧道内数据双向转发（帧回声）；抓包按 request/response 分帧且目标正确。
func TestRelayHTTPConnectProxy(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(testWriter{t}, nil))

	// 1. GTA mobile source（length_prefix 分帧）
	src, err := capture.Open(context.Background(), "mobile", mobile.MobileConfig{
		ListenAddr: "127.0.0.1:0",
		FrameStyle: mobile.FrameLengthPrefix,
		PrefixLen:  4,
	})
	if err != nil {
		t.Fatalf("open mobile source: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })
	srcAddr := src.(addrSource).Addr().String()
	if srcAddr == "" {
		t.Fatalf("mobile source addr is empty")
	}

	// 2. 模拟游戏服务端（作为 CONNECT 动态解析出的上游目标）
	echoLis, err := EchoServer("127.0.0.1:0", logger)
	if err != nil {
		t.Fatalf("start echo server: %v", err)
	}
	t.Cleanup(func() { _ = echoLis.Close() })
	echoAddr := echoLis.Addr().String()

	// 3. PushClient → mobile source
	client, err := NewPushClient(srcAddr, logger)
	if err != nil {
		t.Fatalf("new push client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// 4. 纯代理模式：TargetAddr 为空 → 等待 CONNECT 动态解析目标
	relay := NewRelay(RelayConfig{ListenAddr: "127.0.0.1:0"}, client, logger)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	relayErr := make(chan error, 1)
	go func() { relayErr <- relay.Serve(ctx) }()

	var relayAddr string
	deadline := time.Now().Add(3 * time.Second)
	for relayAddr == "" {
		if a := relay.Addr(); a != nil {
			relayAddr = a.String()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay not ready")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 5. 客户端连接 relay，发 CONNECT 请求，期待 200
	conn, err := net.Dial("tcp", relayAddr)
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)
	if _, err := io.WriteString(conn, connectReq); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	resp := make([]byte, 128)
	n, err := conn.Read(resp)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if !bytes.HasPrefix(resp[:n], []byte("HTTP/1.1 200")) {
		t.Fatalf("CONNECT response = %q, want HTTP/1.1 200", resp[:n])
	}

	// 6. 隧道内发一帧，期待服务端回声（双向转发；EchoServer 回 `{"ok":true,...}` JSON）
	payload := []byte(`{"msg":"login","user":"bob"}`)
	if err := WriteFrame(conn, payload); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	echo, err := ReadFrame(conn)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Contains(echo, []byte(`"ok":true`)) {
		t.Fatalf("echo = %q, want EchoServer JSON response", echo)
	}

	// 7. 期望 1 request + 1 response，方向与目标正确
	var reqs, resps int
	deadline = time.Now().Add(5 * time.Second)
	for reqs+resps < 2 {
		select {
		case pkt, ok := <-src.Packets():
			if !ok {
				t.Fatalf("packet channel closed early: reqs=%d resps=%d", reqs, resps)
			}
			if pkt.LinkType != event.LinkTypeProxyPayload || pkt.Protocol != "tcp" {
				t.Errorf("packet link_type/protocol = %d/%s", pkt.LinkType, pkt.Protocol)
			}
			switch pkt.Metadata["direction"] {
			case "request":
				if pkt.Dst.String() != echoAddr {
					t.Errorf("request dst = %s, want %s", pkt.Dst, echoAddr)
				}
				reqs++
			case "response":
				if pkt.Src.String() != echoAddr {
					t.Errorf("response src = %s, want %s", pkt.Src, echoAddr)
				}
				resps++
			default:
				t.Errorf("unexpected direction metadata: %v", pkt.Metadata["direction"])
			}
		case <-time.After(time.Until(deadline)):
			t.Fatalf("timeout: reqs=%d resps=%d", reqs, resps)
		}
	}
	if reqs != 1 || resps != 1 {
		t.Fatalf("reqs=%d resps=%d, want 1/1", reqs, resps)
	}

	// 8. 停止 relay，确认干净退出
	cancel()
	if err := <-relayErr; err != nil {
		t.Fatalf("relay serve: %v", err)
	}
}

// TestConnectionFilter 验证连接筛选判定：
//   - 无筛选时全部允许；
//   - 按主机 / 按端口 / 主机+端口（交集）判定正确；
//   - 非法 target 不允许。
func TestConnectionFilter(t *testing.T) {
	cases := []struct {
		name   string
		filter connectionFilter
		target string
		want   bool
	}{
		{name: "no filter allows all", filter: connectionFilter{}, target: "api.x.com:443", want: true},
		{name: "host match", filter: connectionFilter{hosts: map[string]struct{}{"api.x.com": {}}}, target: "api.x.com:443", want: true},
		{name: "host match case-insensitive", filter: connectionFilter{hosts: map[string]struct{}{"api.x.com": {}}}, target: "API.X.COM:443", want: true},
		{name: "host mismatch", filter: connectionFilter{hosts: map[string]struct{}{"api.x.com": {}}}, target: "other.com:443", want: false},
		{name: "port match", filter: connectionFilter{ports: map[int]struct{}{443: {}}}, target: "1.2.3.4:443", want: true},
		{name: "port mismatch", filter: connectionFilter{ports: map[int]struct{}{443: {}}}, target: "1.2.3.4:80", want: false},
		{name: "host+port both hit (AND)", filter: connectionFilter{hosts: map[string]struct{}{"g.x.com": {}}, ports: map[int]struct{}{443: {}}}, target: "g.x.com:443", want: true},
		{name: "host+port one miss", filter: connectionFilter{hosts: map[string]struct{}{"g.x.com": {}}, ports: map[int]struct{}{443: {}}}, target: "g.x.com:80", want: false},
		{name: "invalid target denied", filter: connectionFilter{hosts: map[string]struct{}{"g.x.com": {}}}, target: "not-a-host:port", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.filter.allow(tc.target); got != tc.want {
				t.Fatalf("allow(%q) = %v, want %v", tc.target, got, tc.want)
			}
		})
	}
}

// TestRelayHTTPConnectFilter 验证连接筛选：设置只抓取 echo 目标地址后，
// 命中筛选的连接上报，不匹配的 CONNECT 连接照常中继但不上报给 GTA。
func TestRelayHTTPConnectFilter(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(testWriter{t}, nil))

	// 1. GTA mobile source（length_prefix 分帧）
	src, err := capture.Open(context.Background(), "mobile", mobile.MobileConfig{
		ListenAddr: "127.0.0.1:0",
		FrameStyle: mobile.FrameLengthPrefix,
		PrefixLen:  4,
	})
	if err != nil {
		t.Fatalf("open mobile source: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })
	srcAddr := src.(addrSource).Addr().String()

	// 2. 两个上游：echo（应被抓）+ discard（应被筛掉，只中继不上报）
	echoLis, err := EchoServer("127.0.0.1:0", logger)
	if err != nil {
		t.Fatalf("start echo server: %v", err)
	}
	t.Cleanup(func() { _ = echoLis.Close() })
	echoAddr := echoLis.Addr().String()

	discardLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start discard server: %v", err)
	}
	t.Cleanup(func() { _ = discardLis.Close() })
	go func() {
		for {
			c, err := discardLis.Accept()
			if err != nil {
				return
			}
			go func() { _ = c.Close() }()
		}
	}()
	discardAddr := discardLis.Addr().String()

	// 3. PushClient → mobile source
	client, err := NewPushClient(srcAddr, logger)
	if err != nil {
		t.Fatalf("new push client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// 4. 筛选：仅抓取 echo 目标端口；不匹配的连接照常中继但不上报
	_, echoPortStr, _ := net.SplitHostPort(echoAddr)
	echoPort, _ := strconv.Atoi(echoPortStr)
	relay := NewRelay(RelayConfig{
		ListenAddr:  "127.0.0.1:0",
		FilterPorts: []int{echoPort},
	}, client, logger)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	relayErr := make(chan error, 1)
	go func() { relayErr <- relay.Serve(ctx) }()

	var relayAddr string
	deadline := time.Now().Add(3 * time.Second)
	for relayAddr == "" {
		if a := relay.Addr(); a != nil {
			relayAddr = a.String()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay not ready")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 辅助：经 relay 建一条 CONNECT 隧道并发一帧
	tunnel := func(target string) (net.Conn, error) {
		conn, err := net.Dial("tcp", relayAddr)
		if err != nil {
			return nil, err
		}
		if _, err := io.WriteString(conn, fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)); err != nil {
			_ = conn.Close()
			return nil, err
		}
		resp := make([]byte, 128)
		if _, err := conn.Read(resp); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}

	// 5. 不匹配连接：应拿到 200 且中继成功，但 GTA 侧无任何上报
	miss, err := tunnel(discardAddr)
	if err != nil {
		t.Fatalf("discard tunnel: %v", err)
	}
	if err := WriteFrame(miss, []byte(`{"msg":"ignored"}`)); err != nil {
		t.Fatalf("write to discard tunnel: %v", err)
	}
	_ = miss.Close()

	// 6. 匹配连接：应上报 1 request + 1 response
	hit, err := tunnel(echoAddr)
	if err != nil {
		t.Fatalf("echo tunnel: %v", err)
	}
	if err := WriteFrame(hit, []byte(`{"msg":"login"}`)); err != nil {
		t.Fatalf("write to echo tunnel: %v", err)
	}
	if _, err := ReadFrame(hit); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	_ = hit.Close()

	var reqs, resps int
	deadline = time.Now().Add(5 * time.Second)
	for reqs+resps < 2 {
		select {
		case pkt, ok := <-src.Packets():
			if !ok {
				t.Fatalf("packet channel closed early: reqs=%d resps=%d", reqs, resps)
			}
			if pkt.Dst.String() != echoAddr && pkt.Src.String() != echoAddr {
				t.Fatalf("unexpected packet endpoint (filter should exclude discard): %s->%s", pkt.Src, pkt.Dst)
			}
			switch pkt.Metadata["direction"] {
			case "request":
				reqs++
			case "response":
				resps++
			}
		case <-time.After(time.Until(deadline)):
			t.Fatalf("timeout: reqs=%d resps=%d", reqs, resps)
		}
	}
	if reqs != 1 || resps != 1 {
		t.Fatalf("reqs=%d resps=%d, want 1/1 (discard connection must not be captured)", reqs, resps)
	}

	cancel()
	if err := <-relayErr; err != nil {
		t.Fatalf("relay serve: %v", err)
	}
}

