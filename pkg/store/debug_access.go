package store

import (
	"context"
	"time"
)

// DebugAccess is one row of the plugin_debug_access audit trail (设计 §6).
// It records that a forensic tool (currently sample_bytes) was invoked against
// a session, and how much data it actually returned — never the requested
// amount, so truncation cannot produce false audit figures.
type DebugAccess struct {
	ID               int64
	At               time.Time
	Actor            string // who triggered: "mcp" | "pipeline" | "plugin-dev"
	Tool             string // e.g. "sample_bytes"
	Plugin           string
	SessionID        string
	RequestedPackets int64
	ReturnedPackets  int64
	ReturnedBytes    int64
	Truncated        bool
}

// RecordDebugAccess appends a single audit row. It is append-only by design
// (no UPDATE/DELETE exists elsewhere); the returned id is the row's primary key.
func (cs *ControlStore) RecordDebugAccess(ctx context.Context, d DebugAccess) (int64, error) {
	if d.At.IsZero() {
		d.At = time.Now()
	}
	res, err := cs.db.ExecContext(ctx, `
INSERT INTO plugin_debug_access
    (at, actor, tool, plugin, session_id, requested_packets, returned_packets, returned_bytes, truncated)
VALUES (?,?,?,?,?,?,?,?,?)`,
		d.At, d.Actor, d.Tool, d.Plugin, d.SessionID,
		d.RequestedPackets, d.ReturnedPackets, d.ReturnedBytes, boolToInt(d.Truncated),
	)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// DebugAccesses returns the audit rows for a session, most recent first.
func (cs *ControlStore) DebugAccesses(ctx context.Context, sessionID string) ([]DebugAccess, error) {
	rows, err := cs.db.QueryContext(ctx, `
SELECT id, at, actor, tool, plugin, session_id, requested_packets, returned_packets, returned_bytes, truncated
FROM plugin_debug_access
WHERE session_id=?
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

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
