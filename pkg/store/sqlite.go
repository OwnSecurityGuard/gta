package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"

	"gta/pkg/event"
	"gta/pkg/schema"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db        *sql.DB
	schemaReg *schema.Registry
	readOnly  bool
	// traceCols 表示 events 表是否含 scenario_id/replay_id 列
	//（旧库无此列时读取侧用 NULL 占位，保持 Scan 列数一致）。
	traceCols bool
}

// DB 返回底层的 *sql.DB，供外部（如 MCP handler）执行查询。
// 主要用于测试场景注入，避免 Windows SQLite 文件锁问题。
func (s *SQLiteStore) DB() *sql.DB {
	return s.db
}

func NewSQLiteStore(path string, schemaReg *schema.Registry) (*SQLiteStore, error) {
	if schemaReg == nil {
		schemaReg = schema.NewRegistry()
	}
	slog.Info("opening sqlite store", "path", path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	s := &SQLiteStore{db: db, schemaReg: schemaReg}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// NewSQLiteStoreReadOnly 以只读方式打开一个已存在的会话库，用于 test_plugin 对运行中会话的解码采样。
// 与 NewSQLiteStore 的区别：
//   - 跳过全部 DDL（CREATE TABLE/ALTER/CREATE INDEX），这些表已由写入方（captureTask）建好；
//   - 仅设置 busy_timeout，使读取在 WAL 下与运行中 writer 安全并发（读不阻塞写、写不阻塞读）；
//   - 全程只发 SELECT，绝不写库，因此不会与 captureTask 的写操作冲突。
func NewSQLiteStoreReadOnly(path string, schemaReg *schema.Registry) (*SQLiteStore, error) {
	if schemaReg == nil {
		schemaReg = schema.NewRegistry()
	}
	slog.Info("opening sqlite store (read-only)", "path", path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	s := &SQLiteStore{db: db, schemaReg: schemaReg, readOnly: true}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) init() error {
	if s.readOnly {
		// 只读模式：表/索引已由写入方建好，无需 DDL；仅设 busy_timeout 以便与运行中 writer 安全并发读。
		if _, err := s.db.Exec("PRAGMA busy_timeout=5000;"); err != nil {
			return err
		}
		s.probeTraceCols()
		return nil
	}
	schemaText := `
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
);
CREATE TABLE IF NOT EXISTS aggregated_metrics (
    name TEXT,
    window INTEGER NOT NULL,
    group_json TEXT,
    value REAL,
    PRIMARY KEY (name, window, group_json)
);
-- 旧 decoded_events / entity_snapshots 表已在 Event 迁移后移除；保留 migrate.go 供历史库迁移。
CREATE TABLE IF NOT EXISTS events (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    type TEXT NOT NULL,
    schema_id TEXT NOT NULL,
    source TEXT NOT NULL,
    timestamp INTEGER NOT NULL,
    causation_id TEXT,
    correlation_id TEXT,
    origin_id TEXT,
    context BLOB NOT NULL,
    payload BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    scenario_id TEXT,
    replay_id TEXT
);
CREATE TABLE IF NOT EXISTS state_changes (
    id TEXT PRIMARY KEY,
    event_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    flow_id TEXT,
    timestamp INTEGER NOT NULL,
    subject_type TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    op TEXT NOT NULL,
    path TEXT NOT NULL,
    before_value TEXT,
    after_value TEXT,
    version INTEGER,
    before_resolved BOOLEAN NOT NULL DEFAULT 0,
    after_resolved BOOLEAN NOT NULL DEFAULT 0,
    metadata TEXT
);
CREATE TABLE IF NOT EXISTS event_index (
    event_id TEXT PRIMARY KEY REFERENCES events(id),
    session_id TEXT NOT NULL,
    type TEXT NOT NULL,
    timestamp INTEGER NOT NULL,
    flow_id TEXT,
    direction TEXT,
    conn_id TEXT,
    correlation_id TEXT,
    projection_json TEXT NOT NULL
);`
	if _, err := s.db.Exec(schemaText); err != nil {
		return err
	}
	// 迁移：为已存在的 raw_packets 表添加 link_type 列
	// 如果列已存在，ALTER TABLE 会失败，忽略错误
	_, _ = s.db.Exec("ALTER TABLE raw_packets ADD COLUMN link_type INT")
	// 迁移：为已存在的 raw_packets 表添加 conn_id / metadata 列（代理抓包连接元数据）
	_, _ = s.db.Exec("ALTER TABLE raw_packets ADD COLUMN conn_id TEXT")
	_, _ = s.db.Exec("ALTER TABLE raw_packets ADD COLUMN metadata TEXT")
	// 迁移：为已存在的 event_index 表添加 conn_id 列（连接聚合索引）
	_, _ = s.db.Exec("ALTER TABLE event_index ADD COLUMN conn_id TEXT")

	// 迁移：为已存在的 events 表添加 context 列
	// X'80' 是空 msgpack map 的二进制表示（空 EventContext）
	_, _ = s.db.Exec("ALTER TABLE events ADD COLUMN context BLOB NOT NULL DEFAULT X'80'")

	// 迁移：为已存在的 state_changes 表添加 before_resolved / after_resolved 列
	_, _ = s.db.Exec("ALTER TABLE state_changes ADD COLUMN before_resolved BOOLEAN NOT NULL DEFAULT 0")
	_, _ = s.db.Exec("ALTER TABLE state_changes ADD COLUMN after_resolved BOOLEAN NOT NULL DEFAULT 0")

	// 迁移：为已存在的 events 表添加 scenario_id / replay_id 列（Scenario/Replay 前向兼容）
	_, _ = s.db.Exec("ALTER TABLE events ADD COLUMN scenario_id TEXT")
	_, _ = s.db.Exec("ALTER TABLE events ADD COLUMN replay_id TEXT")

	// 索引：支持高效查询
	indexes := []string{
		// Event 模型索引
		"CREATE INDEX IF NOT EXISTS idx_events_time ON events(timestamp)",
		"CREATE INDEX IF NOT EXISTS idx_events_session ON events(session_id)",
		"CREATE INDEX IF NOT EXISTS idx_events_type ON events(type)",
		"CREATE INDEX IF NOT EXISTS idx_events_correlation ON events(correlation_id)",
		"CREATE INDEX IF NOT EXISTS idx_events_origin ON events(origin_id)",
		"CREATE INDEX IF NOT EXISTS idx_events_causation ON events(causation_id)",
		// state_changes 索引
		"CREATE INDEX IF NOT EXISTS idx_state_changes_subject ON state_changes(session_id, flow_id, subject_type, subject_id, timestamp)",
		"CREATE INDEX IF NOT EXISTS idx_state_changes_event ON state_changes(event_id)",
		// event_index 索引
		"CREATE INDEX IF NOT EXISTS idx_event_index_session_time ON event_index(session_id, timestamp)",
		"CREATE INDEX IF NOT EXISTS idx_event_index_flow ON event_index(session_id, flow_id, timestamp)",
		"CREATE INDEX IF NOT EXISTS idx_event_index_conn ON event_index(session_id, conn_id, timestamp)",
		"CREATE INDEX IF NOT EXISTS idx_event_index_correlation ON event_index(correlation_id)",
		// raw_packets 连接索引（代理抓包按 conn_id 聚合）
		"CREATE INDEX IF NOT EXISTS idx_raw_packets_conn ON raw_packets(conn_id, timestamp)",
	}
	for _, idx := range indexes {
		if _, err := s.db.Exec(idx); err != nil {
			return err
		}
	}

	// WAL + busy_timeout：同机多进程（pipeline 写 + mcp 经 ControlStore / 只读
	// 会话库并发读）下读不阻塞写、写不阻塞读；busy_timeout 让偶发锁冲突等待
	// 5s 而非立即报 SQLITE_BUSY（写侧与只读侧同一保障，见 NewSQLiteStoreReadOnly）。
	if _, err := s.db.Exec("PRAGMA busy_timeout=5000;"); err != nil {
		return err
	}
	if _, err := s.db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		return err
	}
	if _, err := s.db.Exec("PRAGMA synchronous=NORMAL;"); err != nil {
		return err
	}
	s.probeTraceCols()
	return nil
}

// probeTraceCols 探测 events 表是否含 scenario_id/replay_id 列。
// 旧库无此列时读取侧用 NULL 占位，保证查询列数一致。
func (s *SQLiteStore) probeTraceCols() {
	rows, err := s.db.Query("PRAGMA table_info(events)")
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return
		}
		if name == "scenario_id" {
			s.traceCols = true
			return
		}
	}
}

