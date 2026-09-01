package decode

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
	"gta/pkg/event"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// gatedDecoderV2 收满 gateN 个请求前不返回任何响应；攒够后把积压请求全部响应。
// 用于确定性地验证 Dispatcher 的 multi-in-flight 流水线能力
// （若 Submit 是逐包阻塞等待，则永远凑不满 gateN）。
type gatedDecoderV2 struct {
	pb.UnimplementedDecoderServer
	gateN int
	mu    sync.Mutex
	open  bool
	reqs  []*pb.DecodeRequest
}

func (m *gatedDecoderV2) DecodeV2(stream grpc.BidiStreamingServer[pb.DecodeRequest, pb.DecodeResponseV2]) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			return err
		}
		m.mu.Lock()
		m.reqs = append(m.reqs, req)
		m.open = m.open || len(m.reqs) >= m.gateN
		ready := m.open
		var backlog []*pb.DecodeRequest
		if ready {
			backlog = m.reqs
			m.reqs = nil
		}
		m.mu.Unlock()
		if !ready {
			continue // 攒够 gateN 个在途请求前不响应
		}
		for _, r := range backlog {
			if err := m.respond(stream, r); err != nil {
				return err
			}
		}
	}
}

func (m *gatedDecoderV2) respond(stream grpc.BidiStreamingServer[pb.DecodeRequest, pb.DecodeResponseV2], req *pb.DecodeRequest) error {
	payload, err := event.ValueObject(map[string]event.Value{
		"msg": event.ValueString("hello"),
	}).MarshalMsgpack()
	if err != nil {
		return err
	}
	resp := &pb.DecodeResponseV2{
		InputId:        req.InputId,
		EventType:      "test.event",
		SchemaId:       "unknown.v1",
		PayloadMsgpack: payload,
	}
	if err := stream.Send(resp); err != nil {
		return err
	}
	return stream.Send(&pb.DecodeResponseV2{InputId: req.InputId, Done: true})
}

func TestDispatcherPipeline_MultiInFlight(t *testing.T) {
	const n = 5
	addr, cleanup := startMockServer(t, &gatedDecoderV2{gateN: n})
	defer cleanup()

	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	dispatcher, err := NewDispatcher(pb.NewDecoderClient(conn), "test-session", nil, nil)
	if err != nil {
		t.Fatalf("failed to create dispatcher: %v", err)
	}
	defer dispatcher.Close()

	// 连续提交 n 个请求，不等待任何一个返回——若实现退化为同步逐包，
	// gatedDecoderV2 永远凑不满 n 个在途请求，测试将超时失败。
	futures := make([]*Future, 0, n)
	for i := 0; i < n; i++ {
		pkt := event.Packet{
			ID:        string(rune('a' + i)),
			Src:       netip.MustParseAddrPort("192.168.1.1:12345"),
			Dst:       netip.MustParseAddrPort("10.0.0.1:80"),
			Protocol:  "tcp",
			Raw:       []byte("payload"),
			Timestamp: time.Now(),
			LinkType:  1,
		}
		f, err := dispatcher.Submit(pkt)
		if err != nil {
			t.Fatalf("Submit #%d failed: %v", i, err)
		}
		futures = append(futures, f)
	}

	// 按提交顺序 Wait：事件全局保序。
	for i, f := range futures {
		evs, err := f.Wait(context.Background())
		if err != nil {
			t.Fatalf("Wait #%d failed: %v", i, err)
		}
		if len(evs) == 0 {
			t.Fatalf("Wait #%d returned 0 events", i)
		}
		// 每个 raw packet ID（pkt.ID = 'a'+i）应回填到对应事件的 raw_packet_id。
		if got := string(evs[0].Context.RawPacketID); got != string(rune('a'+i)) {
			t.Errorf("event #%d raw_packet_id = %q, want %q", i, got, string(rune('a'+i)))
		}
	}
}

// explodingDecoderV2 收到 failAfter+1 个请求后直接让流失败，用于验证 pending Future 的错误传播。
type explodingDecoderV2 struct {
	pb.UnimplementedDecoderServer
	failAfter int
	reqs      int
}

func (m *explodingDecoderV2) DecodeV2(stream grpc.BidiStreamingServer[pb.DecodeRequest, pb.DecodeResponseV2]) error {
	for {
		_, err := stream.Recv()
		if err != nil {
			return err
		}
		m.reqs++
		if m.reqs > m.failAfter {
			return errors.New("plugin exploded")
		}
	}
}

func TestDispatcherPipeline_StreamErrorFailsPending(t *testing.T) {
	addr, cleanup := startMockServer(t, &explodingDecoderV2{failAfter: 0})
	defer cleanup()

	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	dispatcher, err := NewDispatcher(pb.NewDecoderClient(conn), "test-session", nil, nil)
	if err != nil {
		t.Fatalf("failed to create dispatcher: %v", err)
	}
	defer dispatcher.Close()

	pkt := event.Packet{
		Src:       netip.MustParseAddrPort("192.168.1.1:12345"),
		Dst:       netip.MustParseAddrPort("10.0.0.1:80"),
		Protocol:  "tcp",
		Raw:       []byte("x"),
		Timestamp: time.Now(),
	}
	f, err := dispatcher.Submit(pkt)
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}
	if _, err := f.Wait(context.Background()); err == nil {
		t.Fatal("expected stream error to fail pending future")
	}
}

func TestDispatcherPipeline_CloseFailsPending(t *testing.T) {
	addr, cleanup := startMockServer(t, &gatedDecoderV2{gateN: 1 << 30}) // 永不响应
	defer cleanup()

	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	dispatcher, err := NewDispatcher(pb.NewDecoderClient(conn), "test-session", nil, nil)
	if err != nil {
		t.Fatalf("failed to create dispatcher: %v", err)
	}

	pkt := event.Packet{
		Src:       netip.MustParseAddrPort("192.168.1.1:12345"),
		Dst:       netip.MustParseAddrPort("10.0.0.1:80"),
		Protocol:  "tcp",
		Raw:       []byte("x"),
		Timestamp: time.Now(),
	}
	f, err := dispatcher.Submit(pkt)
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}
	if err := dispatcher.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if _, err := f.Wait(context.Background()); !errors.Is(err, ErrDispatcherClosed) {
		t.Fatalf("expected ErrDispatcherClosed, got %v", err)
	}
	// Close 后再 Submit 应立即失败。
	if _, err := dispatcher.Submit(pkt); !errors.Is(err, ErrDispatcherClosed) {
		t.Fatalf("expected ErrDispatcherClosed on submit after close, got %v", err)
	}
}
