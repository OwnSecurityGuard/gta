package decode

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
	"gta/pkg/event"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// fakeDecoderV2 返回正常的 MsgPack 编码结果。
type fakeDecoderV2 struct {
	pb.UnimplementedDecoderServer
}

func (f *fakeDecoderV2) DecodeV2(stream grpc.BidiStreamingServer[pb.DecodeRequest, pb.DecodeResponseV2]) error {
	req, err := stream.Recv()
	if err != nil {
		return err
	}
	payload := event.ValueObject(map[string]event.Value{
		"data": event.ValueObject(map[string]event.Value{
			"ok": event.ValueBool(true),
		}),
	})
	msgpackData, err := payload.MarshalMsgpack()
	if err != nil {
		return err
	}
	_ = stream.Send(&pb.DecodeResponseV2{
		InputId:        req.InputId,
		EventType:      "test.event",
		SchemaId:       "test.v1",
		PayloadMsgpack: msgpackData,
	})
	_ = stream.Send(&pb.DecodeResponseV2{
		InputId: req.InputId,
		Done:    true,
	})
	return nil
}

// errorDecoderV2 在结果中返回错误（V2 中错误为 per-result 而非 per-call）。
type errorDecoderV2 struct {
	pb.UnimplementedDecoderServer
}

func (f *errorDecoderV2) DecodeV2(stream grpc.BidiStreamingServer[pb.DecodeRequest, pb.DecodeResponseV2]) error {
	req, err := stream.Recv()
	if err != nil {
		return err
	}
	_ = stream.Send(&pb.DecodeResponseV2{
		InputId: req.InputId,
		Error:   "bad payload",
	})
	_ = stream.Send(&pb.DecodeResponseV2{
		InputId: req.InputId,
		Done:    true,
	})
	return nil
}

func TestDispatcherDecodeV2Error(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "test.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterDecoderServer(srv, &errorDecoderV2{})
	go srv.Serve(lis)
	defer srv.Stop()

	conn, err := grpc.NewClient("unix:"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	d, err := NewDispatcher(pb.NewDecoderClient(conn), "test-session", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	pkt := event.Packet{
		Timestamp: time.Now(),
		Protocol:  "tcp",
		Raw:       []byte("x"),
		Src:       netip.MustParseAddrPort("10.0.0.1:1"),
		Dst:       netip.MustParseAddrPort("10.0.0.2:2"),
		Metadata:  make(map[string]any),
	}
	evs, err := d.DecodeV2(context.Background(), pkt)
	if err != nil {
		t.Fatal(err)
	}
	// V2 中错误结果被跳过，应返回空列表
	if len(evs) != 0 {
		t.Fatalf("expected 0 events from error decoder, got %d", len(evs))
	}
}

func TestDispatcherDecodeV2Success(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "test.sock")
	_ = os.Remove(socket)
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterDecoderServer(srv, &fakeDecoderV2{})
	go srv.Serve(lis)
	defer srv.Stop()

	conn, err := grpc.NewClient("unix:"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	d, err := NewDispatcher(pb.NewDecoderClient(conn), "test-session", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	pkt := event.Packet{
		Timestamp: time.Now(),
		Protocol:  "tcp",
		Raw:       []byte("x"),
		Src:       netip.MustParseAddrPort("10.0.0.1:1"),
		Dst:       netip.MustParseAddrPort("10.0.0.2:2"),
		Metadata:  make(map[string]any),
	}
	evs, err := d.DecodeV2(context.Background(), pkt)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	ev := evs[0]
	if ev.Identity.Type != "test.event" {
		t.Fatalf("unexpected event type: %s", ev.Identity.Type)
	}
	// 验证 payload 内容
	obj, ok := ev.Payload.Value.AsObject()
	if !ok {
		t.Fatal("payload is not object")
	}
	data, ok := obj["data"]
	if !ok {
		t.Fatal("payload missing 'data' key")
	}
	dataObj, ok := data.AsObject()
	if !ok {
		t.Fatal("data is not object")
	}
	if v, ok := dataObj["ok"]; !ok || !v.Bool {
		t.Fatalf("expected data.ok=true, got %v", v)
	}
}

// closeAfterRecvDecoder 接收一个请求后关闭流，模拟断流。
type closeAfterRecvDecoder struct {
	pb.UnimplementedDecoderServer
}

func (f *closeAfterRecvDecoder) DecodeV2(stream grpc.BidiStreamingServer[pb.DecodeRequest, pb.DecodeResponseV2]) error {
	_, err := stream.Recv()
	if err != nil {
		return err
	}
	// 直接关闭流（不发送响应），模拟对端断流。
	return nil
}

func TestDispatcherIsHealthy(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "test.sock")
	_ = os.Remove(socket)
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterDecoderServer(srv, &closeAfterRecvDecoder{})
	go srv.Serve(lis)
	defer srv.Stop()

	conn, err := grpc.NewClient("unix:"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	d, err := NewDispatcher(pb.NewDecoderClient(conn), "test-session", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// 新建时流是健康的。
	if !d.IsHealthy() {
		t.Fatal("expected dispatcher to be healthy after creation")
	}

	// 提交一个包触发对端关闭流。
	pkt := event.Packet{
		Timestamp: time.Now(),
		Protocol:  "tcp",
		Raw:       []byte("x"),
		Src:       netip.MustParseAddrPort("10.0.0.1:1"),
		Dst:       netip.MustParseAddrPort("10.0.0.2:2"),
		Metadata:  make(map[string]any),
	}
	_, _ = d.Submit(pkt)

	// 等待 recvLoop 退出。
	requireEventuallyHealthy(t, d, false, 2*time.Second)
}

// requireEventuallyHealthy 轮询直到 d.IsHealthy() == want 或超时。
func requireEventuallyHealthy(t *testing.T, d *Dispatcher, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if d.IsHealthy() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for dispatcher healthy=%v (got %v)", want, d.IsHealthy())
}

func TestInferDirection(t *testing.T) {
	tests := []struct {
		name       string
		serverPort int
		src        uint16
		dst        uint16
		want       string
	}{
		{"well-known server port", 0, 12345, 80, "client_to_server"},
		{"well-known server response", 0, 80, 12345, "server_to_client"},
		{"game server port client to server", 8989, 12345, 8989, "client_to_server"},
		{"game server port server to client", 8989, 8989, 12345, "server_to_client"},
		{"game server port both equal", 8989, 8989, 8989, "unknown"},
		{"game server port with well-known fallback", 8989, 12345, 80, "client_to_server"},
		{"unknown ports", 0, 12345, 8989, "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inferDirection(tt.src, tt.dst, tt.serverPort)
			if got != tt.want {
				t.Errorf("inferDirection(%d, %d, %d) = %q, want %q", tt.src, tt.dst, tt.serverPort, got, tt.want)
			}
		})
	}
}