// ClearDecodedData 清空 events、state_changes 和 event_index 表（保留 raw_packets）。
// 用于离线解码前清空旧的解码结果。capture.sqlite 是单 session 库，故整表清空。
// aggregated_metrics 表暂不清空（旧指标可能仍有参考价值，由调用方决定是否处理）。
func (s *SQLiteStore) ClearDecodedData(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM events"); err != nil {
		return fmt.Errorf("clear events: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM state_changes"); err != nil {
		return fmt.Errorf("clear state_changes: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM event_index"); err != nil {
		return fmt.Errorf("clear event_index: %w", err)
	}
	return tx.Commit()
}

// AppendRawPackets 追加原始数据包到 raw_packets 表（实现 EventWriter）。
// 代理抓包场景（conn_id / metadata）在此持久化，供 Connections 页面按连接聚合。
func (s *SQLiteStore) AppendRawPackets(ctx context.Context, packets []event.Packet) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, "INSERT OR REPLACE INTO raw_packets(id,timestamp,src,dst,protocol,payload,link_type,conn_id,metadata) VALUES(?,?,?,?,?,?,?,?,?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, p := range packets {
		// 若 Packet 已携带稳定 ID（如 capture_task 分配的 raw_packet_id），优先使用；否则新建 UUID。
		id := p.ID
		if id == "" {
			id = uuid.NewString()
		}
		// conn_id：移动代理在 Packet.Metadata 携带，回填到 raw_packets 供连接聚合。
		var connID sql.NullString
		if c, ok := p.Metadata["conn_id"].(string); ok && c != "" {
			connID = sql.NullString{String: c, Valid: true}
		}
		// metadata：整包元数据以 JSON 落库（client_addr/server_addr/source/device/app 等）。
		var metaJSON sql.NullString
		if len(p.Metadata) > 0 {
			if b, err := json.Marshal(p.Metadata); err == nil {
				metaJSON = sql.NullString{String: string(b), Valid: true}
			}
		}
		if _, err := stmt.ExecContext(ctx, id, p.Timestamp, p.Src.String(), p.Dst.String(), p.Protocol, p.Raw, int32(p.LinkType), connID, metaJSON); err != nil {
			return err
		}
	}
	slog.Debug("appended raw packets", "count", len(packets))
	return tx.Commit()
}

func (s *SQLiteStore) WriteMetrics(ctx context.Context, metrics []event.Metric) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, "INSERT OR REPLACE INTO aggregated_metrics(name,window,group_json,value) VALUES(?,?,?,?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, m := range metrics {
		gj, err := json.Marshal(m.Group)
		if err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, m.Name, m.Window.UnixNano(), string(gj), m.Value); err != nil {
			return err
		}
	}
	slog.Debug("wrote metrics", "count", len(metrics))
	return tx.Commit()
}

func (s *SQLiteStore) Flush() error { return nil }
func (s *SQLiteStore) Close() error {
	slog.Info("closing sqlite store")
	return s.db.Close()
}

// 编译期断言：SQLiteStore 实现 EventWriter 和 ProjectionWriter。
var _ EventWriter = (*SQLiteStore)(nil)
var _ ProjectionWriter = (*SQLiteStore)(nil)
