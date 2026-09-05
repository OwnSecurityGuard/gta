package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// PGControlStore 是 PostgreSQL 后端下的控制元数据存储，满足 ControlStoreBackend 接口。
//
// 与 SQLiteControlStore 的差异：
//   - 一个 PG 库承载全部会话的 sessions / plugin_debug_access；
//   - 占位符用 $N；plugin_debug_access.id 为 IDENTITY，RETURNING 取回（pgx 不支持 LastInsertId）；
//   - started_at/stopped_at 用 TIMESTAMP（driver 直接处理 time.Time），无 AUTOINCREMENT / 无 ALTER 迁移；
//   - 共享连接池按 DSN 缓存，Close 为 no-op。
type PGControlStore struct {
	db *sql.DB
}

// Close 为 no-op：PG 连接池在进程内按 DSN 共享缓存，不应随单个控制库关闭。
func (cs *PGControlStore) Close() error { return nil }

// DB 返回底层共享连接池。
func (cs *PGControlStore) DB() *sql.DB { return cs.db }

// CreateSession 插入一条新会话元数据。
func (cs *PGControlStore) CreateSession(ctx context.Context, meta SessionMeta) error {
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
INSERT INTO sessions(owner, tenant_id, project_id, session_id, started_at, stopped_at, status, port, plugin, interface, pcap_file,
                     raw_packets, events, metrics, decode_errors, duration_sec, db_path, extra, manifest_snapshot)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
		meta.Owner, normalizeTenant(meta.TenantID), meta.ProjectID, meta.SessionID, meta.StartedAt, stoppedAt, meta.Status, meta.Port, meta.Plugin,
		meta.Interface, meta.PCAPFile, meta.RawPackets, meta.Events, meta.Metrics,
		meta.DecodeErrors, meta.DurationSec, meta.DBPath, extraJSON, meta.ManifestSnapshot,
	)
	return err
}

// GetSession 查询单个会话元数据（不过滤 owner，行为与引入 owner 前一致）。
func (cs *PGControlStore) GetSession(ctx context.Context, sessionID string) (*SessionMeta, error) {
	return cs.GetSessionFor(ctx, sessionID, SessionOwnerFilter{AllOwners: true})
}

