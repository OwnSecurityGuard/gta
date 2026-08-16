package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"gta/pkg/event"
)

// eventScanner 抽象 *sql.Rows 与 *sql.Row，避免 Event scan 逻辑重复。
type eventScanner interface {
	Scan(dest ...any) error
}

// scanEvent 从一行查询结果构建 Event。
func scanEvent(sc eventScanner) (*event.Event, error) {
	var (
		id            string
		sessionID     string
		eventType     string
		schemaID      string
		source        string
		timestamp     int64
		causationID   sql.NullString
		correlationID sql.NullString
		originID      sql.NullString
		contextBytes  []byte
		payloadBytes  []byte
	)

	if err := sc.Scan(
		&id, &sessionID, &eventType, &schemaID, &source, &timestamp,
		&causationID, &correlationID, &originID, &contextBytes, &payloadBytes,
	); err != nil {
		return nil, fmt.Errorf("scan event: %w", err)
	}

	payloadValue, err := event.UnmarshalValueMsgpack(payloadBytes)
	if err != nil {
		return nil, fmt.Errorf("unmarshal payload: %w", err)
	}

	ctx, err := event.UnmarshalContextMsgpack(contextBytes)
	if err != nil {
		// 兼容旧数据：context 列可能为空（旧行 DEFAULT X'80'）
		ctx = event.EventContext{}
	}

	e := &event.Event{
		Identity: event.Identity{
			ID:        event.EventID(id),
			SessionID: sessionID,
			Type:      event.EventType(eventType),
			SchemaID:  schemaID,
			Source:    event.SourceID(source),
			Timestamp: time.Unix(0, timestamp),
		},
		Relation: event.Relation{},
		Context:  ctx,
		Payload: event.Payload{
			SchemaID: schemaID,
			Value:    payloadValue,
		},
	}

	if causationID.Valid {
		e.Relation.CausationID = event.EventID(causationID.String)
	}
	if correlationID.Valid {
		e.Relation.CorrelationID = correlationID.String
	}
	if originID.Valid {
		e.Relation.OriginID = event.EventID(originID.String)
	}

	return e, nil
}

// QueryEvents 查询 Event。
func (s *SQLiteStore) QueryEvents(ctx context.Context, sessionID string, limit, offset int) ([]*event.Event, error) {
	query := `
		SELECT id, session_id, type, schema_id, source, timestamp,
		       causation_id, correlation_id, origin_id, context, payload
		FROM events
		WHERE session_id = ?
		ORDER BY timestamp DESC
	`
	// limit<=0 视为"无限制"（与 applyLimitOffset 语义一致），
	// 避免调用方传 limit=0 时生成 LIMIT 0 导致返回 0 行。
	query, args := applyLimitOffset(query, []any{sessionID}, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	var events []*event.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}

	return events, nil
}

// GetEventByID 根据 ID 查询单个 Event。
func (s *SQLiteStore) GetEventByID(ctx context.Context, id string) (*event.Event, error) {
	query := `
		SELECT id, session_id, type, schema_id, source, timestamp,
		       causation_id, correlation_id, origin_id, context, payload
		FROM events
		WHERE id = ?
	`

	e, err := scanEvent(s.db.QueryRowContext(ctx, query, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get event by id: %w", err)
	}
	return e, nil
}

// QueryEventsByType 按事件类型查询 Event。
func (s *SQLiteStore) QueryEventsByType(ctx context.Context, sessionID, eventType string, limit, offset int) ([]*event.Event, error) {
	query := `
		SELECT id, session_id, type, schema_id, source, timestamp,
		       causation_id, correlation_id, origin_id, context, payload
		FROM events
		WHERE session_id = ? AND type = ?
		ORDER BY timestamp DESC
	`
	// limit<=0 视为"无限制"（与 applyLimitOffset 语义一致）。
	query, args := applyLimitOffset(query, []any{sessionID, eventType}, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query events by type: %w", err)
	}
	defer rows.Close()

	var events []*event.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}

	return events, nil
}

// QueryEventsByCorrelation 按关联 ID 查询 Event。
func (s *SQLiteStore) QueryEventsByCorrelation(ctx context.Context, correlationID string, limit, offset int) ([]*event.Event, error) {
	query := `
		SELECT id, session_id, type, schema_id, source, timestamp,
		       causation_id, correlation_id, origin_id, context, payload
		FROM events
		WHERE correlation_id = ?
		ORDER BY timestamp DESC
	`
	// limit<=0 视为"无限制"（与 applyLimitOffset 语义一致）。
	query, args := applyLimitOffset(query, []any{correlationID}, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query events by correlation: %w", err)
	}
	defer rows.Close()

	var events []*event.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}

	return events, nil
}

// applyLimitOffset 追加 LIMIT/OFFSET 子句到查询。
func applyLimitOffset(query string, args []any, limit, offset int) (string, []any) {
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
		if offset > 0 {
			query += " OFFSET ?"
			args = append(args, offset)
		}
	}
	return query, args
}

// QueryRawPackets 查询 raw_packets 表，支持 protocol/src/dst 过滤。
// capture.sqlite 为单 session 库，raw_packets 表目前无 session_id 列。
func (s *SQLiteStore) QueryRawPackets(ctx context.Context, q RawPacketQuery) ([]RawPacketRow, error) {
	query := `SELECT id, timestamp, src, dst, protocol, payload, link_type
FROM raw_packets WHERE 1=1`
	var args []any
	if q.Protocol != "" {
		query += " AND protocol = ?"
		args = append(args, q.Protocol)
	}
	if q.Src != "" {
		query += " AND src LIKE ?"
		args = append(args, "%"+q.Src+"%")
	}
	if q.Dst != "" {
		query += " AND dst LIKE ?"
		args = append(args, "%"+q.Dst+"%")
	}
	query += " ORDER BY timestamp ASC"
	query, args = applyLimitOffset(query, args, q.Limit, q.Offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query raw packets: %w", err)
	}
	defer rows.Close()
	var result []RawPacketRow
	for rows.Next() {
		var r RawPacketRow
		if err := rows.Scan(&r.ID, &r.Timestamp, &r.Src, &r.Dst, &r.Protocol, &r.Payload, &r.LinkType); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// GetSchema 返回事件数据库的表结构信息，供 MCP get_capture_schema 工具使用。
func (s *SQLiteStore) GetSchema(ctx context.Context, sessionID string) (SchemaInfo, error) {
	tables := []string{"raw_packets", "events", "aggregated_metrics", "state_changes", "event_index"}
	var info SchemaInfo
	for _, tbl := range tables {
		ts, err := s.getTableSchema(ctx, tbl)
		if err != nil {
			slog.Warn("get schema for table", "table", tbl, "error", err)
			continue
		}
		info.Tables = append(info.Tables, ts)
	}
	return info, nil
}

// getTableSchema 查询单个表的列结构。
func (s *SQLiteStore) getTableSchema(ctx context.Context, tbl string) (TableSchema, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", tbl))
	if err != nil {
		return TableSchema{}, err
	}
	defer rows.Close()
	ts := TableSchema{Name: tbl}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return ts, err
		}
		ts.Columns = append(ts.Columns, ColumnSchema{Name: name, Type: ctype})
	}
	return ts, rows.Err()
}

// RawQuery 执行任意 SQL 查询（逃生舱）。
// 警告：不同后端方言可能不兼容；仅用于临时查询。
func (s *SQLiteStore) RawQuery(ctx context.Context, query string, args ...any) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("raw query: %w", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var result []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = vals[i]
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// 编译期断言：SQLiteStore 实现 EventReader。
var _ EventReader = (*SQLiteStore)(nil)
