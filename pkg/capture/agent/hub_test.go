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
	sub, cancel := h.Subscribe("s1", ch)
	defer cancel()

	for i := 0; i < 3; i++ {
		h.Deliver("s1", []event.Packet{testPacket("p1")})
	}
	// 非阻塞投递：channel 容量 4，3 个包应全部可读。
	got := 0
	for got < 3 {
		select {
		case pkt := <-sub.Packets():
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
	if d, dr, b := sub.Stats(); d != 3 || dr != 0 || b == 0 {
		t.Fatalf("sub stats = (%d,%d,%d), want (3,0,>0)", d, dr, b)
	}
}

func TestHubDropWithoutBlocking(t *testing.T) {
	h := NewHub()
	// 容量 1，且无人消费：模拟慢消费者。
	ch := make(chan event.Packet, 1)
	sub, cancel := h.Subscribe("s1", ch)
	defer cancel()

	const total = 100
	delivered, droppedBusy, droppedNoSub := h.Deliver("s1", makeBatch(total))
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1 (容量 1)", delivered)
	}
	if droppedBusy != total-1 || droppedNoSub != 0 {
		t.Fatalf("dropped = (%d,%d), want (%d,0)", droppedBusy, droppedNoSub, total-1)
	}
	if _, dr, _ := sub.Stats(); dr != total-1 {
		t.Fatalf("sub dropped = %d, want %d", dr, total-1)
	}
	// channel 里应恰好还有 1 个包可读。
	select {
	case <-sub.Packets():
	default:
		t.Fatal("expected 1 buffered packet")
	}
	select {
	case <-sub.Packets():
		t.Fatal("expected channel to be drained")
	default:
	}
}

// 多订阅者：一个满一个不满时，满的那个的丢包也必须被逐个记录（review 修复项）。
func TestHubDeliverMultipleSubscribersPartialFull(t *testing.T) {
	h := NewHub()
	full := make(chan event.Packet, 1)
	subFull, cancelFull := h.Subscribe("s1", full)
	defer cancelFull()
	roomy := make(chan event.Packet, 8)
	subRoomy, cancelRoomy := h.Subscribe("s1", roomy)
	defer cancelRoomy()

	const total = 3
	delivered, droppedBusy, droppedNoSub := h.Deliver("s1", makeBatch(total))
	if droppedNoSub != 0 {
		t.Fatalf("droppedNoSub = %d, want 0", droppedNoSub)
	}
	// delivered/droppedBusy 按订阅者逐次计：full 收 1 丢 2，roomy 收 3。
	if delivered != 4 || droppedBusy != 2 {
		t.Fatalf("delivered/droppedBusy = %d/%d, want 4/2", delivered, droppedBusy)
	}
	if _, drFull, _ := subFull.Stats(); drFull != total-1 {
		t.Fatalf("full sub dropped = %d, want %d", drFull, total-1)
	}
	if d, _, _ := subRoomy.Stats(); d != total {
		t.Fatalf("roomy sub delivered = %d, want %d", d, total)
	}
}

func TestHubDropNoSubscriber(t *testing.T) {
	h := NewHub()
	delivered, droppedBusy, droppedNoSub := h.Deliver("no-such-session", makeBatch(5))
	if delivered != 0 || droppedBusy != 0 || droppedNoSub != 5 {
		t.Fatalf("delivered/busy/noSub = %d/%d/%d, want 0/0/5", delivered, droppedBusy, droppedNoSub)
	}
	_, _, n := h.Stats()
	if n != 5 {
		t.Fatalf("droppedNoSub = %d, want 5", n)
	}
}

func TestHubUnsubscribeThenCloseSafe(t *testing.T) {
	h := NewHub()
	ch := make(chan event.Packet, 4)
	_, cancel := h.Subscribe("s1", ch)

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
