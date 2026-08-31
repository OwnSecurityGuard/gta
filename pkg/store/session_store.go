package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// ControlStore 管理 control.sqlite，仅实现 SessionStore 接口。
// 用于会话生命周期元数据的持久化（非事件数据）。
type ControlStore struct {
	db *sql.DB
}

// NewControlStore 打开或创建 control.sqlite。
// controlPath 应为绝对路径，避免工作目录依赖。
func NewControlStore(controlPath string) (*ControlStore, error) {
	if abs, err := filepath.Abs(controlPath); err == nil {
		controlPath = abs
	}
	slog.Info("opening control store", "path", controlPath)
	db, err := sql.Open("sqlite", controlPath)
	if err != nil {
		return nil, err
	}
	cs := &ControlStore{db: db}
	if err := cs.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return cs, nil
}

func (cs *ControlStore) init() error {
	schema := `
CREATE TABLE IF NOT EXISTS sessions (
    session_id    TEXT PRIMARY KEY,
    started_at    DATETIME NOT NULL,
    stopped_at    DATETIME,
    status        TEXT NOT NULL,
    port          INTEGER,
    plugin        TEXT,
    interface     TEXT,
    pcap_file     TEXT,
    raw_packets   INTEGER DEFAULT 0,
    events        INTEGER DEFAULT 0,
    metrics       INTEGER DEFAULT 0,
    decode_errors INTEGER DEFAULT 0,
    duration_sec  REAL,
    db_path       TEXT NOT NULL,
    extra         TEXT,
    manifest_snapshot TEXT DEFAULT '',
    project_id      TEXT NOT NULL DEFAULT ''
);`
	if _, err := cs.db.Exec(schema); err != nil {
		return err
	}
	if err := cs.migrateSessionsAddOwner(); err != nil {
		return err
	}
	if err := cs.migrateSessionsAddProjectID(); err != nil {
		return err
	}
	// plugin_debug_access 审计表（设计 §6）：sample_bytes 等取证工具的访问留痕。
	// 仅追加，无 UPDATE/DELETE 路径；写入方唯一为 Runtime Plane（pipeline /
	// 内嵌的 Developer Plane），避免与 MCP 进程加剧 SQLite 锁竞争。
	auditSchema := `
CREATE TABLE IF NOT EXISTS plugin_debug_access (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    at                DATETIME NOT NULL,
    actor             TEXT,
    tool              TEXT,
    plugin            TEXT,
    session_id        TEXT,
    requested_packets INTEGER,
    returned_packets  INTEGER,
    returned_bytes    INTEGER,
    truncated         INTEGER
);`
	if _, err := cs.db.Exec(auditSchema); err != nil {
		return err
	}
	if _, err := cs.db.Exec("PRAGMA busy_timeout=5000;"); err != nil {
		return err
	}
	if _, err := cs.db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		return err
	}
	if _, err := cs.db.Exec("PRAGMA synchronous=NORMAL;"); err != nil {
		return err
	}
	return nil
}

// migrateSessionsAddProjectID 为既有数据库补齐 sessions.project_id 列（默认 ''）。
func (cs *ControlStore) migrateSessionsAddProjectID() error {
	if hasColumn(cs.db, "sessions", "project_id") {
		return nil
	}
	if _, err := cs.db.Exec(`ALTER TABLE sessions ADD COLUMN project_id TEXT NOT NULL DEFAULT ''`); err != nil {
		if hasColumn(cs.db, "sessions", "project_id") {
			slog.Info("sessions.project_id column added concurrently by another process; skipping migration")
			return nil
		}
		return fmt.Errorf("migrate sessions.project_id: %w", err)
	}
	slog.Info("migrated control store: added sessions.project_id column (backfilled '' = no project)")
	return nil
}

// migrateSessionsAddOwner 为既有数据库补齐 sessions.owner 列（默认 ''）。
// 老库回填 '' = 匿名：已有会话全部归属匿名 owner，本地单机用法行为不变。
func (cs *ControlStore) migrateSessionsAddOwner() error {
	if hasOwnerColumn(cs.db) {
		return nil
	}
	if _, err := cs.db.Exec(`ALTER TABLE sessions ADD COLUMN owner TEXT NOT NULL DEFAULT ''`); err != nil {
		// 迁移的 check-then-ALTER 不是原子的：两个进程并发打开同一老库时，
		// 后者会撞 "duplicate column name: owner"。列已在（对手刚加成功），
		// 复查 PRAGMA 确认后视为迁移完成而不是启动失败。
		if hasOwnerColumn(cs.db) {
			slog.Info("sessions.owner column added concurrently by another process; skipping migration")
			return nil
		}
		return fmt.Errorf("migrate sessions.owner: %w", err)
	}
	slog.Info("migrated control store: added sessions.owner column (backfilled '' = anonymous)")
	return nil
}

