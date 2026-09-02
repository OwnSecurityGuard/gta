// Package spool 提供 gta-agent 上行链路的磁盘缓冲队列（断电续传）。
//
// 解决的问题：agent 与 pipeline 断线时，内存里未发送的包会直接丢失；
// agent 进程重启（崩溃 / 升级 / 断电）时同样全丢。本包把「已抓到但未确认送达」
// 的包落到磁盘，发送成功后才推进消费游标，因此：
//
//   - 断线期间：包继续落盘，由磁盘吸收积压，不丢；
//   - 重连之后：从上次未确认的位置继续推送（断点续传）；
//   - 进程重启：重新 Open 同一目录即可接着推（断电续传）。
//
// 语义是 at-least-once：Send 成功才 Ack，失败/断线会重发。重发是幂等的，
// 因为 RawPacket.Id 由 agent 侧生成（UUIDv7），服务端落库用 INSERT OR REPLACE。
//
// 磁盘格式（追加写）：
//
//	<dir>/seg-000000001.bin   记录序列：[uint32 大端长度][protobuf 字节]...
//	<dir>/cursor.json         消费游标：{"segment":N,"offset":M}
//
// 单条记录的长度前缀使崩溃留下的半条记录可被安全丢弃（扫描到不完整处即停并截断）。
package spool

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	agentproto "gta/pkg/capture/agent/proto"
)

// 参数。调参入口见 Options。
const (
	// headerSize 是每条记录的长度前缀字节数（uint32 大端）。
	headerSize = 4

	// DefaultMaxBytes 是未确认数据的默认磁盘上限（512MB）。
	// 触顶后拒绝新包（ErrFull）：宁可丢新包，也不覆盖已缓存的连续历史。
	DefaultMaxBytes = 512 << 20

	// DefaultSegmentBytes 是单个 segment 文件的滚动阈值（64MB），
	// 使已消费数据能整段删除，无需重写文件。
	DefaultSegmentBytes = 64 << 20

	// syncEveryBytes / syncEvery 控制 fsync 频率。每条都 fsync 在高包率下会
	// 拖垮抓包链路，故按「累计字节 or 时间间隔」批量刷：进程崩溃不丢，
	// 系统断电最多丢最后一个刷盘窗口（默认 4MB / 200ms）。
	syncEveryBytes = 4 << 20
	syncEvery      = 200 * time.Millisecond

	// cursorSyncEvery 是游标持久化的节流间隔（每 N 条 Ack 落一次 cursor）。
	// 崩溃时最多重复推送 N 条——幂等，可接受。Close 时无条件落盘。
	cursorSyncEvery = 512
)

// ErrFull 表示队列达到磁盘上限，该包未被接收（调用方应丢弃并计数）。
var ErrFull = errors.New("spool queue full")

// Options 是队列参数；零值字段取默认值。
type Options struct {
	// MaxBytes 是未确认数据的磁盘上限；<=0 取 DefaultMaxBytes。
	MaxBytes int64
	// SegmentBytes 是单个 segment 文件的滚动阈值；<=0 取 DefaultSegmentBytes。
	SegmentBytes int64
}

// entry 是内存中的记录索引（未确认条目），值语义、体积小。
type entry struct {
	seg uint64
	off int64
	n   int // 记录体长度（不含长度前缀）
}

// cursor 是磁盘上的一个位置：segment 号 + 文件内偏移。
type cursor struct {
	Segment uint64 `json:"segment"`
	Offset  int64  `json:"offset"`
}

