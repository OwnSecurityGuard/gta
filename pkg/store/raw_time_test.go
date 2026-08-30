package store

import (
	"context"
	"database/sql"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"gta/pkg/event"
)

func TestRawTimestamp_ZeroSentinel(t *testing.T) {
	if got := rawTimestamp(time.Time{}); got != 0 {
		t.Fatalf("zero time should map to 0, got %d", got)
	}
	now := time.Now()
	if got := rawTimestamp(now); got != now.UnixNano() {
		t.Fatalf("non-zero time should map to UnixNano, got %d", got)
	}
	if got := fromRawTimestamp(0); !got.IsZero() {
		t.Fatalf("0 should restore zero time, got %v", got)
	}
	if got := fromRawTimestamp(now.UnixNano()); !got.Equal(now) {
		t.Fatalf("UnixNano should restore time, got %v want %v", got, now)
	}
}

func TestScanPacketTime_DualFormat(t *testing.T) {
	now := time.Now().Truncate(time.Second) // 文本布局秒级截断前的可逆比较

	// INTEGER（迁移后的新格式）
	got, err := scanPacketTime(int64(now.UnixNano()))
	if err != nil || !got.Equal(now) {
		t.Fatalf("int64 scan: got %v err %v", got, err)
	}
	// time.Time：旧文本格式被 modernc 驱动读取时自动解析的形态
	got, err = scanPacketTime(now)
	if err != nil || !got.Equal(now) {
		t.Fatalf("time.Time scan: got %v err %v", got, err)
	}
	// 旧文本格式（驱动未识别时的字符串形态）
	legacy := now.Format("2006-01-02 15:04:05.999999999 -0700 MST")
	got, err = scanPacketTime(legacy)
	if err != nil || !got.Equal(now) {
		t.Fatalf("text scan: got %v err %v (input %q)", got, err, legacy)
	}
	// 零值哨兵
	got, err = scanPacketTime(int64(0))
	if err != nil || !got.IsZero() {
		t.Fatalf("zero sentinel scan: got %v err %v", got, err)
	}
	// NULL（MIN/MAX 空集）
	got, err = scanPacketTime(nil)
	if err != nil || !got.IsZero() {
		t.Fatalf("nil scan: got %v err %v", got, err)
	}
	// 不可解析文本 → 零值（不报错，调用方按零值处理）
	got, err = scanPacketTime("not-a-time")
	if err != nil || !got.IsZero() {
		t.Fatalf("garbage text scan: got %v err %v", got, err)
	}
}