// GetSessionFor 按 owner 过滤地查询单个会话；不可见时按未找到处理。
func (cs *PGControlStore) GetSessionFor(ctx context.Context, sessionID string, f SessionOwnerFilter) (*SessionMeta, error) {
	row := cs.db.QueryRowContext(ctx, `SELECT `+sessionSelectCols+` FROM sessions WHERE session_id=$1`, sessionID)
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
func (cs *PGControlStore) ListSessions(ctx context.Context) ([]SessionMeta, error) {
	return cs.ListSessionsFor(ctx, SessionOwnerFilter{AllOwners: true})
}

// ListSessionsFor 按可见性过滤地列出会话元数据，按 started_at 降序。
// 可见性 = owner 匹配 OR 归属 ProjectIDs 中的项目（与 SQLite 版一致）。
func (cs *PGControlStore) ListSessionsFor(ctx context.Context, f SessionOwnerFilter) ([]SessionMeta, error) {
	query := `SELECT ` + sessionSelectCols + ` FROM sessions`
	args := []any{}
	if !f.AllOwners {
		query += ` WHERE (owner=$1`
		args = append(args, f.Owner)
		for _, id := range f.ProjectIDs {
			query += fmt.Sprintf(` OR project_id=$%d`, len(args)+1)
			args = append(args, id)
		}
		query += `)`
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

// ListSessionsForProject 列出某项目下的全部会话元数据，按 started_at 降序。
// 不做 owner 过滤：项目是协作边界，调用方必须先做 ActionProjectRead 鉴权。
func (cs *PGControlStore) ListSessionsForProject(ctx context.Context, projectID string, _ SessionOwnerFilter) ([]SessionMeta, error) {
	query := `SELECT ` + sessionSelectCols + ` FROM sessions WHERE project_id=$1`
	args := []any{projectID}
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
func (cs *PGControlStore) UpdateSession(ctx context.Context, meta SessionMeta) error {
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
UPDATE sessions SET started_at=$1, tenant_id=$2, project_id=$3, stopped_at=$4, status=$5, port=$6, plugin=$7, interface=$8, pcap_file=$9,
                    raw_packets=$10, events=$11, metrics=$12, decode_errors=$13, duration_sec=$14, db_path=$15, extra=$16,
                    manifest_snapshot=$17
WHERE session_id=$18`,
		meta.StartedAt, normalizeTenant(meta.TenantID), meta.ProjectID, stoppedAt, meta.Status, meta.Port, meta.Plugin,
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
func (cs *PGControlStore) DeleteSession(ctx context.Context, sessionID string) error {
	_, err := cs.db.ExecContext(ctx, "DELETE FROM sessions WHERE session_id=$1", sessionID)
	return err
}

// ReconcileRunningSessions 将上一进程残留的 running 会话标记为 stopped。
func (cs *PGControlStore) ReconcileRunningSessions(ctx context.Context, stoppedAt time.Time) (int64, error) {
	res, err := cs.db.ExecContext(ctx, `
UPDATE sessions SET status='stopped', stopped_at=$1 WHERE status='running'`, stoppedAt)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetSessionProject 将会话绑定到某项目（或传空串清空绑定）。
//
// Deprecated: 无鉴权裸更新，仅限已鉴权路径内部使用；新代码走 MoveSessionToProject。
func (cs *PGControlStore) SetSessionProject(ctx context.Context, sessionID, projectID string) error {
	res, err := cs.db.ExecContext(ctx,
		`UPDATE sessions SET project_id=$1 WHERE session_id=$2`, projectID, sessionID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("session %s not found", sessionID)
	}
	return nil
}

// MoveSessionToProject 是 move_session_to_project 的原子落点（带租户 CAS，与 SQLite 版一致）。
func (cs *PGControlStore) MoveSessionToProject(ctx context.Context, sessionID, projectID, expectTenant string) error {
	res, err := cs.db.ExecContext(ctx,
		`UPDATE sessions SET project_id=$1 WHERE session_id=$2 AND tenant_id=$3`,
		projectID, sessionID, expectTenant)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("session %s not found or tenant changed", sessionID)
	}
	return nil
}

// RecordDebugAccess 追加一条 plugin_debug_access 审计行（PG 用 RETURNING id 取回主键）。
func (cs *PGControlStore) RecordDebugAccess(ctx context.Context, d DebugAccess) (int64, error) {
	if d.At.IsZero() {
		d.At = time.Now()
	}
	var id int64
	err := cs.db.QueryRowContext(ctx, `
INSERT INTO plugin_debug_access
    (at, actor, tool, plugin, session_id, requested_packets, returned_packets, returned_bytes, truncated)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		d.At, d.Actor, d.Tool, d.Plugin, d.SessionID,
		d.RequestedPackets, d.ReturnedPackets, d.ReturnedBytes, boolToInt(d.Truncated),
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// DebugAccesses 返回某会话的审计行（最新在前）。
func (cs *PGControlStore) DebugAccesses(ctx context.Context, sessionID string) ([]DebugAccess, error) {
	rows, err := cs.db.QueryContext(ctx, `
SELECT id, at, actor, tool, plugin, session_id, requested_packets, returned_packets, returned_bytes, truncated
FROM plugin_debug_access
WHERE session_id=$1
ORDER BY at DESC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DebugAccess
	for rows.Next() {
		var d DebugAccess
		var truncated int
		if err := rows.Scan(&d.ID, &d.At, &d.Actor, &d.Tool, &d.Plugin,
			&d.SessionID, &d.RequestedPackets, &d.ReturnedPackets, &d.ReturnedBytes, &truncated); err != nil {
			return nil, err
		}
		d.Truncated = truncated != 0
		out = append(out, d)
	}
	return out, rows.Err()
}
