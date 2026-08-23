package mobile

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"gta/pkg/capture"
	"gta/pkg/capture/mobile/proto"
	"gta/pkg/event"
)

// startSource 启动一个 mobile source，返回 Source 与 gRPC 监听地址。
func startSource(t *testing.T, style FrameStyle, prefixLen int) (capture.Source, string) {
	t.Helper()
	cfg := MobileConfig{ListenAddr: "127.0.0.1:0", FrameStyle: style, PrefixLen: prefixLen}
	src, err := capture.Open(context.Background(), "mobile", cfg)
	if err != nil {
		t.Fatalf("open mobile source: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })
	addr := src.(*mobileSource).Addr().String()
	if addr == "" {
		t.Fatalf("source addr is empty")
	}
	return src, addr
}

// openPush 建立到 source 的 Push 客户端流。
func openPush(t *testing.T, addr string) proto.MobileCapture_PushClient {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	stream, err := proto.NewMobileCaptureClient(conn).Push(context.Background())
	if err != nil {
		t.Fatalf("open push stream: %v", err)
	}
	return stream
}

func dataEvent(connID, direction string, payload []byte) *proto.AgentEvent {
	return &proto.AgentEvent{
		ConnId: connID,
		Event:  &proto.AgentEvent_Data{Data: &proto.ConnData{Direction: direction, Payload: payload}},
	}
}

// readN 从包通道读取 n 个包，超时则测试失败。
func readN(t *testing.T, ch <-chan event.Packet, n int) []event.Packet {
	t.Helper()
	out := make([]event.Packet, 0, n)
	deadline := time.After(5 * time.Second)
	for len(out) < n {
		select {
		case pkt, ok := <-ch:
			if !ok {
				t.Fatalf("packet channel closed early: got %d want %d", len(out), n)
			}
			out = append(out, pkt)
		case <-deadline:
			t.Fatalf("timeout waiting for %d packets, got %d", n, len(out))
		}
	}
	return out
}

// TestMobileSourceEndToEnd 覆盖：open → data（完整帧/拆帧/多方向）→ close，
// 验证 packet 的 LinkType/Src/Dst/Protocol/Metadata 与流重组结果。
func TestMobileSourceEndToEnd(t *testing.T) {
	src, addr := startSource(t, FrameLengthPrefix, 4)
	stream := openPush(t, addr)

	mustSend := func(evt *proto.AgentEvent) {
		t.Helper()
		if err := stream.Send(evt); err != nil {
			t.Fatalf("stream.Send: %v", err)
		}
	}

	mustSend(&proto.AgentEvent{
		ConnId: "c1",
		Event: &proto.AgentEvent_Open{Open: &proto.ConnOpen{
			ClientAddr: "10.0.0.5:50000",
			ServerAddr: "1.2.3.4:443",
			Network:    "tcp",
			App:        "com.game.demo",
			Device:     "pixel-8",
		}},
	})
	// 完整帧
	mustSend(dataEvent("c1", "request", be4Frame([]byte("login"))))
	// 拆帧：一条消息分两次推送
	m := be4Frame([]byte("move x=1 y=2"))
	mustSend(dataEvent("c1", "request", m[:7]))
	mustSend(dataEvent("c1", "request", m[7:]))
	// 服务端响应
	mustSend(dataEvent("c1", "response", be4Frame([]byte("login_ok"))))
	// 关闭连接（应 flush 残余；本例无残余）
	mustSend(&proto.AgentEvent{ConnId: "c1", Event: &proto.AgentEvent_Close{Close: &proto.ConnClose{Reason: "fin"}}})

	if _, err := stream.CloseAndRecv(); err != nil {
		t.Fatalf("close and recv: %v", err)
	}

	// 期望 3 个完整应用帧：login(req)、move(req)、login_ok(resp)
	pkts := readN(t, src.Packets(), 3)

	req := []event.Packet{pkts[0], pkts[1]}
	resp := pkts[2]
	for i, p := range req {
		if p.Protocol != "tcp" {
			t.Errorf("req[%d] protocol = %q, want tcp", i, p.Protocol)
		}
		if p.LinkType != event.LinkTypeProxyPayload {
			t.Errorf("req[%d] link_type = %d, want ProxyPayload", i, p.LinkType)
		}
		if p.Src.String() != "10.0.0.5:50000" || p.Dst.String() != "1.2.3.4:443" {
			t.Errorf("req[%d] src/dst = %s/%s, want 10.0.0.5:50000/1.2.3.4:443", i, p.Src, p.Dst)
		}
		if p.Metadata["direction"] != "request" {
			t.Errorf("req[%d] direction metadata = %v, want request", i, p.Metadata["direction"])
		}
		if p.Metadata[capture.MetaAppPackage] != "com.game.demo" {
			t.Errorf("req[%d] app = %v", i, p.Metadata[capture.MetaAppPackage])
		}
	}
	if string(req[0].Raw) != "login" || string(req[1].Raw) != "move x=1 y=2" {
		t.Errorf("request payloads mismatch: %q / %q", req[0].Raw, req[1].Raw)
	}

	if resp.Src.String() != "1.2.3.4:443" || resp.Dst.String() != "10.0.0.5:50000" {
		t.Errorf("resp src/dst = %s/%s, want reversed", resp.Src, resp.Dst)
	}
	if resp.Metadata["direction"] != "response" {
		t.Errorf("resp direction = %v, want response", resp.Metadata["direction"])
	}
	if string(resp.Raw) != "login_ok" {
		t.Errorf("resp payload = %q, want login_ok", resp.Raw)
	}
}

// TestMobileSourceRawStyle 验证 raw 模式下每个数据块直接成为一个 packet。
func TestMobileSourceRawStyle(t *testing.T) {
	src, addr := startSource(t, FrameRaw, 0)
	stream := openPush(t, addr)

	if err := stream.Send(&proto.AgentEvent{
		ConnId: "r1",
		Event: &proto.AgentEvent_Open{Open: &proto.ConnOpen{
			ClientAddr: "10.0.0.5:1111",
			ServerAddr: "10.0.0.1:2222",
			Network:    "tcp",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(dataEvent("r1", "request", []byte("chunk-1"))); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(dataEvent("r1", "request", []byte("chunk-2"))); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		t.Fatal(err)
	}

	pkts := readN(t, src.Packets(), 2)
	if string(pkts[0].Raw) != "chunk-1" || string(pkts[1].Raw) != "chunk-2" {
		t.Fatalf("raw payloads mismatch: %q / %q", pkts[0].Raw, pkts[1].Raw)
	}
}

// TestMobileSourceDataBeforeOpen 验证 open 未到时的数据被丢弃（agent 违约）并计入错误。
func TestMobileSourceDataBeforeOpen(t *testing.T) {
	src, addr := startSource(t, FrameRaw, 0)
	stream := openPush(t, addr)

	if err := stream.Send(dataEvent("ghost", "request", []byte("orphan"))); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		t.Fatal(err)
	}
	if got := src.Stats().Errors; got != 1 {
		t.Fatalf("want 1 error, got %d", got)
	}
}