func TestAppendRawPackets_IntegerTimestamp(t *testing.T) {
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now()
	src := netip.MustParseAddrPort("127.0.0.1:1")
	dst := netip.MustParseAddrPort("127.0.0.1:2")
	pkts := []event.Packet{
		{ID: "p1", Timestamp: now, Src: src, Dst: dst, Protocol: "tcp"},
		{ID: "p2", Timestamp: time.Time{}, Src: src, Dst: dst, Protocol: "tcp"},
	}
	if err := s.AppendRawPackets(context.Background(), pkts); err != nil {
		t.Fatal(err)
	}

	// 存储格式必须是 INTEGER，值等于 UnixNano；零值时间归一为 0。
	var typ string
	var ts int64
	if err := s.db.QueryRow("SELECT typeof(timestamp), timestamp FROM raw_packets WHERE id='p1'").Scan(&typ, &ts); err != nil {
		t.Fatal(err)
	}
	if typ != "integer" || ts != now.UnixNano() {
		t.Fatalf("p1: typeof=%s ts=%d want integer %d", typ, ts, now.UnixNano())
	}
	if err := s.db.QueryRow("SELECT typeof(timestamp), timestamp FROM raw_packets WHERE id='p2'").Scan(&typ, &ts); err != nil {
		t.Fatal(err)
	}
	if typ != "integer" || ts != 0 {
		t.Fatalf("p2 zero time: typeof=%s ts=%d want integer 0", typ, ts)
	}

	// 读回路径 roundtrip（ASC 排序：0 哨兵在前）。
	rows, err := s.QueryRawPackets(context.Background(), RawPacketQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	byID := map[string]time.Time{}
	for _, r := range rows {
		byID[r.ID] = r.Timestamp
	}
	if !byID["p1"].Equal(now) {
		t.Fatalf("p1 roundtrip: %v want %v", byID["p1"], now)
	}
	if !byID["p2"].IsZero() {
		t.Fatalf("p2 roundtrip: %v want zero", byID["p2"])
	}
}

func TestMigrateRawTimestamps_LegacyTextDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	// 1) 手工构造旧格式库：直接写文本时间戳（绕过 AppendRawPackets），
	//    模拟旧版本二进制写入的会话库。
	legacyNow := time.Now().Truncate(time.Second)
	func() {
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		schema := rawPacketsOnlySchema
		if _, err := db.Exec(schema); err != nil {
			t.Fatal(err)
		}
		for _, row := range []struct {
			id string
			ts time.Time
		}{
			{"old1", legacyNow},
			{"old2", legacyNow.Add(2 * time.Second)},
			{"old3", legacyNow.Add(time.Second)},
		} {
			// 旧格式：modernc 绑定 time.Time 落库的文本表示。
			if _, err := db.Exec("INSERT INTO raw_packets(id,timestamp,src,dst,protocol,payload,link_type) VALUES(?,?,?,?,?,?,?)",
				row.id, row.ts.Format("2006-01-02 15:04:05.999999999 -0700 MST"), "a", "b", "tcp", []byte{0}, 1); err != nil {
				t.Fatal(err)
			}
		}
		// 混入一行不可解析文本，验证迁移收敛（该行归零而非死循环）。
		if _, err := db.Exec("INSERT INTO raw_packets(id,timestamp,src,dst,protocol,payload,link_type) VALUES(?,?,?,?,?,?,?)",
			"bad1", "garbage", "a", "b", "tcp", []byte{0}, 1); err != nil {
			t.Fatal(err)
		}
	}()

	// 2) 以写入方身份打开：init 触发迁移。
	s, err := NewSQLiteStore(dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < rawTsUserVersion {
		t.Fatalf("user_version not bumped: %d", version)
	}

	check := func(db *sql.DB) {
		rows, err := db.Query("SELECT id, typeof(timestamp), timestamp FROM raw_packets")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		got := map[string]int64{}
		for rows.Next() {
			var id, typ string
			var ts int64
			if err := rows.Scan(&id, &typ, &ts); err != nil {
				t.Fatal(err)
			}
			if typ != "integer" {
				t.Fatalf("row %s still %s", id, typ)
			}
			got[id] = ts
		}
		if got["old1"] != legacyNow.UnixNano() {
			t.Fatalf("old1 = %d want %d", got["old1"], legacyNow.UnixNano())
		}
		if got["old2"] != legacyNow.Add(2*time.Second).UnixNano() {
			t.Fatalf("old2 wrong: %d", got["old2"])
		}
		if got["bad1"] != 0 {
			t.Fatalf("bad1 should be zeroed, got %d", got["bad1"])
		}
	}
	check(s.db)

	// 3) 幂等：重开不再改行（值不变、user_version 不回退）。
	s2, err := NewSQLiteStore(dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	check(s2.db)

	// 4) 迁移后读路径正常（QueryRawPackets 排序走数值比较，0 哨兵行在最前）。
	rows, err := s2.QueryRawPackets(context.Background(), RawPacketQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("want 4 rows, got %d", len(rows))
	}
	if !rows[0].Timestamp.IsZero() {
		t.Fatalf("zeroed bad1 should sort first, got %v", rows[0].Timestamp)
	}
	if !rows[1].Timestamp.Equal(legacyNow) {
		t.Fatalf("old1 should be second, got %v want %v", rows[1].Timestamp, legacyNow)
	}
}

// rawPacketsOnlySchema 仅建 raw_packets 表，模拟旧版本库结构。
const rawPacketsOnlySchema = `
CREATE TABLE IF NOT EXISTS raw_packets (
    id TEXT PRIMARY KEY,
    timestamp DATETIME,
    src TEXT,
    dst TEXT,
    protocol TEXT,
    payload BLOB,
    link_type INT,
    conn_id TEXT,
    metadata TEXT
);`

func TestMigrateRawTimestamps_EmptyDBNoop(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "empty.db")
	s, err := NewSQLiteStore(dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != rawTsUserVersion {
		t.Fatalf("empty db should be marked migrated, got %d", version)
	}
}

func TestQueryConnections_IntegerOrdering(t *testing.T) {
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "conn.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	base := time.Now().Truncate(time.Second)
	src := netip.MustParseAddrPort("127.0.0.1:1")
	dst := netip.MustParseAddrPort("127.0.0.1:2")
	mk := func(id, conn string, off time.Duration) event.Packet {
		return event.Packet{ID: id, Timestamp: base.Add(off), Src: src, Dst: dst, Protocol: "tcp", Metadata: map[string]any{"conn_id": conn}}
	}
	pkts := []event.Packet{
		mk("a1", "connA", 0),
		mk("b1", "connB", 10*time.Millisecond),
		mk("a2", "connA", 20*time.Millisecond),
		mk("b2", "connB", 30*time.Millisecond),
	}
	if err := s.AppendRawPackets(context.Background(), pkts); err != nil {
		t.Fatal(err)
	}

	conns, err := s.QueryConnections(context.Background(), "s1", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(conns) != 2 {
		t.Fatalf("want 2 conns, got %d", len(conns))
	}
	// 最新活跃连接在前（数值 MAX 比较）。
	if conns[0].ConnID != "connB" || conns[1].ConnID != "connA" {
		t.Fatalf("order wrong: %s then %s", conns[0].ConnID, conns[1].ConnID)
	}
	if !conns[1].StartTime.Equal(base) || !conns[1].EndTime.Equal(base.Add(20*time.Millisecond)) {
		t.Fatalf("connA time range wrong: %v ~ %v", conns[1].StartTime, conns[1].EndTime)
	}
	if conns[1].FrameCount != 2 {
		t.Fatalf("connA frames = %d", conns[1].FrameCount)
	}

	d, err := s.QueryConnectionDetail(context.Background(), "s1", "connA")
	if err != nil || d == nil {
		t.Fatalf("detail: %v %v", d, err)
	}
	if d.FrameCount != 2 || !d.StartTime.Equal(base) {
		t.Fatalf("detail wrong: frames=%d start=%v", d.FrameCount, d.StartTime)
	}

	frames, err := s.QueryConnectionFrames(context.Background(), "connA", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || !frames[0].Timestamp.Equal(base) || !frames[1].Timestamp.Equal(base.Add(20*time.Millisecond)) {
		t.Fatalf("frames wrong: %+v", frames)
	}
}
