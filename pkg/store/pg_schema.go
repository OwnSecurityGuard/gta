package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

// PostgreSQL DDL（事件数据 + 控制元数据，同库多 session）。
//
// 与 SQLite 版对齐，方言差异：
//   - raw_packets 增加 session_id 列：PG 下一个库承载全部 session，必须用 session_id 隔离；
//     SQLite 模式仍是每 session 一个文件，raw_packets 无 session_id（兼容既有库）。
//   - 时间统一用 BIGINT 存 unix nano（与 SQLite 实际存储一致），避开 TIMESTAMP 时区/解析方言；
//     仅 sessions 表的 started_at/stopped_at 用 TIMESTAMP（控制元数据，driver 直接处理 time.Time）。
//   - AUTOINCREMENT → GENERATED ALWAYS AS IDENTITY。
//   - UPSERT 由各写入方法的 ON CONFLICT ... DO UPDATE 处理（见 pg_store.go / pg_control_store.go）。
//   - 无 PRAGMA / 无 ALTER 迁移：PG 库为全新创建，schema 一次建好。

const pgEventSchema = `
CREATE TABLE IF NOT EXISTS raw_packets (
    id          TEXT PRIMARY KEY,
    session_id  TEXT NOT NULL,
    timestamp   BIGINT,
    src         TEXT,
    dst         TEXT,
    protocol    TEXT,
    payload     BYTEA,
    link_type   INT,
    conn_id     TEXT,
    metadata    TEXT
);
CREATE TABLE IF NOT EXISTS events (
    id          TEXT PRIMARY KEY,
    session_id  TEXT NOT NULL,
    type        TEXT NOT NULL,
    schema_id   TEXT NOT NULL,
    source      TEXT NOT NULL,
    timestamp   BIGINT NOT NULL,
    causation_id TEXT,
    correlation_id TEXT,
    origin_id   TEXT,
    context     BYTEA NOT NULL,
    payload     BYTEA NOT NULL,
    created_at  BIGINT NOT NULL,
    scenario_id TEXT,
    replay_id   TEXT
);
CREATE TABLE IF NOT EXISTS state_changes (
    id            TEXT PRIMARY KEY,
    event_id      TEXT NOT NULL,
    session_id    TEXT NOT NULL,
    flow_id       TEXT,
    timestamp     BIGINT NOT NULL,
    subject_type  TEXT NOT NULL,
    subject_id    TEXT NOT NULL,
    op            TEXT NOT NULL,
    path          TEXT NOT NULL,
    before_value  TEXT,
    after_value   TEXT,
    version       BIGINT,
    before_resolved BOOLEAN NOT NULL DEFAULT FALSE,
    after_resolved   BOOLEAN NOT NULL DEFAULT FALSE,
    metadata      TEXT
);
CREATE TABLE IF NOT EXISTS event_index (
    event_id       TEXT PRIMARY KEY REFERENCES events(id),
    session_id     TEXT NOT NULL,
    type           TEXT NOT NULL,
    timestamp      BIGINT NOT NULL,
    flow_id        TEXT,
    direction      TEXT,
    conn_id        TEXT,
    correlation_id TEXT,
    projection_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS aggregated_metrics (
    session_id TEXT NOT NULL,
    name       TEXT,
    window     BIGINT NOT NULL,
    group_json TEXT,
    value      DOUBLE PRECISION,
    PRIMARY KEY (session_id, name, window, group_json)
);`

