package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// QueryMetrics 查询 aggregated_metrics 表。
func (s *SQLiteStore) QueryMetrics(ctx context.Context, q MetricQuery) ([]MetricRow, error) {
	query := "SELECT name, window, group_json, value FROM aggregated_metrics WHERE 1=1"
	var args []any
	if q.Name != "" {
		query += " AND name=?"
		args = append(args, q.Name)
	}
	query += " ORDER BY window ASC"
	query, args = applyLimitOffset(query, args, q.Limit, q.Offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query metrics: %w", err)
	}
	defer rows.Close()
	var result []MetricRow
	for rows.Next() {
		var r MetricRow
		var groupJSON string
		var windowNano int64
		if err := rows.Scan(&r.Name, &windowNano, &groupJSON, &r.Value); err != nil {
			return nil, err
		}
		r.Window = time.Unix(0, windowNano)
		if groupJSON != "" {
			if err := json.Unmarshal([]byte(groupJSON), &r.Group); err != nil {
				// 解析失败不阻断，Group 留空
				r.Group = nil
			}
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// QueryStateChanges 查询 state_changes 表。
func (s *SQLiteStore) QueryStateChanges(ctx context.Context, q StateChangeQuery) ([]StateChangeRow, error) {
	query := `SELECT id, event_id, session_id, flow_id, timestamp, subject_type, subject_id, op, path, before_value, after_value, version, metadata
FROM state_changes WHERE 1=1`
	var args []any
	if q.SessionID != "" {
		query += " AND session_id=?"
		args = append(args, q.SessionID)
	}
	if q.FlowID != "" {
		query += " AND flow_id=?"
		args = append(args, q.FlowID)
	}
	if q.SubjectType != "" {
		query += " AND subject_type=?"
		args = append(args, q.SubjectType)
	}
	if q.SubjectID != "" {
		query += " AND subject_id=?"
		args = append(args, q.SubjectID)
	}
	if q.Op != "" {
		query += " AND op=?"
		args = append(args, q.Op)
	}
	if q.Path != "" {
		query += " AND path=?"
		args = append(args, q.Path)
	}
	query += " ORDER BY timestamp ASC"
	query, args = applyLimitOffset(query, args, q.Limit, q.Offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query state changes: %w", err)
	}
	defer rows.Close()
	var result []StateChangeRow
	for rows.Next() {
		var r StateChangeRow
		var tsNano int64
		var flowID sql.NullString
		var beforeValue, afterValue, metadata sql.NullString
		if err := rows.Scan(&r.ID, &r.EventID, &r.SessionID, &flowID, &tsNano, &r.SubjectType, &r.SubjectID, &r.Op, &r.Path, &beforeValue, &afterValue, &r.Version, &metadata); err != nil {
			return nil, err
		}
		r.Timestamp = time.Unix(0, tsNano)
		if flowID.Valid {
			r.FlowID = flowID.String
		}
		if beforeValue.Valid {
			r.Before = beforeValue.String
		}
		if afterValue.Valid {
			r.After = afterValue.String
		}
		if metadata.Valid {
			r.Metadata = metadata.String
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// 编译期断言：SQLiteStore 实现 ProjectionReader。
var _ ProjectionReader = (*SQLiteStore)(nil)
