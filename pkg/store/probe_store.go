package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ProbeMeta 是探针的完整记录：注册信息 + 三维度运行状态快照。
// 快照由探针心跳刷新（pkg/probe Manager），供探针离线后 UI 仍能看到最后状态。
// 语义约定见 docs/plans/2026-09-05-probe-control-archive-design.md §5/§6.2。
type ProbeMeta struct {
	ProbeID     string
	Name        string // 机器名（默认 hostname，可改）
	Owner       string // 注册者；探针是个人资源，仅本人可用（creator-only）
	TenantID    string
	Capabilities string // csv: pcap,mobile,plugin_host
	TokenHash   string // SHA-256(probe_token)；revoke 后为空串
	Version     string
	Hostname    string
	OS          string
	Arch        string

	// 维度一：连接（online 判定 = 控制流存活；落库的是最后快照）
	ConnectionState string
	LastSeenAt      time.Time

	// 维度二：抓包状态机
	CaptureState  string // idle|starting|running|stopped|failed
	LastSessionID string
	StatusError   string
	CaptureIface  string
	CapturePorts  string // csv，展示用

	// 维度三：数据
	LastPacketMs    int64 // 0 = 从未抓到帧
	LastUploadMs    int64 // 0 = 从未成功推流（未被确认）
	PacketsCaptured uint64
	PacketsAcked    uint64
	SpoolDepth      uint64
	Dropped         uint64

	// 归档摘要（本地留存）
	ArchiveBytes    uint64
	ArchiveSegments uint32
	ArchiveOldestMs int64
	ArchiveNewestMs int64

	CreatedAt time.Time
}

// ProbeRuntimeStatus 是心跳携带的运行时快照（不含注册信息，Update 专用）。
type ProbeRuntimeStatus struct {
	ConnectionState string
	CaptureState    string
	LastSessionID   string
	StatusError     string
	CaptureIface    string
	CapturePorts    string
	LastPacketMs    int64
	LastUploadMs    int64
	PacketsCaptured uint64
	PacketsAcked    uint64
	SpoolDepth      uint64
	Dropped         uint64
	ArchiveBytes    uint64
	ArchiveSegments uint32
	ArchiveOldestMs int64
	ArchiveNewestMs int64
	LastSeenAt      time.Time
}

// ArchiveSegmentMeta 是探针本地归档段的摘要（服务端缓存，探针离线也能展示）。
type ArchiveSegmentMeta struct {
	SegID    string
	FirstMs  int64
	LastMs   int64
	Packets  uint64
	Bytes    uint64
	LinkType uint32
}

const probeCols = `probe_id, name, owner, tenant_id, capabilities, token_hash, version, hostname, os, arch,
connection_state, last_seen_at, capture_state, last_session_id, status_error, capture_iface, capture_ports,
last_packet_ms, last_upload_ms, packets_captured, packets_acked, spool_depth, dropped,
archive_bytes, archive_segments, archive_oldest_ms, archive_newest_ms, created_at`

const probeScanCols = `probe_id, name, owner, COALESCE(tenant_id,'default'), capabilities, token_hash, version, hostname, os, arch,
connection_state, last_seen_at, capture_state, last_session_id, status_error, capture_iface, capture_ports,
last_packet_ms, last_upload_ms, packets_captured, packets_acked, spool_depth, dropped,
archive_bytes, archive_segments, archive_oldest_ms, archive_newest_ms, created_at`

func scanProbe(row interface{ Scan(...any) error }) (*ProbeMeta, error) {
	var m ProbeMeta
	var lastSeen, created sql.NullTime
	err := row.Scan(&m.ProbeID, &m.Name, &m.Owner, &m.TenantID, &m.Capabilities, &m.TokenHash,
		&m.Version, &m.Hostname, &m.OS, &m.Arch,
		&m.ConnectionState, &lastSeen, &m.CaptureState, &m.LastSessionID, &m.StatusError, &m.CaptureIface, &m.CapturePorts,
		&m.LastPacketMs, &m.LastUploadMs, &m.PacketsCaptured, &m.PacketsAcked, &m.SpoolDepth, &m.Dropped,
		&m.ArchiveBytes, &m.ArchiveSegments, &m.ArchiveOldestMs, &m.ArchiveNewestMs, &created)
	if err != nil {
		return nil, err
	}
	m.LastSeenAt = lastSeen.Time
	if m.LastSeenAt.IsZero() {
		m.LastSeenAt = created.Time
	}
	m.CreatedAt = created.Time
	return &m, nil
}