// Queue 是落盘缓冲队列。所有方法并发安全。
//
// 位置有两个，勿混淆：
//   - wrCur：写位置（下一个 Append 落盘的地方），只前进；
//   - nextIdx：Next 已交付但尚未 Ack 的「在途」条数（entries[:nextIdx]）；
//   - 读游标：队头位置 = entries[0]；队列排空时等于 wrCur。
//
// cursor.json 持久化的始终是「读游标」，因此重启后从第一个未确认的记录续传。
// AckN 只能确认在途的条目——这条约束保证「确认」与「实际发送」不会错位。
type Queue struct {
	dir      string
	maxBytes int64
	segBytes int64

	mu      sync.Mutex
	entries []entry   // 未确认条目（entries[0] 是队头）
	nextIdx int       // 在途条数：entries[:nextIdx] 已交付给调用方，等 Ack
	bytes   int64     // 未确认字节数（含长度前缀）
	wrCur   cursor    // 写位置
	wrFile  *os.File  // 当前写 segment 的句柄（O_RDWR，追加写）
	files   map[uint64]*os.File // 已打开的 segment（惰性，滚动/清理时关闭）

	recBuf     []byte    // Append 时的序列化复用缓冲（header + body）
	unsynced   int64     // 自上次 fsync 起写入的字节数
	lastSync   time.Time // 上次 fsync 时刻
	ackSinceCS int       // 自上次游标落盘起的 Ack 条数
	dropped    uint64    // 因 ErrFull 或写盘失败被丢弃的包数
	closed     bool
}

