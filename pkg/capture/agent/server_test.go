package agent

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"gta/pkg/auth"
	"gta/pkg/capture/agent/proto"
	"gta/pkg/event"
)

// newBufconnServer 起一个带鉴权拦截器的 AgentIngest server，返回客户端拨号函数。
func newBufconnServer(t *testing.T, resolver auth.Resolver, sessions SessionOwnerChecker) (func(context.Context, string) (net.Conn, error), *Hub, *Source) {
	t.Helper()

	hub := NewHub()
	src, err := NewSource(Config{Hub: hub, SessionID: "sess-1", ChannelSize: 64})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := src.Start(ctx); err != nil {
		t.Fatalf("start source: %v", err)
	}
	t.Cleanup(func() { cancel(); _ = src.Close() })

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(grpc.ChainStreamInterceptor(auth.StreamInterceptor(resolver)))
	proto.RegisterAgentIngestServer(srv, NewIngestServer(hub, sessions))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	dial := func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}
	return dial, hub, src
}

func newAgentClient(t *testing.T, dial func(context.Context, string) (net.Conn, error)) proto.AgentIngest_PushClient {
	t.Helper()
	cc, err := grpc.NewClient("passthrough:///agent",
		grpc.WithContextDialer(dial),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	stream, err := proto.NewAgentIngestClient(cc).Push(context.Background())
	if err != nil {
		t.Fatalf("open push stream: %v", err)
	}
	return stream
}

func testBatch(sessionID string, ids ...string) *proto.PacketBatch {
	batch := &proto.PacketBatch{SessionId: sessionID, Iface: "eth0"}
	for _, id := range ids {
		batch.Packets = append(batch.Packets, &proto.RawPacket{
			Id:          id,
			TimestampNs: 1700000000000000000,
			LinkType:    1, // ethernet
			Raw:         []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x00, 0x11},
			Src:         "10.0.0.1:1234",
			Dst:         "10.0.0.2:5678",
			Protocol:    "tcp",
			Metadata:    map[string]string{"k": "v"},
		})
	}
	return batch
}

// 场景 1：完整链路 bufconn 往返——agent 推送的完整帧 + link_type 原样到达 source。
func TestPushRoundTripPreservesFullFrameAndLinkType(t *testing.T) {
	dial, hub, src := newBufconnServer(t, auth.NewStaticResolver(nil), nil)

	stream := newAgentClient(t, dial)
	if err := stream.Send(testBatch("sess-1", "id-1", "id-2")); err != nil {
		t.Fatalf("send: %v", err)
	}

	// source（capture pipeline 视角）应收到完整帧 + 真实 link_type。
	deadline := time.After(5 * time.Second)
	got := 0
	for got < 2 {
		select {
		case pkt := <-src.Packets():
			got++
			if len(pkt.Raw) == 0 {
				t.Fatal("packet raw frame is empty")
			}
			if pkt.LinkType != event.LinkTypeEthernet {
				t.Fatalf("link_type = %d, want %d (必须保留真实链路层类型，不能退化为 ProxyPayload)", pkt.LinkType, event.LinkTypeEthernet)
			}
			if pkt.Src.String() != "10.0.0.1:1234" || pkt.Dst.String() != "10.0.0.2:5678" {
				t.Fatalf("src/dst = %s/%s", pkt.Src, pkt.Dst)
			}
			if pkt.Protocol != "tcp" {
				t.Fatalf("protocol = %q", pkt.Protocol)
			}
			if pkt.Timestamp.IsZero() {
				t.Fatal("timestamp should be parsed from timestamp_ns")
			}
			if pkt.Metadata["interface"] != "eth0" {
				t.Fatalf("metadata interface = %v", pkt.Metadata["interface"])
			}
		case <-deadline:
			t.Fatalf("timeout: got %d packets", got)
		}
	}
	if got := hub.Subscribers("sess-1"); got != 1 {
		t.Fatalf("subscribers = %d, want 1", got)
	}

	// 关流拿 PushAck。
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}
	ack, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatalf("recv ack: %v", err)
	}
	if ack.GetBatches() != 1 || ack.GetPackets() != 2 || ack.GetDelivered() != 2 {
		t.Fatalf("ack = %+v", ack)
	}
	if ack.GetDropped() != 0 || ack.GetRejected() != 0 {
		t.Fatalf("ack = %+v", ack)
	}
}

