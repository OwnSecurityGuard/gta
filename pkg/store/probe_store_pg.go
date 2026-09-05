package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// probes 表的 PG 后端实现。与 SQLite 版语义一致（见 probe_store.go）：
// 差异仅占位符（$N）与无 LastInsertId 需求（本表不用自增主键）。

func (cs *PGControlStore) UpsertProbe(ctx context.Context, m ProbeMeta) error {
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	if m.TenantID == "" {
		m.TenantID = normalizeTenant("")
	}
	_, err := cs.db.ExecContext(ctx, `
INSERT INTO probes(probe_id, name, owner, tenant_id, capabilities, token_hash, version, hostname, os, arch,
                   connection_state, last_seen_at, capture_state, created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'idle',$13)
ON CONFLICT(probe_id) DO UPDATE SET
  name=excluded.name, capabilities=excluded.capabilities, token_hash=excluded.token_hash,
  version=excluded.version, hostname=excluded.hostname, os=excluded.os, arch=excluded.arch`,
		m.ProbeID, m.Name, m.Owner, m.TenantID, m.Capabilities, m.TokenHash,
		m.Version, m.Hostname, m.OS, m.Arch,
		m.ConnectionState, m.LastSeenAt, m.CreatedAt)
	return err
}

func (cs *PGControlStore) GetProbe(ctx context.Context, probeID string) (*ProbeMeta, error) {
	row := cs.db.QueryRowContext(ctx, `SELECT `+probeScanCols+` FROM probes WHERE probe_id=$1`, probeID)
	m, err := scanProbe(row)
	if err != nil {
		return nil, fmt.Errorf("get probe %s: %w", probeID, err)
	}
	return m, nil
}

func (cs *PGControlStore) GetProbeByTokenHash(ctx context.Context, tokenHash string) (*ProbeMeta, error) {
	if tokenHash == "" {
		return nil, sql.ErrNoRows
	}
	row := cs.db.QueryRowContext(ctx, `SELECT `+probeScanCols+` FROM probes WHERE token_hash=$1`, tokenHash)
	m, err := scanProbe(row)
	if err != nil {
		return nil, sql.ErrNoRows
	}
	return m, nil
}

func (cs *PGControlStore) ListProbes(ctx context.Context) ([]ProbeMeta, error) {
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

func (cs *PGControlStore) UpdateProbeStatus(ctx context.Context, probeID string, st ProbeRuntimeStatus) error {
	_, err := cs.db.ExecContext(ctx, `
UPDATE probes SET connection_state=$1, capture_state=$2, last_session_id=$3, status_error=$4,
  capture_iface=$5, capture_ports=$6, last_packet_ms=$7, last_upload_ms=$8,
  packets_captured=$9, packets_acked=$10, spool_depth=$11, dropped=$12,
  archive_bytes=$13, archive_segments=$14, archive_oldest_ms=$15, archive_newest_ms=$16, last_seen_at=$17
WHERE probe_id=$18`,
		st.ConnectionState, st.CaptureState, st.LastSessionID, st.StatusError,
		st.CaptureIface, st.CapturePorts, st.LastPacketMs, st.LastUploadMs,
		st.PacketsCaptured, st.PacketsAcked, st.SpoolDepth, st.Dropped,
		st.ArchiveBytes, st.ArchiveSegments, st.ArchiveOldestMs, st.ArchiveNewestMs, st.LastSeenAt,
		probeID)
	return err
}

func (cs *PGControlStore) SetProbeConnection(ctx context.Context, probeID, state string, seen time.Time) error {
	_, err := cs.db.ExecContext(ctx,
		`UPDATE probes SET connection_state=$1, last_seen_at=$2 WHERE probe_id=$3`, state, seen, probeID)
	return err
}

func (cs *PGControlStore) RenameProbe(ctx context.Context, probeID, name string) error {
	_, err := cs.db.ExecContext(ctx, `UPDATE probes SET name=$1 WHERE probe_id=$2`, name, probeID)
	return err
}

func (cs *PGControlStore) RevokeProbe(ctx context.Context, probeID string) error {
	_, err := cs.db.ExecContext(ctx,
		`UPDATE probes SET token_hash='', connection_state='offline' WHERE probe_id=$1`, probeID)
	return err
}

func (cs *PGControlStore) DeleteProbe(ctx context.Context, probeID string) error {
	if _, err := cs.db.ExecContext(ctx, `DELETE FROM probe_archive_segments WHERE probe_id=$1`, probeID); err != nil {
		return err
	}
	_, err := cs.db.ExecContext(ctx, `DELETE FROM probes WHERE probe_id=$1`, probeID)
	return err
}

func (cs *PGControlStore) ReplaceProbeSegments(ctx context.Context, probeID string, segs []ArchiveSegmentMeta) error {
	tx, err := cs.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM probe_archive_segments WHERE probe_id=$1`, probeID); err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, s := range segs {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO probe_archive_segments(probe_id, seg_id, first_ms, last_ms, packets, bytes, link_type, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			probeID, s.SegID, s.FirstMs, s.LastMs, s.Packets, s.Bytes, s.LinkType, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (cs *PGControlStore) ListProbeSegments(ctx context.Context, probeID string, fromMs, toMs int64) ([]ArchiveSegmentMeta, error) {
	q := `SELECT seg_id, first_ms, last_ms, packets, bytes, link_type
          FROM probe_archive_segments WHERE probe_id=$1`
	args := []any{probeID}
	if fromMs > 0 {
		args = append(args, fromMs)
		q += fmt.Sprintf(` AND last_ms >= $%d`, len(args))
	}
	if toMs > 0 {
		args = append(args, toMs)
		q += fmt.Sprintf(` AND first_ms <= $%d`, len(args))
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