// Open 打开（必要时创建）一个磁盘队列。dir 不存在时创建。
// 已存在的数据（含上次进程未发完的积压）会被重新索引并从游标处续传。
func Open(dir string, opts Options) (*Queue, error) {
	if dir == "" {
		return nil, errors.New("spool: dir is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("spool: create dir %s: %w", dir, err)
	}
	q := &Queue{
		dir:      dir,
		maxBytes: opts.MaxBytes,
		segBytes: opts.SegmentBytes,
		files:    make(map[uint64]*os.File),
		lastSync: time.Now(),
	}
	if q.maxBytes <= 0 {
		q.maxBytes = DefaultMaxBytes
	}
	if q.segBytes <= 0 {
		q.segBytes = DefaultSegmentBytes
	}
	if err := q.recover(); err != nil {
		_ = q.Close()
		return nil, err
	}
	return q, nil
}

// recover 载入游标、扫描 segment 重建未确认条目索引、定位写位置。
func (q *Queue) recover() error {
	segs, err := listSegments(q.dir)
	if err != nil {
		return err
	}
	// 写位置 = 最后一个 segment 的末尾（全新目录则创建 1 号空文件）。
	if len(segs) > 0 {
		last := segs[len(segs)-1]
		fi, err := os.Stat(q.segPath(last))
		if err != nil {
			return fmt.Errorf("spool: stat segment %d: %w", last, err)
		}
		q.wrCur = cursor{Segment: last, Offset: fi.Size()}
	} else {
		if _, err := q.openSegment(1); err != nil {
			return err
		}
		q.wrCur = cursor{Segment: 1, Offset: 0}
	}
	if err := q.openWrite(); err != nil {
		return err
	}

	// 读游标：cursor.json 优先，缺省从 1 号 segment 开头。
	cur := cursor{Segment: 1}
	if c, ok, err := loadCursor(q.dir); err != nil {
		return err
	} else if ok {
		cur = c
	}
	// 删除读游标之前的 segment：它们已全部确认，留着只占磁盘。
	for _, seg := range segs {
		if seg < cur.Segment {
			q.dropSegment(seg)
		}
	}
	return q.scanFrom(cur)
}

// scanFrom 从 (seg, off) 顺序读记录直至 EOF，重建未确认条目索引。
// 遇到不完整的尾部记录（崩溃留下的半条）就地截断，避免污染后续 Append。
func (q *Queue) scanFrom(c cursor) error {
	seg := c.Segment
	off := c.Offset
	for {
		f, err := q.fileFor(seg)
		if err != nil {
			if os.IsNotExist(err) {
				return nil // 读游标已越过所有数据（正常：全部已确认）
			}
			return err
		}
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		stop, err := q.scanSegment(f, seg, off, fi.Size())
		if err != nil {
			return err
		}
		if !stop.atEOF {
			// 半条记录：截断到最后一个完整记录末尾，后续 Append 从这里续写。
			if err := f.Truncate(stop.offset); err != nil {
				return fmt.Errorf("spool: truncate segment %d: %w", seg, err)
			}
			q.wrCur = cursor{Segment: seg, Offset: stop.offset}
			if err := q.openWrite(); err != nil {
				return err
			}
			return nil
		}
		// 还有下一个 segment 就继续，否则写位置已在 recover 里定好。
		next := seg + 1
		if _, err := os.Stat(q.segPath(next)); err != nil {
			return nil
		}
		seg, off = next, 0
	}
}

// scanStop 是单段扫描的结果。
type scanStop struct {
	offset int64 // 最后一个完整记录之后的位置
	atEOF  bool  // 是否恰好停在文件尾（无残缺记录）
}

func (q *Queue) scanSegment(f *os.File, seg uint64, off, size int64) (scanStop, error) {
	if off >= size {
		// 游标已在文件尾（或越界）：没有残缺记录，也不该截断。
		return scanStop{offset: size, atEOF: true}, nil
	}
	hdr := make([]byte, headerSize)
	for off+headerSize <= size {
		if _, err := f.ReadAt(hdr, off); err != nil {
			return scanStop{offset: off}, err
		}
		n := int64(binary.BigEndian.Uint32(hdr))
		if n <= 0 || off+headerSize+n > size {
			return scanStop{offset: off, atEOF: false}, nil // 不完整
		}
		q.entries = append(q.entries, entry{seg: seg, off: off, n: int(n)})
		q.bytes += headerSize + n
		off += headerSize + n
	}
	if off != size {
		// 剩余不足一个长度前缀：截断剔除。
		return scanStop{offset: off, atEOF: false}, nil
	}
	return scanStop{offset: off, atEOF: true}, nil
}

// Append 把一个包追加到队尾（落盘）。返回 ErrFull 表示已达磁盘上限、
// 该包未被接收（调用方丢弃并计数）。其他错误同样表示未被接收。
func (q *Queue) Append(pkt *agentproto.RawPacket) error {
	rec, err := proto.Marshal(pkt)
	if err != nil {
		return fmt.Errorf("spool: marshal packet: %w", err)
	}
	if int64(len(rec)) > q.maxBytes {
		return ErrFull // 单包就超过整个配额，无解
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return errors.New("spool: queue closed")
	}
	total := headerSize + int64(len(rec))
	if q.bytes+total > q.maxBytes {
		q.dropped++
		return ErrFull
	}
	if err := q.writeRecord(rec); err != nil {
		q.dropped++
		return err
	}
	q.entries = append(q.entries, entry{seg: q.wrCur.Segment, off: q.wrCur.Offset, n: len(rec)})
	q.wrCur.Offset += total
	q.bytes += total
	return q.maybeSync(total)
}

// writeRecord 写一条完整记录（含 segment 滚动），调用方须持有 mu。
// 一次 Write 落盘 header+body：不引入用户态缓冲，使 Next 的 ReadAt 总能读到
// 刚 Append 的数据（缓冲写会造成读写不一致）。
func (q *Queue) writeRecord(rec []byte) error {
	if q.wrCur.Offset >= q.segBytes {
		if err := q.rollSegment(); err != nil {
			return err
		}
	}
	buf := q.recBuf[:0]
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(rec)))
	buf = append(buf, rec...)
	if _, err := q.wrFile.Write(buf); err != nil {
		return fmt.Errorf("spool: write record: %w", err)
	}
	q.recBuf = buf[:0] // 复用底层数组，下次 Append 免分配
	return nil
}

// rollSegment 滚动到下一个 segment（调用方须持有 mu）。
func (q *Queue) rollSegment() error {
	if err := q.syncLocked(); err != nil {
		return err
	}
	next := q.wrCur.Segment + 1
	if _, err := q.openSegment(next); err != nil {
		return err
	}
	q.wrCur = cursor{Segment: next, Offset: 0}
	return q.openWrite()
}

// maybeSync 按累计字节 / 时间间隔刷盘，调用方须持有 mu。
func (q *Queue) maybeSync(written int64) error {
	q.unsynced += written
	if q.unsynced < syncEveryBytes && time.Since(q.lastSync) < syncEvery {
		return nil
	}
	return q.syncLocked()
}