// ensureProbesTable 建探针表（幂等）。挂进 ControlStore.init 的迁移链。
func (cs *ControlStore) ensureProbesTable() error {
	schema := `
CREATE TABLE IF NOT EXISTS probes (
    probe_id        TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    owner           TEXT NOT NULL,
    tenant_id       TEXT NOT NULL DEFAULT 'default',
    capabilities    TEXT NOT NULL DEFAULT '',
    token_hash      TEXT NOT NULL DEFAULT '',
    version         TEXT NOT NULL DEFAULT '',
    hostname        TEXT NOT NULL DEFAULT '',
    os              TEXT NOT NULL DEFAULT '',
    arch            TEXT NOT NULL DEFAULT '',
    connection_state TEXT NOT NULL DEFAULT '',
    last_seen_at    DATETIME,
    capture_state   TEXT NOT NULL DEFAULT '',
    last_session_id TEXT NOT NULL DEFAULT '',
    status_error    TEXT NOT NULL DEFAULT '',
    capture_iface   TEXT NOT NULL DEFAULT '',
    capture_ports   TEXT NOT NULL DEFAULT '',
    last_packet_ms  INTEGER NOT NULL DEFAULT 0,
    last_upload_ms  INTEGER NOT NULL DEFAULT 0,
    packets_captured INTEGER NOT NULL DEFAULT 0,
    packets_acked   INTEGER NOT NULL DEFAULT 0,
    spool_depth     INTEGER NOT NULL DEFAULT 0,
    dropped         INTEGER NOT NULL DEFAULT 0,
    archive_bytes   INTEGER NOT NULL DEFAULT 0,
    archive_segments INTEGER NOT NULL DEFAULT 0,
    archive_oldest_ms INTEGER NOT NULL DEFAULT 0,
    archive_newest_ms INTEGER NOT NULL DEFAULT 0,
    created_at      DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_probes_owner ON probes(owner);
CREATE TABLE IF NOT EXISTS probe_archive_segments (
    probe_id   TEXT NOT NULL,
    seg_id     TEXT NOT NULL,
    first_ms   INTEGER NOT NULL,
    last_ms    INTEGER NOT NULL,
    packets    INTEGER NOT NULL DEFAULT 0,
    bytes      INTEGER NOT NULL DEFAULT 0,
    link_type  INTEGER NOT NULL DEFAULT 0,
    updated_at DATETIME NOT NULL,
    PRIMARY KEY (probe_id, seg_id)
);`
	if _, err := cs.db.Exec(schema); err != nil {
		return fmt.Errorf("ensure probes schema: %w", err)
	}
	return nil
}

// UpsertProbe 注册或更新探针。ON CONFLICT 保留 created_at 与 owner（探针不许换主）；
// re-register 场景由调用方先 RevokeProbe 旧记录或直接覆盖 token_hash。
func (cs *ControlStore) UpsertProbe(ctx context.Context, m ProbeMeta) error {
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	if m.TenantID == "" {
		m.TenantID = normalizeTenant("")
	}
	_, err := cs.db.ExecContext(ctx, `
INSERT INTO probes(probe_id, name, owner, tenant_id, capabilities, token_hash, version, hostname, os, arch,
                   connection_state, last_seen_at, capture_state, created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,'idle',?)
ON CONFLICT(probe_id) DO UPDATE SET
  name=excluded.name, capabilities=excluded.capabilities, token_hash=excluded.token_hash,
  version=excluded.version, hostname=excluded.hostname, os=excluded.os, arch=excluded.arch`,
		m.ProbeID, m.Name, m.Owner, m.TenantID, m.Capabilities, m.TokenHash,
		m.Version, m.Hostname, m.OS, m.Arch,
		m.ConnectionState, m.LastSeenAt, m.CreatedAt)
	return err
}

// GetProbe 按 id 查询；不存在返回 sql.ErrNoRows。
func (cs *ControlStore) GetProbe(ctx context.Context, probeID string) (*ProbeMeta, error) {
	row := cs.db.QueryRowContext(ctx, `SELECT `+probeScanCols+` FROM probes WHERE probe_id=?`, probeID)
	m, err := scanProbe(row)
	if err != nil {
		return nil, fmt.Errorf("get probe %s: %w", probeID, err)
	}
	return m, nil
}

// GetProbeByTokenHash 按凭证哈希查询（auth resolver 用；token 明文不落库）。
func (cs *ControlStore) GetProbeByTokenHash(ctx context.Context, tokenHash string) (*ProbeMeta, error) {
	if tokenHash == "" {
		return nil, sql.ErrNoRows
	}
	row := cs.db.QueryRowContext(ctx, `SELECT `+probeScanCols+` FROM probes WHERE token_hash=?`, tokenHash)
	m, err := scanProbe(row)
	if err != nil {
		return nil, sql.ErrNoRows // 不泄露失败细节
	}
	return m, nil
}

