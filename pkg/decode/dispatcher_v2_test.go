package decode

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
	"gametrace/pkg/event"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// mockDecoderV2 实现 V2 协议（DecodeV2）的 mock 服务器。
type mockDecoderV2 struct {
	pb.UnimplementedDecoderServer
}

func (m *mockDecoderV2) DecodeV2(stream grpc.BidiStreamingServer[pb.DecodeRequest, pb.DecodeResponseV2]) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			return err
		}

		payload := event.ValueObject(map[string]event.Value{
			"msg": event.ValueString("hello"),
		})
		msgpackData, err := payload.MarshalMsgpack()
		if err != nil {
			return err
		}

		resp := &pb.DecodeResponseV2{
			InputId:          req.InputId,
			EventType:        "http.request",
			SchemaId:         "http.request.v1",
			PayloadMsgpack:   msgpackData,
			CausationInputId: "causation-123",
			CorrelationKey:   "correlation-456",
		}
		if err := stream.Send(resp); err != nil {
			return err
		}

		done := &pb.DecodeResponseV2{
			InputId: req.InputId,
			Done:    true,
		}
		if err := stream.Send(done); err != nil {
			return err
		}
	}
}

func startMockServer(t *testing.T, server pb.DecoderServer) (string, func()) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	s := grpc.NewServer()
	pb.RegisterDecoderServer(s, server)

	go func() {
		if err := s.Serve(lis); err != nil {
			t.Logf("server stopped: %v", err)
		}
	}()

	return lis.Addr().String(), func() {
		s.Stop()
	}
}

func TestDispatcherV2Protocol(t *testing.T) {
	addr, cleanup := startMockServer(t, &mockDecoderV2{})
	defer cleanup()

	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	client := pb.NewDecoderClient(conn)
	dispatcher, err := NewDispatcher(client, "test-session", nil, nil)
	if err != nil {
		t.Fatalf("failed to create dispatcher: %v", err)
	}
	defer dispatcher.Close()

	pkt := event.Packet{
		Src:       netip.MustParseAddrPort("192.168.1.1:12345"),
		Dst:       netip.MustParseAddrPort("10.0.0.1:80"),
		Protocol:  "http",
		Raw:       []byte("GET / HTTP/1.1\r\n"),
		Timestamp: time.Now(),
		LinkType:  1,
	}

	evs, err := dispatcher.DecodeV2(context.Background(), pkt)
	if err != nil {
		t.Fatalf("DecodeV2 failed: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("DecodeV2 returned 0 results")
	}
	ev := evs[0]

	if ev.Identity.Type != "http.request" {
		t.Errorf("expected event type 'http.request', got '%s'", ev.Identity.Type)
	}
	if ev.Trace.CorrelationID != "correlation-456" {
		t.Errorf("expected correlation_id 'correlation-456', got '%s'", ev.Trace.CorrelationID)
	}
}