// hasOwnerColumn 复查 sessions 表是否已有 owner 列。
func hasOwnerColumn(db *sql.DB) bool {
	return hasColumn(db, "sessions", "owner")
}

// hasColumn 通过 PRAGMA table_info 判断某表是否已有指定列。
func hasColumn(db *sql.DB, table, col string) bool {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dfltValue any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); err != nil {
			return false
		}
		if name == col {
			return true
		}
	}
	return false
}

// Close 关闭数据库连接。
func (cs *ControlStore) Close() error {
	slog.Info("closing control store")
	return cs.db.Close()
}

// CreateSession 插入一条新会话元数据。
func (cs *ControlStore) CreateSession(ctx context.Context, meta SessionMeta) error {
	var extraJSON sql.NullString
	if len(meta.Extra) > 0 {
		b, err := json.Marshal(meta.Extra)
		if err != nil {
			return fmt.Errorf("marshal extra: %w", err)
		}
		extraJSON = sql.NullString{String: string(b), Valid: true}
	}
	var stoppedAt sql.NullTime
	if meta.StoppedAt != nil {
		stoppedAt = sql.NullTime{Time: *meta.StoppedAt, Valid: true}
	}
	_, err := cs.db.ExecContext(ctx, `
INSERT INTO sessions(owner, project_id, session_id, started_at, stopped_at, status, port, plugin, interface, pcap_file,
                     raw_packets, events, metrics, decode_errors, duration_sec, db_path, extra, manifest_snapshot)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		meta.Owner, meta.ProjectID, meta.SessionID, meta.StartedAt, stoppedAt, meta.Status, meta.Port, meta.Plugin,
		meta.Interface, meta.PCAPFile, meta.RawPackets, meta.Events, meta.Metrics,
		meta.DecodeErrors, meta.DurationSec, meta.DBPath, extraJSON, meta.ManifestSnapshot,
	)
	return err
}

// sessionSelectCols 是 sessions 查询的列清单（scanSession 与之配套）。
const sessionSelectCols = `owner, project_id, session_id, started_at, stopped_at, status, port, plugin, interface, pcap_file,
	raw_packets, events, metrics, decode_errors, duration_sec, db_path, extra, manifest_snapshot`

// GetSession 查询单个会话元数据（不过滤 owner，行为与引入 owner 前一致）。
func (cs *ControlStore) GetSession(ctx context.Context, sessionID string) (*SessionMeta, error) {
	return cs.GetSessionFor(ctx, sessionID, SessionOwnerFilter{AllOwners: true})
}

// GetSessionFor 按 owner 过滤地查询单个会话；不可见时按未找到处理。
func (cs *ControlStore) GetSessionFor(ctx context.Context, sessionID string, f SessionOwnerFilter) (*SessionMeta, error) {
	row := cs.db.QueryRowContext(ctx, `SELECT `+sessionSelectCols+` FROM sessions WHERE session_id=?`, sessionID)
	meta, err := scanSession(row)
	if err != nil {
		return nil, fmt.Errorf("get session %s: %w", sessionID, err)
	}
	if !f.Matches(*meta) {
		// 不可见按未找到处理，避免向非归属者泄露会话存在性。
		return nil, fmt.Errorf("get session %s: %w", sessionID, sql.ErrNoRows)
	}
	return meta, nil
}

// ListSessions 列出所有会话元数据（不过滤 owner），按 started_at 降序。
func (cs *ControlStore) ListSessions(ctx context.Context) ([]SessionMeta, error) {
	return cs.ListSessionsFor(ctx, SessionOwnerFilter{AllOwners: true})
}

// ListSessionsFor 按 owner 过滤地列出会话元数据，按 started_at 降序。
func (cs *ControlStore) ListSessionsFor(ctx context.Context, f SessionOwnerFilter) ([]SessionMeta, error) {
	query := `SELECT ` + sessionSelectCols + ` FROM sessions`
	args := []any{}
	if !f.AllOwners {
		query += ` WHERE owner=?`
		args = append(args, f.Owner)
	}
	query += ` ORDER BY started_at DESC`
	rows, err := cs.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SessionMeta
	for rows.Next() {
		meta, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *meta)
	}
	return result, rows.Err()
}

// ListSessionsForProject 列出某项目下的会话元数据，按 started_at 降序、按 owner 可见性过滤。
func (cs *ControlStore) ListSessionsForProject(ctx context.Context, projectID string, f SessionOwnerFilter) ([]SessionMeta, error) {
	query := `SELECT ` + sessionSelectCols + ` FROM sessions WHERE project_id=?`
	args := []any{projectID}
	if !f.AllOwners {
		query += ` AND owner=?`
		args = append(args, f.Owner)
	}
	query += ` ORDER BY started_at DESC`
	rows, err := cs.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SessionMeta
	for rows.Next() {
		meta, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *meta)
	}
	return result, rows.Err()
}

// UpdateSession 更新会话元数据（按 session_id 匹配）。
func (cs *ControlStore) UpdateSession(ctx context.Context, meta SessionMeta) error {
	var extraJSON sql.NullString
	if len(meta.Extra) > 0 {
		b, err := json.Marshal(meta.Extra)
		if err != nil {
			return fmt.Errorf("marshal extra: %w", err)
		}
		extraJSON = sql.NullString{String: string(b), Valid: true}
	}
	var stoppedAt sql.NullTime
	if meta.StoppedAt != nil {
		stoppedAt = sql.NullTime{Time: *meta.StoppedAt, Valid: true}
	}
	res, err := cs.db.ExecContext(ctx, `
UPDATE sessions SET started_at=?, project_id=?, stopped_at=?, status=?, port=?, plugin=?, interface=?, pcap_file=?,
                    raw_packets=?, events=?, metrics=?, decode_errors=?, duration_sec=?, db_path=?, extra=?,
                    manifest_snapshot=?
WHERE session_id=?`,
		meta.StartedAt, meta.ProjectID, stoppedAt, meta.Status, meta.Port, meta.Plugin,
		meta.Interface, meta.PCAPFile, meta.RawPackets, meta.Events, meta.Metrics,
		meta.DecodeErrors, meta.DurationSec, meta.DBPath, extraJSON,
		meta.ManifestSnapshot, meta.SessionID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("session %s not found", meta.SessionID)
	}
	return nil
}

// DeleteSession 删除会话元数据。
func (cs *ControlStore) DeleteSession(ctx context.Context, sessionID string) error {
	_, err := cs.db.ExecContext(ctx, "DELETE FROM sessions WHERE session_id=?", sessionID)
	return err
}

// ReconcileRunningSessions 将上一进程残留的 running 会话标记为 stopped。
//
// 用于进程启动兜底：若上一进程崩溃、被 SIGKILL、或断电等未走优雅退出路径，
// ControlStore 中会残留 status='running' 的会话，导致重启后前端持续显示"运行中"。
// 本方法仅更新 status 与 stopped_at，保留其余字段（含 started_at、统计、extra）。
// 返回受影响行数。配合 pipelineService.StopAll 的优雅退出，二者共同保证
// 会话状态最终一致：正常退出由 StopAll 主动置 stopped，异常退出由本兜底在下次启动时纠正。
func (cs *ControlStore) ReconcileRunningSessions(ctx context.Context, stoppedAt time.Time) (int64, error) {
	res, err := cs.db.ExecContext(ctx, `
UPDATE sessions SET status='stopped', stopped_at=? WHERE status='running'`, stoppedAt)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// sessionScanner 抽象 sql.Row 和 sql.Rows 的 Scan 方法。
type sessionScanner interface {
	Scan(dest ...any) error
}

func scanSession(s sessionScanner) (*SessionMeta, error) {
	var meta SessionMeta
	var stoppedAt sql.NullTime
	var extraJSON sql.NullString
	if err := s.Scan(
		&meta.Owner, &meta.ProjectID, &meta.SessionID, &meta.StartedAt, &stoppedAt, &meta.Status, &meta.Port, &meta.Plugin,
		&meta.Interface, &meta.PCAPFile, &meta.RawPackets, &meta.Events, &meta.Metrics,
		&meta.DecodeErrors, &meta.DurationSec, &meta.DBPath, &extraJSON,
		&meta.ManifestSnapshot,
	); err != nil {
		return nil, err
	}
	if stoppedAt.Valid {
		t := stoppedAt.Time
		meta.StoppedAt = &t
	}
	if extraJSON.Valid && extraJSON.String != "" {
		if err := json.Unmarshal([]byte(extraJSON.String), &meta.Extra); err != nil {
			slog.Warn("unmarshal session extra", "error", err)
		}
	}
	return &meta, nil
}

// 确保 ControlStore 实现 SessionStore 接口。
var _ SessionStore = (*ControlStore)(nil)

// 便于测试：返回底层 *sql.DB。
func (cs *ControlStore) DB() *sql.DB { return cs.db }