// 场景 2：owner 不匹配 → PermissionDenied，整条流结束。
func TestPushRejectsOwnerMismatch(t *testing.T) {
	resolver := auth.NewStaticResolver(map[string]auth.Principal{
		"gta_alice": {Owner: "alice"},
		"gta_bob":   {Owner: "bob"},
	})
	sessions := SessionOwnerCheckerFunc(func(sessionID string) (string, bool) {
		if sessionID == "sess-1" {
			return "alice", true
		}
		return "", false
	})
	dial, hub, src := newBufconnServer(t, resolver, sessions)

	// bob 不能推 alice 的会话。
	stream := newAgentClient(t, dial)
	_ = stream.Send(testBatch("sess-1", "x"))
	if _, err := stream.CloseAndRecv(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", status.Code(err))
	}
	// 包不应被投递。
	select {
	case pkt := <-src.Packets():
		t.Fatalf("unexpected packet delivered: %+v", pkt)
	default:
	}
	if _, busy, _ := hub.Stats(); busy != 0 {
		t.Fatal("mismatched batch should not reach the hub")
	}

	// alice 推自己的会话 OK。
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer gta_alice")
	cc, err := grpc.NewClient("passthrough:///agent",
		grpc.WithContextDialer(dial),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	stream3, err := proto.NewAgentIngestClient(cc).Push(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream3.Send(testBatch("sess-1", "ok-1")); err != nil {
		t.Fatal(err)
	}
	select {
	case pkt := <-src.Packets():
		if pkt.ID != "ok-1" {
			t.Fatalf("packet id = %q", pkt.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for alice's packet")
	}

	// 不存在的会话也应拒绝。
	stream4, err := proto.NewAgentIngestClient(cc).Push(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream4.Send(testBatch("sess-404", "y"))
	if _, err := stream4.CloseAndRecv(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied for unknown session", status.Code(err))
	}
}

// 场景 3：匿名模式（未配置 token）→ owner 统一为 local，可推 owner=''/'local' 的会话。
func TestPushAnonymousMode(t *testing.T) {
	sessions := SessionOwnerCheckerFunc(func(sessionID string) (string, bool) {
		return "", true // 老库会话 owner 为空
	})
	dial, _, src := newBufconnServer(t, auth.NewStaticResolver(nil), sessions)

	stream := newAgentClient(t, dial)
	if err := stream.Send(testBatch("sess-1", "anon-1")); err != nil {
		t.Fatal(err)
	}
	select {
	case pkt := <-src.Packets():
		if pkt.ID != "anon-1" {
			t.Fatalf("packet id = %q", pkt.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for anonymous packet")
	}
}

// 场景 4：慢消费者——server 流不被阻塞，包被丢弃并计入 dropped。
func TestPushSlowConsumerDropsWithoutBlocking(t *testing.T) {
	// ChannelSize=1 的 source，无消费者。
	hub := NewHub()
	src, err := NewSource(Config{Hub: hub, SessionID: "sess-1", ChannelSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := src.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(grpc.ChainStreamInterceptor(auth.StreamInterceptor(auth.NewStaticResolver(nil))))
	proto.RegisterAgentIngestServer(srv, NewIngestServer(hub, nil))
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	cc, err := grpc.NewClient("passthrough:///agent",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	stream, err := proto.NewAgentIngestClient(cc).Push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 发一批 32 个包，source 缓冲只有 1 且无人读：流必须立刻可继续。
	for i := 0; i < 5; i++ {
		if err := stream.Send(testBatch("sess-1", "a")); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	ack, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatalf("recv ack: %v", err)
	}
	if ack.GetPackets() != 5 {
		t.Fatalf("packets = %d, want 5", ack.GetPackets())
	}
	if ack.GetDelivered() != 1 || ack.GetDropped() != 4 {
		t.Fatalf("delivered/dropped = %d/%d, want 1/4", ack.GetDelivered(), ack.GetDropped())
	}
}
