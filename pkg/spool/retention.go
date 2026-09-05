package spool

// retention.go：spool 的归档模式（v2 探针本地留存，docs/plans/2026-09-05 §8.2）。
//
// 双游标语义：
//   - send-cursor（cursor.json，原有）：上行未确认位置，AckN 只推进它，不删数据；
//   - retention（本文件）：独立清理"已全部确认 且 超出保留窗口"的段。
//
// 铁律：retention 绝不删除未确认数据。未确认积压撑爆上行配额时仍走 ErrFull
//（丢新包并计数），与"宁可丢新包也不覆盖已缓存历史"的既有约定一致。

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	agentproto "gametrace/pkg/capture/agent/proto"
)

// EnableRetention 打开归档模式：AckN 不再删除段，改由 EnforceRetention 按窗口清理。
// 可在 Open 后任意时刻调用（含运行中调整参数）；传 nil 关闭归档模式（恢复发后即焚）。
func (q *Queue) EnableRetention(r *Retention) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if r == nil {
		q.retention = nil
		return
	}
	rr := *r
	q.retention = &rr
}

// noteSegRecord 在 Append 后更新段元数据（调用方须持有 mu）。
func (q *Queue) noteSegRecord(seg uint64, pkt *agentproto.RawPacket, total int64) {
	meta, ok := q.segMeta[seg]
	if !ok {
		meta = &segInfo{}
		q.segMeta[seg] = meta
	}
	tsMs := pkt.GetTimestampNs() / int64(time.Millisecond)
	if tsMs <= 0 {
		tsMs = time.Now().UnixMilli()
	}
	if meta.FirstMs == 0 || tsMs < meta.FirstMs {
		meta.FirstMs = tsMs
	}
	if tsMs > meta.LastMs {
		meta.LastMs = tsMs
	}
	meta.Packets++
	meta.Bytes += uint64(total)
}

// segFullyConfirmedLocked 报告某段是否已全部确认（段末偏移落在读游标之前）。
// 调用方须持有 mu。
func (q *Queue) segFullyConfirmedLocked(seg uint64) bool {
	head := q.readCurLocked()
	if head.Segment > seg {
		return true
	}
	if head.Segment < seg {
		return false
	}
	// 同段：读游标越过段内最后一条记录末尾才算全部确认。
	meta := q.segMeta[seg]
	segEnd := int64(0)
	if meta != nil {
		segEnd = int64(meta.Bytes)
	} else if f, err := q.fileFor(seg); err == nil {
		if fi, serr := f.Stat(); serr == nil {
			segEnd = fi.Size()
		}
	}
	return head.Offset >= segEnd
}

// EnforceRetention 执行一次留存清理：删除"已全部确认 且 超出保留窗口"的段，
// 已确认数据总量超 MaxBytes 时从最老开始删（同样只删已确认段）。
// 返回被删除的段数。retention 未开启时为 no-op。
// 未确认数据永不被删除；它们的配额由 Append 的 ErrFull（maxBytes）独立约束。
func (q *Queue) EnforceRetention(now time.Time) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.retention == nil || q.closed {
		return 0, nil
	}
	var cutoffMs int64
	if q.retention.MaxAge > 0 {
		cutoffMs = now.Add(-q.retention.MaxAge).UnixMilli()
	}

	// 已确认数据总量 = 全部留存字节 - 未确认字节。
	totalBytes := int64(0)
	for seg, meta := range q.segMeta {
		if q.segFullyConfirmedLocked(seg) {
			totalBytes += int64(meta.Bytes)
		}
	}

	removed := 0
	// 段号升序 = 时间升序（滚动写），从最老开始评估。
	for seg := range q.segMeta {
		meta := q.segMeta[seg]
		confirmed := q.segFullyConfirmedLocked(seg)
		if !confirmed {
			continue
		}
		expired := cutoffMs > 0 && meta.LastMs > 0 && meta.LastMs < cutoffMs
		overBudget := q.retention.MaxBytes > 0 && totalBytes > q.retention.MaxBytes
		if !expired && !overBudget {
			continue
		}
		q.dropSegment(seg)
		totalBytes -= int64(meta.Bytes)
		removed++
	}
	if removed > 0 {
		_ = q.saveAllSegMetaLocked()
	}
	return removed, nil
}

// RetentionStats 返回留存观测：段数、总字节、已确认字节、未确认字节。
type RetentionStats struct {
	Segments   int
	TotalBytes int64
	Confirmed  int64
	Pending    int64
}

// Stats 读取留存观测快照。
func (q *Queue) RetentionStats() RetentionStats {
	q.mu.Lock()
	defer q.mu.Unlock()
	st := RetentionStats{Segments: len(q.segMeta)}
	for seg, meta := range q.segMeta {
		st.TotalBytes += int64(meta.Bytes)
		if q.segFullyConfirmedLocked(seg) {
			st.Confirmed += int64(meta.Bytes)
		}
	}
	st.Pending = st.TotalBytes - st.Confirmed
	return st
}

// SegmentInfo 是一个留存段的摘要（归档查询用，时间单位毫秒）。
type SegmentInfo struct {
	SegID    string
	FirstMs  int64
	LastMs   int64
	Packets  uint64
	Bytes    uint64
	LinkType uint32
}