// syncLocked 把当前 segment fsync 到磁盘（调用方须持有 mu）。
func (q *Queue) syncLocked() error {
	if q.wrFile == nil {
		return nil
	}
	if err := q.wrFile.Sync(); err != nil {
		return fmt.Errorf("spool: sync: %w", err)
	}
	q.unsynced = 0
	q.lastSync = time.Now()
	return nil
}

// Next 从队头交付最多 n 条记录（不确认）。返回空切片表示队列已排空。
//
// 交付的记录处于「在途」：后续 Next 不会重复交付它们，直到调用 AckN 确认、
// 或调用 Requeue 把它们放回队头重发。调用方必须在发送成功后 AckN，
// 发送失败时 Requeue（或什么都不做，等下次 Open 自动从队头续传）。
func (q *Queue) Next(n int) ([]*agentproto.RawPacket, error) {
	if n <= 0 {
		n = 1
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	avail := len(q.entries) - q.nextIdx
	if avail <= 0 {
		return nil, nil
	}
	if n > avail {
		n = avail
	}

	// 只在同一 segment 内做一次连续读（跨 segment 留给下一次调用）。
	head := q.entries[q.nextIdx]
	count, span := 0, int64(0)
	for i := 0; i < n; i++ {
		e := q.entries[q.nextIdx+i]
		if e.seg != head.seg {
			break
		}
		span += headerSize + int64(e.n)
		count++
	}
	f, err := q.fileFor(head.seg)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, span)
	if _, err := f.ReadAt(buf, head.off); err != nil {
		return nil, fmt.Errorf("spool: read segment %d: %w", head.seg, err)
	}
	out := make([]*agentproto.RawPacket, 0, count)
	for i, off := 0, int64(0); i < count; i++ {
		size := int(binary.BigEndian.Uint32(buf[off:]))
		off += headerSize
		var pkt agentproto.RawPacket
		if err := proto.Unmarshal(buf[off:off+int64(size)], &pkt); err != nil {
			return nil, fmt.Errorf("spool: unmarshal record: %w", err)
		}
		off += int64(size)
		out = append(out, &pkt)
	}
	q.nextIdx += count
	return out, nil
}

// AckN 确认在途的前 n 条已成功送达并推进读游标。
// n 超过在途条数时只确认在途部分（防止调用方把未发送的数据误标记为已送达）。
func (q *Queue) AckN(n int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if n > q.nextIdx {
		n = q.nextIdx
	}
	if n <= 0 {
		return nil
	}
	for i := 0; i < n; i++ {
		q.bytes -= headerSize + int64(q.entries[i].n)
	}
	q.entries = q.entries[n:]
	if len(q.entries) == 0 {
		q.entries = nil
	}
	q.nextIdx -= n
	q.ackSinceCS += n
	if q.ackSinceCS >= cursorSyncEvery {
		if err := q.persistCursorLocked(); err != nil {
			return err
		}
	}
	q.cleanupLocked()
	return nil
}

// Requeue 把所有在途记录放回队头，使后续 Next 从队头重新交付它们。
// 用于发送失败后重建连接再发一次（断点续传）：这些记录从未被 Ack，必须重发。
func (q *Queue) Requeue() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.nextIdx = 0
}

// InFlight 返回已交付但未确认的条数（观测用）。
func (q *Queue) InFlight() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.nextIdx
}

// readCurLocked 返回当前读游标（调用方须持有 mu）。
func (q *Queue) readCurLocked() cursor {
	if len(q.entries) > 0 {
		return cursor{Segment: q.entries[0].seg, Offset: q.entries[0].off}
	}
	return q.wrCur // 队列排空：游标追到写位置
}

// cleanupLocked 删除已全部确认的 segment（读游标所在 segment 之前的所有文件）。
func (q *Queue) cleanupLocked() {
	head := q.readCurLocked().Segment
	for seg := range q.files {
		if seg < head {
			q.dropSegment(seg)
		}
	}
}

