package spool

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentproto "gta/pkg/capture/agent/proto"
)

// mkRawPacket 构造带指定时间戳的测试帧。
func mkRawPacket(tsMs int64, id string) *agentproto.RawPacket {
	return &agentproto.RawPacket{
		Id:          id,
		Raw:         []byte("frame-" + id),
		LinkType:    1,
		TimestampNs: tsMs * int64(time.Millisecond),
		Protocol:    "tcp",
	}
}

func TestRetention_AckDoesNotDelete(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	q, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	q.EnableRetention(&Retention{MaxAge: time.Hour})

	now := time.Now().UnixMilli()
	for i := 0; i < 10; i++ {
		if err := q.Append(mkRawPacket(now, fmt.Sprintf("pkt-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	batch, err := q.Next(10)
	if err != nil || len(batch) != 10 {
		t.Fatalf("next: %v %d", err, len(batch))
	}
	if err := q.AckN(10); err != nil {
		t.Fatal(err)
	}
	// 归档模式：确认后数据必须留在磁盘。
	stats := q.RetentionStats()
	if stats.Segments == 0 || stats.TotalBytes == 0 {
		t.Fatalf("retention mode must retain acknowledged data: %+v", stats)
	}
	if stats.Pending != 0 || stats.Confirmed != stats.TotalBytes {
		t.Fatalf("all data confirmed: %+v", stats)
	}
}

func TestRetention_OnlyDeletesExpiredConfirmed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	q, err := Open(dir, Options{SegmentBytes: 256}) // 小段：几条就滚动
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	q.EnableRetention(&Retention{MaxAge: time.Hour})

	now := time.Now()
	nowMs := now.UnixMilli()
	// 单条记录 > 段阈值：一段一包，老包和新包不会混进同一个段。
	big := func(tsMs int64, id string) *agentproto.RawPacket {
		p := mkRawPacket(tsMs, id)
		p.Raw = bytes.Repeat([]byte("x"), 300)
		return p
	}
	// 老段（2h 前）+ 新段（现在）。
	for i := 0; i < 6; i++ {
		if err := q.Append(big(now.Add(-2*time.Hour).UnixMilli(), fmt.Sprintf("old-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 6; i++ {
		if err := q.Append(big(nowMs, fmt.Sprintf("new-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	// 全部确认（发送成功）。Next 只在单段内交付，需循环排空。
	total := 0
	for {
		batch, err := q.Next(100)
		if err != nil {
			t.Fatal(err)
		}
		total += len(batch)
		if len(batch) == 0 {
			break
		}
	}
	if err := q.AckN(total); err != nil {
		t.Fatal(err)
	}
	removed, err := q.EnforceRetention(now)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 6 {
		t.Fatalf("6 expired confirmed segments should be removed, removed %d", removed)
	}
	segs := q.Segments()
	if len(segs) != 6 {
		t.Fatalf("only fresh segments should survive, got %d", len(segs))
	}
	for _, s := range segs {
		if s.LastMs > 0 && s.LastMs < now.Add(-time.Hour).UnixMilli() {
			t.Fatalf("expired segment survived: %+v", s)
		}
	}
}

func TestRetention_NeverDeletesUnconfirmed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	q, err := Open(dir, Options{SegmentBytes: 256})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	// MaxAge=1ns：任何数据都"超期"；MaxBytes=1B：任何数据都"超预算"。
	q.EnableRetention(&Retention{MaxAge: time.Nanosecond, MaxBytes: 1})

	now := time.Now().UnixMilli()
	for i := 0; i < 5; i++ {
		if err := q.Append(mkRawPacket(now, fmt.Sprintf("pkt-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	// 一条都没确认：retention 不得删除任何东西（铁律）。
	removed, err := q.EnforceRetention(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("unconfirmed data must never be deleted, removed %d segments", removed)
	}
	if stats := q.RetentionStats(); stats.Pending == 0 {
		t.Fatal("unconfirmed data should be pending")
	}

	// 确认一半：只有已确认部分可被清理。
	batch, err := q.Next(2)
	if err != nil || len(batch) != 2 {
		t.Fatalf("next: %v %d", err, len(batch))
	}
	if err := q.AckN(2); err != nil {
		t.Fatal(err)
	}
	// 注意：已确认段 = 只有当读游标越过整段才可删（半段的未确认部分挡住它）。
	removed, err = q.EnforceRetention(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	stats := q.RetentionStats()
	if removed > 0 && stats.Pending == 0 {
		t.Fatal("confirmed segments removed but pending data vanished")
	}
}

func TestRetention_ReopenRestoresMetadata(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	q, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for i := 0; i < 3; i++ {
		if err := q.Append(mkRawPacket(now, fmt.Sprintf("pkt-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Next(3); err != nil {
		t.Fatal(err)
	}
	_ = q.AckN(3)
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}

	// 重开：idx 恢复段时间戳；归档模式下老数据不被 recover 删除。
	q2, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer q2.Close()
	q2.EnableRetention(&Retention{MaxAge: time.Hour})
	segs := q2.Segments()
	if len(segs) == 0 {
		t.Fatal("segments lost after reopen (retention mode must keep acknowledged data)")
	}
	if segs[0].FirstMs <= 0 || segs[0].Packets != 3 {
		t.Fatalf("segment metadata not restored: %+v", segs[0])
	}
	// 上行游标：已确认数据不该重发（send-cursor 语义不变）。
	if n, _ := q2.Depth(); n != 0 {
		t.Fatalf("acknowledged data should not be redelivered, depth=%d", n)
	}
}

func TestRetention_ReadSegmentReplays(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	q, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	q.EnableRetention(&Retention{MaxAge: time.Hour})
	now := time.Now().UnixMilli()
	for i := 0; i < 4; i++ {
		if err := q.Append(mkRawPacket(now, fmt.Sprintf("pkt-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	segs := q.Segments()
	if len(segs) == 0 {
		t.Fatal("no segments")
	}
	var got []string
	err = q.ReadSegment(segs[0].SegID, func(pkt *agentproto.RawPacket) error {
		got = append(got, string(pkt.GetRaw()))
		if pkt.GetTimestampNs() <= 0 {
			return errors.New("timestamp lost in replay")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0] != "frame-pkt-0" || got[3] != "frame-pkt-3" {
		t.Fatalf("replay out of order or incomplete: %v", got)
	}
	if err := q.ReadSegment("seg-bogus", func(*agentproto.RawPacket) error { return nil }); err == nil {
		t.Fatal("invalid seg id should error")
	}
	if _, err := os.Stat(filepath.Join(dir, "seg-bogus.bin")); !os.IsNotExist(err) {
		t.Fatal("bogus id should not create files")
	}
}
