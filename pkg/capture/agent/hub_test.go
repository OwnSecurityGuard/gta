package agent

import (
	"testing"

	"gta/pkg/event"
)

func testPacket(id string) event.Packet {
	return event.Packet{
		ID:       id,
		Raw:      []byte{0xff, 0x02, 0x00, 0x01},
		LinkType: event.LinkTypeEthernet,
		Protocol: "tcp",
	}
}

func TestHubDeliverToSubscriber(t *testing.T) {
	h := NewHub()
	ch := make(chan event.Packet, 4)
	cancel := h.Subscribe("s1", ch)
	defer cancel()

	got := 0
	for i := 0; i < 3; i++ {
		h.Deliver("s1", []event.Packet{testPacket("p1")})
	}
	// 非阻塞投递：channel 容量 4，3 个包应全部可读。
	for got < 3 {
		select {
		case pkt := <-ch:
			if pkt.ID != "p1" {
				t.Fatalf("packet id = %q, want p1", pkt.ID)
			}
			got++
		default:
			t.Fatalf("only got %d packets, want 3", got)
		}
	}
	delivered, droppedBusy, droppedNoSub := h.Stats()
	if delivered != 3 || droppedBusy != 0 || droppedNoSub != 0 {
		t.Fatalf("stats = (%d,%d,%d), want (3,0,0)", delivered, droppedBusy, droppedNoSub)
	}
}

func TestHubDropWithoutBlocking(t *testing.T) {
	h := NewHub()
	// 容量 1，且无人消费：模拟慢消费者。
	ch := make(chan event.Packet, 1)
	cancel := h.Subscribe("s1", ch)
	defer cancel()

	const total = 100
	delivered, dropped := h.Deliver("s1", makeBatch(total))
	if delivered+dropped != total {
		t.Fatalf("delivered+dropped = %d, want %d", delivered+dropped, total)
	}
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1 (容量 1)", delivered)
	}
	if dropped != total-1 {
		t.Fatalf("dropped = %d, want %d", dropped, total-1)
	}
	_, droppedBusy, _ := h.Stats()
	if droppedBusy != total-1 {
		t.Fatalf("droppedBusy = %d, want %d", droppedBusy, total-1)
	}
	// channel 里应恰好还有 1 个包可读。
	select {
	case <-ch:
	default:
		t.Fatal("expected 1 buffered packet")
	}
	select {
	case <-ch:
		t.Fatal("expected channel to be drained")
	default:
	}
}

func TestHubDropNoSubscriber(t *testing.T) {
	h := NewHub()
	delivered, dropped := h.Deliver("no-such-session", makeBatch(5))
	if delivered != 0 || dropped != 5 {
		t.Fatalf("delivered=%d dropped=%d, want 0/5", delivered, dropped)
	}
	_, _, droppedNoSub := h.Stats()
	if droppedNoSub != 5 {
		t.Fatalf("droppedNoSub = %d, want 5", droppedNoSub)
	}
}

func TestHubUnsubscribeThenCloseSafe(t *testing.T) {
	h := NewHub()
	ch := make(chan event.Packet, 4)
	cancel := h.Subscribe("s1", ch)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Deliver("s1", makeBatch(50)) // 部分进 channel，部分被丢
	}()
	// 竞争取消订阅：Deliver 与 cancel 并发，验证 cancel 返回后 close 不 panic。
	cancel()
	<-done
	close(ch) // 若 Hub 在 cancel 后仍写 channel，这里会 panic（由 go test 捕获）
}

func makeBatch(n int) []event.Packet {
	pkts := make([]event.Packet, n)
	for i := range pkts {
		pkts[i] = testPacket("p")
	}
	return pkts
}
