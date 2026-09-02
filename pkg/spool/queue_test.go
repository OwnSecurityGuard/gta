package spool

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	agentproto "gta/pkg/capture/agent/proto"
)

func pkt(id string) *agentproto.RawPacket {
	return &agentproto.RawPacket{
		Id:          id,
		Raw:         []byte("frame-" + id),
		LinkType:    1,
		TimestampNs: 1700000000000000000,
		Protocol:    "tcp",
	}
}

func open(t *testing.T, opts Options) *Queue {
	t.Helper()
	q, err := Open(t.TempDir(), opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func mustNext(t *testing.T, q *Queue, n int) []*agentproto.RawPacket {
	t.Helper()
	out, err := q.Next(n)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	return out
}

func ids(pkts []*agentproto.RawPacket) []string {
	out := make([]string, 0, len(pkts))
	for _, p := range pkts {
		out = append(out, p.GetId())
	}
	return out
}

func equalIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// 基本链路：落盘 → 取出 → Ack，顺序必须与写入一致。
func TestAppendNextAckOrder(t *testing.T) {
	q := open(t, Options{})
	for _, id := range []string{"a", "b", "c"} {
		if err := q.Append(pkt(id)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if n, _ := q.Depth(); n != 3 {
		t.Fatalf("depth = %d, want 3", n)
	}
	got := ids(mustNext(t, q, 10))
	if !equalIDs(got, []string{"a", "b", "c"}) {
		t.Fatalf("next = %v, want [a b c]", got)
	}
	// 未确认前它们是「在途」：重复 Next 不再交付，避免同一条被并发发两次。
	if again := mustNext(t, q, 10); len(again) != 0 {
		t.Fatalf("next before ack = %v, want empty (in flight)", ids(again))
	}
	if err := q.AckN(2); err != nil {
		t.Fatalf("ack: %v", err)
	}
	// c 仍在途，需 Requeue 后才重新交付（发送失败重发的路径）。
	q.Requeue()
	if got := ids(mustNext(t, q, 10)); !equalIDs(got, []string{"c"}) {
		t.Fatalf("next after ack2+requeue = %v, want [c]", got)
	}
	if err := q.AckN(1); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if n, b := q.Depth(); n != 0 || b != 0 {
		t.Fatalf("depth after full ack = (%d,%d), want (0,0)", n, b)
	}
	if got := mustNext(t, q, 10); len(got) != 0 {
		t.Fatalf("next on empty queue = %v, want empty", ids(got))
	}
}

// 发送失败 → Requeue → 重发：断线重连的续传路径必须重投在途记录。
func TestRequeueRedeliversInFlight(t *testing.T) {
	q := open(t, Options{})
	for _, id := range []string{"a", "b", "c"} {
		if err := q.Append(pkt(id)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// 模拟发出 a 后、b 正在发时流断开：a 已确认，b 在途未确认。
	if got := ids(mustNext(t, q, 1)); !equalIDs(got, []string{"a"}) {
		t.Fatalf("first delivery = %v, want [a]", got)
	}
	if err := q.AckN(1); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if got := ids(mustNext(t, q, 1)); !equalIDs(got, []string{"b"}) {
		t.Fatalf("second delivery = %v, want [b]", got)
	}
	q.Requeue() // 重建流：在途的全部放回队头
	if got := ids(mustNext(t, q, 10)); !equalIDs(got, []string{"b", "c"}) {
		t.Fatalf("after requeue = %v, want [b c]", got)
	}
	if q.InFlight() != 2 {
		t.Fatalf("in flight = %d, want 2", q.InFlight())
	}
	// AckN 不能越过在途条数，防止把没发出去的包标记为已送达。
	if err := q.AckN(99); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if n, _ := q.Depth(); n != 0 {
		t.Fatalf("depth after over-ack = %d, want 0 (only in-flight confirmed)", n)
	}
}

// 核心用例：Ack 之后进程重启（Close → 重新 Open），未确认的数据必须还在
// 且从断点继续——这就是断电续传。
func TestResumeAfterRestart(t *testing.T) {
	dir := t.TempDir()
	q, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		if err := q.Append(pkt(id)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// 只确认前两个后"断电"（不 Close 也要能恢复，这里额外验证正常 Close 路径）。
	if err := q.AckN(len(mustNext(t, q, 2))); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	q2, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer q2.Close()
	if got := ids(mustNext(t, q2, 10)); !equalIDs(got, []string{"c", "d"}) {
		t.Fatalf("after restart = %v, want [c d]", got)
	}
	// 续传后继续追加，新数据排在未确认数据之后（在途的先确认，游标才前进）。
	if err := q2.AckN(2); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if err := q2.Append(pkt("e")); err != nil {
		t.Fatalf("append after restart: %v", err)
	}
	if got := ids(mustNext(t, q2, 10)); !equalIDs(got, []string{"e"}) {
		t.Fatalf("new data after resume = %v, want [e]", got)
	}
}

// 崩溃（无 Close）后重启：游标没来得及落盘时最多重推，绝不丢。
func TestResumeAfterCrashWithoutClose(t *testing.T) {
	dir := t.TempDir()
	q, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := q.Append(pkt(id)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// 模拟进程被 kill：直接丢弃句柄，不做任何收尾。
	q.mu.Lock()
	for _, f := range q.files {
		_ = f.Close()
	}
	q.mu.Unlock()

	q2, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("reopen after crash: %v", err)
	}
	defer q2.Close()
	got := ids(mustNext(t, q2, 10))
	if !equalIDs(got, []string{"a", "b", "c"}) {
		t.Fatalf("after crash = %v, want [a b c]", got)
	}
}

// 崩溃留下半条记录：必须丢弃残缺尾部且不影响后续 Append。
func TestTruncatePartialTailRecord(t *testing.T) {
	dir := t.TempDir()
	q, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, id := range []string{"a", "b"} {
		if err := q.Append(pkt(id)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// 人为在文件尾追加半个长度前缀，模拟写到一半断电。
	path := filepath.Join(dir, "seg-000000001.bin")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open seg: %v", err)
	}
	if _, err := f.Write([]byte{0x00, 0x10}); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	_ = f.Close()

	q2, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer q2.Close()
	if got := ids(mustNext(t, q2, 10)); !equalIDs(got, []string{"a", "b"}) {
		t.Fatalf("after partial tail = %v, want [a b]", got)
	}
	// 残缺尾部被截断后，新数据能正常落盘并读出。
	if err := q2.Append(pkt("c")); err != nil {
		t.Fatalf("append after truncation: %v", err)
	}
	if err := q2.AckN(2); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if got := ids(mustNext(t, q2, 10)); !equalIDs(got, []string{"c"}) {
		t.Fatalf("append after truncation = %v, want [c]", got)
	}
}

// segment 滚动：跨文件后顺序与续传都必须正确，且旧 segment 被确认后要删掉。
func TestSegmentRollAndCleanup(t *testing.T) {
	dir := t.TempDir()
	// 1KB 滚动：每条记录约 20 字节，写 100 条必然跨越多个 segment。
	q, err := Open(dir, Options{SegmentBytes: 1024})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	const total = 100
	for i := 0; i < total; i++ {
		if err := q.Append(pkt(string(rune('a' + i%26)) + "-" + itoa(i))); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	// 跨 segment 时 Next 一次只返回同段内的连续区间，分批取完应得 total 条。
	var got []string
	for len(got) < total {
		batch := mustNext(t, q, 16)
		if len(batch) == 0 {
			t.Fatalf("stuck after %d packets", len(got))
		}
		got = append(got, ids(batch)...)
		if err := q.AckN(len(batch)); err != nil {
			t.Fatalf("ack: %v", err)
		}
	}
	if len(got) != total {
		t.Fatalf("got %d packets, want %d", len(got), total)
	}
	for i := 0; i < total; i++ {
		want := string(rune('a'+i%26)) + "-" + itoa(i)
		if got[i] != want {
			t.Fatalf("packet %d id = %q, want %q", i, got[i], want)
		}
	}
	// 全部确认后：旧 segment 应被清理，只剩当前写文件。
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	segs, err := listSegments(dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(segs) != 1 {
		t.Fatalf("segments after full ack = %v, want exactly 1 (current write segment)", segs)
	}
}

// 磁盘上限：触顶返回 ErrFull 且被拒的包计入 Dropped，已缓存的历史不受影响。
func TestMaxBytesRejectsNewPackets(t *testing.T) {
	q := open(t, Options{MaxBytes: 1024})
	appended := 0
	var full bool
	for i := 0; i < 500; i++ {
		err := q.Append(pkt(itoa(i)))
		if errors.Is(err, ErrFull) {
			full = true
			break
		}
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		appended++
	}
	if !full {
		t.Fatal("expected ErrFull before 500 packets with 1KB limit")
	}
	if appended == 0 {
		t.Fatal("expected at least one packet to fit")
	}
	if q.Dropped() != 1 {
		t.Fatalf("dropped = %d, want 1", q.Dropped())
	}
	// 已有的数据仍可读且顺序正确。
	if got := ids(mustNext(t, q, 1000)); len(got) != appended {
		t.Fatalf("readable = %d, want %d", len(got), appended)
	}
}

// 并发安全：Append 与 Next/AckN 在不同 goroutine 上跑，不应 data race 或乱序。
func TestConcurrentAppendAndAck(t *testing.T) {
	q := open(t, Options{})
	const total = 500
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < total; i++ {
			if err := q.Append(pkt(itoa(i))); err != nil {
				t.Errorf("append %d: %v", i, err)
				return
			}
		}
	}()

	var got []string
	for len(got) < total {
		batch := mustNext(t, q, 32)
		if len(batch) == 0 {
			select {
			case <-done:
				// 生产者结束且队列空：收尾。
				if len(got) != total {
					t.Fatalf("got %d packets, want %d", len(got), total)
				}
				return
			default:
				continue
			}
		}
		got = append(got, ids(batch)...)
		if err := q.AckN(len(batch)); err != nil {
			t.Fatalf("ack: %v", err)
		}
	}
	for i := 0; i < total; i++ {
		if got[i] != itoa(i) {
			t.Fatalf("packet %d id = %q, want %q", i, got[i], itoa(i))
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
