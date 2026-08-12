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
    link_type INT
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
    created_at INTEGER NOT NULL
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
    correlation_id TEXT,
    projection_json TEXT NOT NULL
);`
	if _, err := s.db.Exec(schemaText); err != nil {
		return err
	}
	// 迁移：为已存在的 raw_packets 表添加 link_type 列
	// 如果列已存在，ALTER TABLE 会失败，忽略错误
	_, _ = s.db.Exec("ALTER TABLE raw_packets ADD COLUMN link_type INT")

	// 迁移：为已存在的 events 表添加 context 列
	// X'80' 是空 msgpack map 的二进制表示（空 EventContext）
	_, _ = s.db.Exec("ALTER TABLE events ADD COLUMN context BLOB NOT NULL DEFAULT X'80'")

	// 迁移：为已存在的 state_changes 表添加 before_resolved / after_resolved 列
	_, _ = s.db.Exec("ALTER TABLE state_changes ADD COLUMN before_resolved BOOLEAN NOT NULL DEFAULT 0")
	_, _ = s.db.Exec("ALTER TABLE state_changes ADD COLUMN after_resolved BOOLEAN NOT NULL DEFAULT 0")

	// 证据图表：以 session 为粒度写入，session_id 已由 capture.sqlite 隐式确定。
	// 为支持 analysis_run 复现原则，analysis_run 字段预留但首阶段不建约束。
	evidenceGraphDDL := `
CREATE TABLE IF NOT EXISTS evidence_nodes (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    flow_id TEXT,
    analysis_run TEXT,
    timestamp INTEGER NOT NULL,
    labels TEXT,
    properties TEXT
);
CREATE TABLE IF NOT EXISTS evidence_edges (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    source TEXT NOT NULL,
    target TEXT NOT NULL,
    type TEXT NOT NULL,
    confidence REAL NOT NULL DEFAULT 1.0,
    reason TEXT,
    analysis_run TEXT,
    properties TEXT
);`
	if _, err := s.db.Exec(evidenceGraphDDL); err != nil {
		return err
	}

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
		"CREATE INDEX IF NOT EXISTS idx_event_index_correlation ON event_index(correlation_id)",
		// evidence_graph 索引
		"CREATE INDEX IF NOT EXISTS idx_evidence_nodes_session_kind ON evidence_nodes(session_id, kind)",
		"CREATE INDEX IF NOT EXISTS idx_evidence_nodes_flow ON evidence_nodes(session_id, flow_id)",
		"CREATE INDEX IF NOT EXISTS idx_evidence_edges_session ON evidence_edges(session_id)",
		"CREATE INDEX IF NOT EXISTS idx_evidence_edges_source ON evidence_edges(source)",
		"CREATE INDEX IF NOT EXISTS idx_evidence_edges_target ON evidence_edges(target)",
	}
	for _, idx := range indexes {
		if _, err := s.db.Exec(idx); err != nil {
			return err
		}
	}

	if _, err := s.db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		return err
	}
	if _, err := s.db.Exec("PRAGMA synchronous=NORMAL;"); err != nil {
		return err
	}
	return nil
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
func (s *SQLiteStore) AppendRawPackets(ctx context.Context, packets []event.Packet) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, "INSERT OR REPLACE INTO raw_packets(id,timestamp,src,dst,protocol,payload,link_type) VALUES(?,?,?,?,?,?,?)")
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
		if _, err := stmt.ExecContext(ctx, id, p.Timestamp, p.Src.String(), p.Dst.String(), p.Protocol, p.Raw, int32(p.LinkType)); err != nil {
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