// pgEventIndexes 事件表索引（CREATE INDEX IF NOT EXISTS 在 PG 同样幂等）。
const pgEventIndexes = `
CREATE INDEX IF NOT EXISTS idx_events_time ON events(timestamp);
CREATE INDEX IF NOT EXISTS idx_events_session ON events(session_id);
CREATE INDEX IF NOT EXISTS idx_events_session_time ON events(session_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_events_type ON events(type);
CREATE INDEX IF NOT EXISTS idx_events_correlation ON events(correlation_id);
CREATE INDEX IF NOT EXISTS idx_events_origin ON events(origin_id);
CREATE INDEX IF NOT EXISTS idx_events_causation ON events(causation_id);
CREATE INDEX IF NOT EXISTS idx_state_changes_subject ON state_changes(session_id, flow_id, subject_type, subject_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_state_changes_event ON state_changes(event_id);
CREATE INDEX IF NOT EXISTS idx_event_index_session_time ON event_index(session_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_event_index_flow ON event_index(session_id, flow_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_event_index_conn ON event_index(session_id, conn_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_event_index_correlation ON event_index(correlation_id);
CREATE INDEX IF NOT EXISTS idx_raw_packets_session ON raw_packets(session_id);
CREATE INDEX IF NOT EXISTS idx_raw_packets_conn ON raw_packets(conn_id, timestamp);`

const pgControlSchema = `
CREATE TABLE IF NOT EXISTS sessions (
    session_id       TEXT PRIMARY KEY,
    started_at       TIMESTAMP NOT NULL,
    stopped_at       TIMESTAMP,
    status           TEXT NOT NULL,
    port             INTEGER,
    plugin           TEXT,
    interface        TEXT,
    pcap_file        TEXT,
    raw_packets      BIGINT DEFAULT 0,
    events           BIGINT DEFAULT 0,
    metrics          BIGINT DEFAULT 0,
    decode_errors    BIGINT DEFAULT 0,
    duration_sec     DOUBLE PRECISION,
    db_path          TEXT NOT NULL,
    extra            TEXT,
    manifest_snapshot TEXT DEFAULT '',
    project_id       TEXT NOT NULL DEFAULT '',
    owner            TEXT NOT NULL DEFAULT '',
    tenant_id        TEXT NOT NULL DEFAULT 'default'
);
CREATE TABLE IF NOT EXISTS plugin_debug_access (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at                TIMESTAMP NOT NULL,
    actor             TEXT,
    tool              TEXT,
    plugin            TEXT,
    session_id        TEXT,
    requested_packets INTEGER,
    returned_packets  INTEGER,
    returned_bytes    INTEGER,
    truncated         INTEGER
);`

// pgControlIndexes 控制元数据表索引。
const pgControlIndexes = `
CREATE INDEX IF NOT EXISTS idx_sessions_owner ON sessions(owner);
CREATE INDEX IF NOT EXISTS idx_sessions_project ON sessions(project_id);
CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status);`

// InitPGEventSchema 在一个 PG 库里创建全部事件相关表 + 索引（幂等）。
// PG 模式下所有 session 共享同一库，故每次服务启动只建一次。
func InitPGEventSchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, pgEventSchema); err != nil {
		return fmt.Errorf("init pg event schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, pgEventIndexes); err != nil {
		return fmt.Errorf("init pg event indexes: %w", err)
	}
	return nil
}

// InitPGControlSchema 创建 sessions + plugin_debug_access 表（幂等）。
// tenant_id 用 ADD COLUMN IF NOT EXISTS 兜底：CREATE TABLE IF NOT EXISTS 不会
// 为既有老库补列（2026-09-05 方案 D4 落字段）。
func InitPGControlSchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, pgControlSchema); err != nil {
		return fmt.Errorf("init pg control schema: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE sessions ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default'`); err != nil {
		return fmt.Errorf("add sessions.tenant_id: %w", err)
	}
	if _, err := db.ExecContext(ctx, pgControlIndexes); err != nil {
		return fmt.Errorf("init pg control indexes: %w", err)
	}
	return nil
}

// ensurePGSchema 同时初始化事件与控制元数据 schema（一个 PG 库同时承载两者）。
func ensurePGSchema(ctx context.Context, db *sql.DB) error {
	if err := InitPGEventSchema(ctx, db); err != nil {
		return err
	}
	if err := InitPGControlSchema(ctx, db); err != nil {
		return err
	}
	slog.Info("postgresql schema initialized")
	return nil
}