// dropSegment 关闭并删除一个 segment 文件（调用方须持有 mu）。
func (q *Queue) dropSegment(seg uint64) {
	if f, ok := q.files[seg]; ok {
		_ = f.Close()
		delete(q.files, seg)
	}
	_ = os.Remove(q.segPath(seg))
}

// Depth 返回未确认的条目数与字节数（观测用）。
func (q *Queue) Depth() (int, int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.entries), q.bytes
}

// Dropped 返回因队列满或写盘失败被丢弃的包数（观测用）。
func (q *Queue) Dropped() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

// Close 刷盘、持久化游标并关闭所有文件句柄。已确认与未确认的数据都留在磁盘上，
// 下次 Open 同一目录即可从读游标续传。
func (q *Queue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil
	}
	var err error
	if serr := q.syncLocked(); serr != nil && err == nil {
		err = serr
	}
	if cerr := q.persistCursorLocked(); cerr != nil && err == nil {
		err = cerr
	}
	q.closed = true
	for seg, f := range q.files {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
		delete(q.files, seg)
	}
	q.wrFile = nil
	return err
}

// fileFor 惰性打开 segment 文件并缓存句柄（调用方须持有 mu）。
func (q *Queue) fileFor(seg uint64) (*os.File, error) {
	if f, ok := q.files[seg]; ok {
		return f, nil
	}
	f, err := os.OpenFile(q.segPath(seg), os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("spool: open segment %d: %w", seg, err)
	}
	q.files[seg] = f
	return f, nil
}

// openSegment 创建并打开一个 segment（调用方须持有 mu）。
func (q *Queue) openSegment(seg uint64) (*os.File, error) {
	f, err := os.OpenFile(q.segPath(seg), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("spool: create segment %d: %w", seg, err)
	}
	q.files[seg] = f
	return f, nil
}

// openWrite 把当前写 segment 定位到文件尾并设为 wrFile（调用方须持有 mu）。
func (q *Queue) openWrite() error {
	f, err := q.fileFor(q.wrCur.Segment)
	if err != nil {
		return err
	}
	if _, err := f.Seek(q.wrCur.Offset, 0); err != nil {
		return fmt.Errorf("spool: seek segment end: %w", err)
	}
	q.wrFile = f
	return nil
}

// persistCursorLocked 原子写游标（临时文件 + rename），调用方须持有 mu。
func (q *Queue) persistCursorLocked() error {
	if err := saveCursor(q.dir, q.readCurLocked()); err != nil {
		return err
	}
	q.ackSinceCS = 0
	return nil
}

func (q *Queue) segPath(seg uint64) string {
	return filepath.Join(q.dir, fmt.Sprintf("seg-%09d.bin", seg))
}

const cursorFile = "cursor.json"

func saveCursor(dir string, c cursor) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, cursorFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("spool: write cursor tmp: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, cursorFile)); err != nil {
		return fmt.Errorf("spool: rename cursor: %w", err)
	}
	return nil
}

func loadCursor(dir string) (cursor, bool, error) {
	b, err := os.ReadFile(filepath.Join(dir, cursorFile))
	if err != nil {
		if os.IsNotExist(err) {
			return cursor{}, false, nil
		}
		return cursor{}, false, fmt.Errorf("spool: read cursor: %w", err)
	}
	var c cursor
	if err := json.Unmarshal(b, &c); err != nil {
		return cursor{}, false, fmt.Errorf("spool: parse cursor: %w", err)
	}
	if c.Segment == 0 {
		c.Segment = 1
	}
	return c, true, nil
}

// listSegments 返回目录里已存在的 segment 编号（升序）。
func listSegments(dir string) ([]uint64, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("spool: read dir: %w", err)
	}
	var out []uint64
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "seg-") || !strings.HasSuffix(name, ".bin") {
			continue
		}
		n, err := strconv.ParseUint(name[len("seg-"):len(name)-len(".bin")], 10, 64)
		if err != nil {
			continue // 无关文件，忽略
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