// Segments 返回全部留存段摘要（按时间升序）。段 LinkType 从首条记录解析（惰性，代价低）。
func (q *Queue) Segments() []SegmentInfo {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]SegmentInfo, 0, len(q.segMeta))
	for seg, meta := range q.segMeta {
		if meta.Packets == 0 {
			continue // 空段（刚滚动还没写）
		}
		info := SegmentInfo{
			SegID:   segIDString(seg),
			FirstMs: meta.FirstMs,
			LastMs:  meta.LastMs,
			Packets: meta.Packets,
			Bytes:   meta.Bytes,
		}
		// LinkType：段内第一条记录。只读一次，代价可忽略（查询是低频操作）。
		if lt, err := q.firstLinkTypeLocked(seg); err == nil {
			info.LinkType = lt
		}
		out = append(out, info)
	}
	// 按段号升序返回（与写入时间序一致）。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].FirstMs < out[j-1].FirstMs; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// firstLinkTypeLocked 读段内第一条记录的 link_type（调用方须持有 mu）。
func (q *Queue) firstLinkTypeLocked(seg uint64) (uint32, error) {
	f, err := q.fileFor(seg)
	if err != nil {
		return 0, err
	}
	hdr := make([]byte, headerSize)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		return 0, err
	}
	n := int64(binary.BigEndian.Uint32(hdr))
	body := make([]byte, n)
	if _, err := f.ReadAt(body, headerSize); err != nil {
		return 0, err
	}
	var pkt agentproto.RawPacket
	if err := proto.Unmarshal(body, &pkt); err != nil {
		return 0, err
	}
	return pkt.GetLinkType(), nil
}

// ReadSegment 遍历某段内的全部记录（原始帧），按写入顺序回调 fn。
// segID 形如 "seg-000000001"（Segments() 返回的 SegID）。
// 回放导入用：保留原始时间戳，顺序与抓包一致。
func (q *Queue) ReadSegment(segID string, fn func(*agentproto.RawPacket) error) error {
	seg, err := parseSegID(segID)
	if err != nil {
		return err
	}
	q.mu.Lock()
	f, err := q.fileFor(seg)
	if err != nil {
		q.mu.Unlock()
		return fmt.Errorf("spool: open segment %s: %w", segID, err)
	}
	size := int64(0)
	if fi, serr := f.Stat(); serr == nil {
		size = fi.Size()
	}
	q.mu.Unlock()

	// 锁外逐条读：回放是长操作，不能占住队列锁（Append/Next 并发进行）。
	rd, err := os.Open(q.segPath(seg))
	if err != nil {
		return fmt.Errorf("spool: open segment %s: %w", segID, err)
	}
	defer rd.Close()
	br := bufio.NewReaderSize(rd, 256*1024)
	hdr := make([]byte, headerSize)
	var total int64
	for total < size {
		if _, err := ioReadFull(br, hdr); err != nil {
			return nil // 尾部不足一个 header：停（与扫描语义一致）
		}
		n := int64(binary.BigEndian.Uint32(hdr))
		if n <= 0 || total+headerSize+n > size {
			return nil // 残缺记录：停
		}
		body := make([]byte, n)
		if _, err := ioReadFull(br, body); err != nil {
			return fmt.Errorf("spool: read record in %s: %w", segID, err)
		}
		total += headerSize + n
		var pkt agentproto.RawPacket
		if err := proto.Unmarshal(body, &pkt); err != nil {
			return fmt.Errorf("spool: unmarshal record in %s: %w", segID, err)
		}
		if err := fn(&pkt); err != nil {
			return err
		}
	}
	return nil
}

// ioReadFull 是 io.ReadFull 的本地别名（避免仅为一个函数引入 io+errors 组合噪音）。
func ioReadFull(r *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func segIDString(seg uint64) string { return fmt.Sprintf("seg-%09d", seg) }

func parseSegID(id string) (uint64, error) {
	id = strings.TrimPrefix(id, "seg-")
	id = strings.TrimSuffix(id, ".bin")
	var n uint64
	if _, err := fmt.Sscanf(id, "%d", &n); err != nil {
		return 0, fmt.Errorf("spool: invalid segment id %q", id)
	}
	return n, nil
}

// ---- 段元数据持久化（idx 文件） ----

func (q *Queue) segIdxPath(seg uint64) string {
	return filepath.Join(q.dir, fmt.Sprintf("seg-%09d.idx.json", seg))
}

// loadSegMeta 载入已有段的元数据；缺失 idx 的段用文件 mtime 估算（first≈mtime，last≈mtime）。
func (q *Queue) loadSegMeta(segs []uint64) {
	for _, seg := range segs {
		b, err := os.ReadFile(q.segIdxPath(seg))
		var meta segInfo
		if err == nil && json.Unmarshal(b, &meta) == nil {
			q.segMeta[seg] = &meta
			continue
		}
		if fi, serr := os.Stat(q.segPath(seg)); serr == nil {
			mt := fi.ModTime().UnixMilli()
			q.segMeta[seg] = &segInfo{FirstMs: mt, LastMs: mt}
		}
	}
}

// saveAllSegMetaLocked 把内存中的段元数据落盘（调用方须持有 mu）。
func (q *Queue) saveAllSegMetaLocked() error {
	var firstErr error
	for seg, meta := range q.segMeta {
		b, err := json.Marshal(meta)
		if err != nil {
			continue
		}
		tmp := q.segIdxPath(seg) + ".tmp"
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := os.Rename(tmp, q.segIdxPath(seg)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
