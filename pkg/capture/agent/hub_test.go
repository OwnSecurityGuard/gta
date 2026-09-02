package agent

import (
	"testing"
	"time"

	"gta/pkg/event"
)

// testBackoff 是测试用的背压窗口：足够短以保持测试快速，
// 又足够长以让"消费者最终追上"的用例稳定通过。
const testBackoff = 200 * time.Millisecond

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

// 慢消费者 + 无人消费：背压窗口耗尽后才丢，且丢弃数被逐个订阅者记录。
func TestHubDropsAfterBackpressureTimeout(t *testing.T) {
	h := NewHub()
	// 容量 1，且无人消费：模拟彻底跟不上的消费者。
	ch := make(chan event.Packet, 1)
	sub, cancel := h.Subscribe("s1", ch, WithDeliverTimeout(testBackoff))
	defer cancel()

	const total = 20
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

// 背压的核心价值：消费者只是慢（不是停摆）时不丢包——
// 旧实现在 channel 满的瞬间就丢，这正是要修的行为。
func TestHubBackpressureWaitsForSlowConsumer(t *testing.T) {
	h := NewHub()
	ch := make(chan event.Packet, 1) // 远小于总包数，必然触发背压路径
	sub, cancel := h.Subscribe("s1", ch, WithDeliverTimeout(2*time.Second))
	defer cancel()

	const total = 50
	// 消费者：拿一个包 sleep 一小会儿，模拟被 sqlite 写入拖慢的 capture 主循环。
	// 总耗时约 50×5ms=250ms，仍在背压窗口内，因此一个包都不该丢。
	got := make(chan int, 1)
	go func() {
		n := 0
		for n < total {
			select {
			case <-sub.Packets():
				n++
				time.Sleep(5 * time.Millisecond)
			case <-time.After(5 * time.Second):
				got <- n
				return
			}
		}
		got <- n
	}()

	delivered, droppedBusy, _ := h.Deliver("s1", makeBatch(total))
	if droppedBusy != 0 {
		t.Fatalf("droppedBusy = %d, want 0 (背压应等到消费者追上)", droppedBusy)
	}
	if delivered != total {
		t.Fatalf("delivered = %d, want %d", delivered, total)
	}
	if n := <-got; n != total {
		t.Fatalf("consumer received %d packets, want %d", n, total)
	}
}

// 多订阅者：一个满一个不满时，满的那个的丢包也必须被逐个记录（review 修复项）。
func TestHubDeliverMultipleSubscribersPartialFull(t *testing.T) {
	h := NewHub()
	full := make(chan event.Packet, 1)
	subFull, cancelFull := h.Subscribe("s1", full, WithDeliverTimeout(-1)) // 非阻塞，便于断言
	defer cancelFull()
	roomy := make(chan event.Packet, 8)
	subRoomy, cancelRoomy := h.Subscribe("s1", roomy, WithDeliverTimeout(-1))
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
	// 非阻塞：本用例验证的是 cancel 与 Deliver 的竞态，不是背压超时。
	_, cancel := h.Subscribe("s1", ch, WithDeliverTimeout(-1))

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