// ListProbes 列出全部探针（鉴权过滤在调用方做 creator 轴判定）。
func (cs *ControlStore) ListProbes(ctx context.Context) ([]ProbeMeta, error) {
	rows, err := cs.db.QueryContext(ctx, `SELECT `+probeScanCols+` FROM probes ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProbeMeta
	for rows.Next() {
		m, err := scanProbe(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// UpdateProbeStatus 心跳落库：刷新三维度快照与 last_seen。
func (cs *ControlStore) UpdateProbeStatus(ctx context.Context, probeID string, st ProbeRuntimeStatus) error {
	_, err := cs.db.ExecContext(ctx, `
UPDATE probes SET connection_state=?, capture_state=?, last_session_id=?, status_error=?,
  capture_iface=?, capture_ports=?, last_packet_ms=?, last_upload_ms=?,
  packets_captured=?, packets_acked=?, spool_depth=?, dropped=?,
  archive_bytes=?, archive_segments=?, archive_oldest_ms=?, archive_newest_ms=?, last_seen_at=?
WHERE probe_id=?`,
		st.ConnectionState, st.CaptureState, st.LastSessionID, st.StatusError,
		st.CaptureIface, st.CapturePorts, st.LastPacketMs, st.LastUploadMs,
		st.PacketsCaptured, st.PacketsAcked, st.SpoolDepth, st.Dropped,
		st.ArchiveBytes, st.ArchiveSegments, st.ArchiveOldestMs, st.ArchiveNewestMs, st.LastSeenAt,
		probeID)
	return err
}

// SetProbeConnection 批量刷新连接态（探针断流时调用，快照字段不动）。
func (cs *ControlStore) SetProbeConnection(ctx context.Context, probeID, state string, seen time.Time) error {
	_, err := cs.db.ExecContext(ctx,
		`UPDATE probes SET connection_state=?, last_seen_at=? WHERE probe_id=?`, state, seen, probeID)
	return err
}

// RenameProbe 改机器名。
func (cs *ControlStore) RenameProbe(ctx context.Context, probeID, name string) error {
	_, err := cs.db.ExecContext(ctx, `UPDATE probes SET name=? WHERE probe_id=?`, name, probeID)
	return err
}

// RevokeProbe 作废凭证（token_hash 置空）：探针下次启动需重新接入。
func (cs *ControlStore) RevokeProbe(ctx context.Context, probeID string) error {
	_, err := cs.db.ExecContext(ctx,
		`UPDATE probes SET token_hash='', connection_state='offline' WHERE probe_id=?`, probeID)
	return err
}

// DeleteProbe 删除探针与其归档摘要缓存。
func (cs *ControlStore) DeleteProbe(ctx context.Context, probeID string) error {
	if _, err := cs.db.ExecContext(ctx, `DELETE FROM probe_archive_segments WHERE probe_id=?`, probeID); err != nil {
		return err
	}
	_, err := cs.db.ExecContext(ctx, `DELETE FROM probes WHERE probe_id=?`, probeID)
	return err
}

// ReplaceProbeSegments 整体替换某探针的归档段摘要缓存。
func (cs *ControlStore) ReplaceProbeSegments(ctx context.Context, probeID string, segs []ArchiveSegmentMeta) error {
	tx, err := cs.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM probe_archive_segments WHERE probe_id=?`, probeID); err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, s := range segs {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO probe_archive_segments(probe_id, seg_id, first_ms, last_ms, packets, bytes, link_type, updated_at)
VALUES (?,?,?,?,?,?,?,?)`,
			probeID, s.SegID, s.FirstMs, s.LastMs, s.Packets, s.Bytes, s.LinkType, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListProbeSegments 查询某探针与 [from,to] 有交集的归档段（毫秒时间戳）。
// from/to 为 0 表示不限。缓存为空不是错误（返回空切片）。
func (cs *ControlStore) ListProbeSegments(ctx context.Context, probeID string, fromMs, toMs int64) ([]ArchiveSegmentMeta, error) {
	q := `SELECT seg_id, first_ms, last_ms, packets, bytes, link_type
          FROM probe_archive_segments WHERE probe_id=?`
	args := []any{probeID}
	if fromMs > 0 {
		q += ` AND last_ms >= ?`
		args = append(args, fromMs)
	}
	if toMs > 0 {
		q += ` AND first_ms <= ?`
		args = append(args, toMs)
	}
	q += ` ORDER BY first_ms`
	rows, err := cs.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ArchiveSegmentMeta
	for rows.Next() {
		var s ArchiveSegmentMeta
		if err := rows.Scan(&s.SegID, &s.FirstMs, &s.LastMs, &s.Packets, &s.Bytes, &s.LinkType); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
