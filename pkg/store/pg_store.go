package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gametrace/pkg/event"
	"gametrace/pkg/schema"
	"gametrace/pkg/capture"

	"github.com/google/uuid"
)

// PGStore 是 PostgreSQL 后端下的事件存储实现，满足完整 Store 接口。
//
// 与 SQLiteStore 的差异（方言 + 共享库隔离）：
//   - 一个 PG 库承载全部会话：raw_packets / events / state_changes / event_index
//     均带 session_id 列，所有查询以 session_id 隔离；PGStore 在构造时绑定 sessionID。
//   - 占位符用 $N（非 ?）；UPSERT 用 ON CONFLICT ... DO UPDATE（非 INSERT OR REPLACE）。
//   - raw_packets 用 id 列代替 SQLite 的 rowid 做首帧排序。
//   - 时间统一 BIGINT（unix nano），扫描走 scanPacketTime 的 int64 分支（无需文本解析）。
//   - GetSchema 用 information_schema.columns 代替 PRAGMA table_info。
//   - 共享连接池在进程级缓存，故 Close 为 no-op（不关闭底层池）。
type PGStore struct {
	db        *sql.DB
	schemaReg *schema.Registry
	sessionID string
	readOnly  bool
}

// DB 返回底层共享连接池（主要用于测试注入，避免 Windows 文件锁）。
func (s *PGStore) DB() *sql.DB { return s.db }

func (s *PGStore) Flush() error { return nil }

// Close 为 no-op：PG 连接池在进程内按 DSN 共享缓存，不应随单个会话关闭。
func (s *PGStore) Close() error { return nil }

// eventSelectSuffix PG 表始终含 scenario_id/replay_id 列，直接返回。
func (s *PGStore) eventSelectSuffix() string { return ", scenario_id, replay_id" }

// eventColsPG 是 events 表固定列清单（与 scanEvent 列顺序一致）。
const eventColsPG = `id, session_id, type, schema_id, source, timestamp,
		causation_id, correlation_id, origin_id, context, payload`

// pgArgs 累积 PostgreSQL 位置参数（$1、$2 …），避免手写编号出错。
type pgArgs struct{ args []any }

// next 追加一个参数并返回其 $N 占位符。
func (a *pgArgs) next(v any) string {
	a.args = append(a.args, v)
	return fmt.Sprintf("$%d", len(a.args))
}

// slice 返回累积的参数列表。
func (a *pgArgs) slice() []any { return a.args }

// inList 为一组值生成 ($N, $M, …) 占位符片段。
func (a *pgArgs) inList(vals []string) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = a.next(v)
	}
	return strings.Join(parts, ",")
}

// limitOffset 追加 LIMIT/OFFSET 子句（limit<=0 时为空）。
func (a *pgArgs) limitOffset(limit, offset int) string {
	if limit <= 0 {
		return ""
	}
	s := " LIMIT " + a.next(limit)
	if offset > 0 {
		s += " OFFSET " + a.next(offset)
	}
	return s
}

// ===== EventWriter =====

// AppendRawPackets 追加原始数据包（PG 版带 session_id 隔离）。
func (s *PGStore) AppendRawPackets(ctx context.Context, packets []event.Packet) error {
	if len(packets) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO raw_packets(id, session_id, timestamp, src, dst, protocol, payload, link_type, conn_id, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT(id) DO UPDATE SET
			session_id = EXCLUDED.session_id, timestamp = EXCLUDED.timestamp, src = EXCLUDED.src,
			dst = EXCLUDED.dst, protocol = EXCLUDED.protocol, payload = EXCLUDED.payload,
			link_type = EXCLUDED.link_type, conn_id = EXCLUDED.conn_id, metadata = EXCLUDED.metadata`)
	if err != nil {
		return fmt.Errorf("prepare stmt: %w", err)
	}
	defer stmt.Close()
	for _, p := range packets {
		id := p.ID
		if id == "" {
			id = uuid.NewString()
		}
		connID, metaJSON := rawPacketConnMeta(p)
		rest := appendRawPacketArgs(p, id, connID, metaJSON) // [id, ts, src, dst, proto, payload, link_type, conn_id, metaJSON]
		rowArgs := append([]any{s.sessionID}, rest...)
		if _, err := stmt.ExecContext(ctx, rowArgs...); err != nil {
			return fmt.Errorf("insert raw packet: %w", err)
		}
	}
	slog.Debug("appended raw packets", "session_id", s.sessionID, "count", len(packets))
	return tx.Commit()
}

// rawPacketConnMeta 从 Packet.Metadata 提取 conn_id 与整包 JSON（PG 版复用 SQLite 逻辑）。
func rawPacketConnMeta(p event.Packet) (sql.NullString, sql.NullString) {
	var connID sql.NullString
	if c, ok := p.Metadata["conn_id"].(string); ok && c != "" {
		connID = sql.NullString{String: c, Valid: true}
	}
	var metaJSON sql.NullString
	if len(p.Metadata) > 0 {
		if b, err := json.Marshal(p.Metadata); err == nil {
			metaJSON = sql.NullString{String: string(b), Valid: true}
		}
	}
	return connID, metaJSON
}

// AppendEvents 追加 Event（PG 版带 session_id 隔离 + ON CONFLICT）。
func (s *PGStore) AppendEvents(ctx context.Context, events []*event.Event) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO events(id, session_id, type, schema_id, source, timestamp,
			causation_id, correlation_id, origin_id, context, payload, created_at, scenario_id, replay_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT(id) DO UPDATE SET
			session_id = EXCLUDED.session_id, type = EXCLUDED.type, schema_id = EXCLUDED.schema_id,
			source = EXCLUDED.source, timestamp = EXCLUDED.timestamp, causation_id = EXCLUDED.causation_id,
			correlation_id = EXCLUDED.correlation_id, origin_id = EXCLUDED.origin_id, context = EXCLUDED.context,
			payload = EXCLUDED.payload, created_at = EXCLUDED.created_at, scenario_id = EXCLUDED.scenario_id,
			replay_id = EXCLUDED.replay_id`)
	if err != nil {
		return fmt.Errorf("prepare stmt: %w", err)
	}
	defer stmt.Close()

	now := time.Now().UnixNano()
	for i, e := range events {
		contextBytes, err := e.Context.MarshalMsgpack()
		if err != nil {
			return fmt.Errorf("marshal context for event[%d]: %w", i, err)
		}
		payloadBytes, err := e.Payload.Value.MarshalMsgpack()
		if err != nil {
			return fmt.Errorf("marshal payload for event[%d]: %w", i, err)
		}
		timestamp := e.Identity.Timestamp.UnixNano()

		var causationID, correlationID, originID, scenarioID, replayID sql.NullString
		if e.Trace.CausationID != "" {
			causationID = sql.NullString{String: string(e.Trace.CausationID), Valid: true}
		}
		if e.Trace.CorrelationID != "" {
			correlationID = sql.NullString{String: e.Trace.CorrelationID, Valid: true}
		}
		if e.Trace.OriginID != "" {
			originID = sql.NullString{String: string(e.Trace.OriginID), Valid: true}
		}
		if e.Identity.ScenarioID != "" {
			scenarioID = sql.NullString{String: e.Identity.ScenarioID, Valid: true}
		}
		if e.Identity.ReplayID != "" {
			replayID = sql.NullString{String: e.Identity.ReplayID, Valid: true}
		}

		if _, err := stmt.ExecContext(ctx,
			string(e.Identity.ID), e.Identity.SessionID, string(e.Identity.Type), e.Identity.SchemaID,
			string(e.Identity.Source), timestamp, causationID, correlationID, originID,
			contextBytes, payloadBytes, now, scenarioID, replayID,
		); err != nil {
			return fmt.Errorf("insert event[%d]: %w", i, err)
		}
	}

	if err := s.appendEventIndex(ctx, tx, events); err != nil {
		return err
	}
	slog.Debug("appended events", "session_id", s.sessionID, "count", len(events))
	return tx.Commit()
}

// appendEventIndex 写入 event_index 投影索引表（PG 版）。
func (s *PGStore) appendEventIndex(ctx context.Context, tx *sql.Tx, events []*event.Event) error {
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO event_index(event_id, session_id, type, timestamp, flow_id, direction, conn_id, correlation_id, projection_json)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT(event_id) DO UPDATE SET
			session_id = EXCLUDED.session_id, type = EXCLUDED.type, timestamp = EXCLUDED.timestamp,
			flow_id = EXCLUDED.flow_id, direction = EXCLUDED.direction, conn_id = EXCLUDED.conn_id,
			correlation_id = EXCLUDED.correlation_id, projection_json = EXCLUDED.projection_json`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range events {
		proj := extractProjection(e, s.schemaReg)
		projJSON, err := json.Marshal(proj)
		if err != nil {
			return err
		}
		var flowID, direction, connID, correlationID sql.NullString
		if e.Context.FlowID != "" {
			flowID = sql.NullString{String: e.Context.FlowID, Valid: true}
		}
		if e.Context.Direction != "" {
			direction = sql.NullString{String: e.Context.Direction, Valid: true}
		}
		if e.Context.ConnID != "" {
			connID = sql.NullString{String: e.Context.ConnID, Valid: true}
		}
		if e.Trace.CorrelationID != "" {
			correlationID = sql.NullString{String: e.Trace.CorrelationID, Valid: true}
		}
		if _, err := stmt.ExecContext(ctx,
			string(e.Identity.ID), e.Identity.SessionID, string(e.Identity.Type), e.Identity.Timestamp.UnixNano(),
			flowID, direction, connID, correlationID, string(projJSON),
		); err != nil {
			return err
		}
	}
	return nil
}

// ===== EventReader =====

func (s *PGStore) QueryEvents(ctx context.Context, sessionID string, limit, offset int) ([]*event.Event, error) {
	return s.queryEventsOrdered(ctx, sessionID, limit, offset, "ASC")
}

func (s *PGStore) QueryEventsDesc(ctx context.Context, sessionID string, limit, offset int) ([]*event.Event, error) {
	return s.queryEventsOrdered(ctx, sessionID, limit, offset, "DESC")
}

func (s *PGStore) queryEventsOrdered(ctx context.Context, sessionID string, limit, offset int, order string) ([]*event.Event, error) {
	var a pgArgs
	q := `SELECT ` + eventColsPG + s.eventSelectSuffix() + `
	FROM events WHERE session_id = ` + a.next(sessionID) + `
	ORDER BY timestamp ` + order + a.limitOffset(limit, offset)
	rows, err := s.db.QueryContext(ctx, q, a.slice()...)
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

func (s *PGStore) GetEventByID(ctx context.Context, id string) (*event.Event, error) {
	q := `SELECT ` + eventColsPG + s.eventSelectSuffix() + ` FROM events WHERE id = $1`
	e, err := scanEvent(s.db.QueryRowContext(ctx, q, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get event by id: %w", err)
	}
	return e, nil
}

func (s *PGStore) QueryEventsByType(ctx context.Context, sessionID, eventType string, limit, offset int) ([]*event.Event, error) {
	var a pgArgs
	q := `SELECT ` + eventColsPG + s.eventSelectSuffix() + `
	FROM events WHERE session_id = ` + a.next(sessionID) + ` AND type = ` + a.next(eventType) + `
	ORDER BY timestamp DESC` + a.limitOffset(limit, offset)
	rows, err := s.db.QueryContext(ctx, q, a.slice()...)
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

func (s *PGStore) QueryEventsByCorrelation(ctx context.Context, correlationID string, limit, offset int) ([]*event.Event, error) {
	var a pgArgs
	q := `SELECT ` + eventColsPG + s.eventSelectSuffix() + `
	FROM events WHERE correlation_id = ` + a.next(correlationID) + `
	ORDER BY timestamp DESC` + a.limitOffset(limit, offset)
	rows, err := s.db.QueryContext(ctx, q, a.slice()...)
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

// QueryRawPackets 查询 raw_packets（PG 版按 session_id 隔离）。
func (s *PGStore) QueryRawPackets(ctx context.Context, q RawPacketQuery) ([]RawPacketRow, error) {
	var a pgArgs
	query := `SELECT id, timestamp, src, dst, protocol, payload, link_type
	FROM raw_packets WHERE session_id = ` + a.next(s.sessionID)
	if q.Protocol != "" {
		query += ` AND protocol = ` + a.next(q.Protocol)
	}
	if q.Src != "" {
		query += ` AND src LIKE ` + a.next("%"+q.Src+"%")
	}
	if q.Dst != "" {
		query += ` AND dst LIKE ` + a.next("%"+q.Dst+"%")
	}
	query += ` ORDER BY timestamp ASC` + a.limitOffset(q.Limit, q.Offset)
	rows, err := s.db.QueryContext(ctx, query, a.slice()...)
	if err != nil {
		return nil, fmt.Errorf("query raw packets: %w", err)
	}
	defer rows.Close()
	var result []RawPacketRow
	for rows.Next() {
		var r RawPacketRow
		var ts any
		if err := rows.Scan(&r.ID, &ts, &r.Src, &r.Dst, &r.Protocol, &r.Payload, &r.LinkType); err != nil {
			return nil, err
		}
		t, err := scanPacketTime(ts)
		if err != nil {
			return nil, err
		}
		r.Timestamp = t
		result = append(result, r)
	}
	return result, rows.Err()
}

// GetSchema 返回事件库表结构（PG 版用 information_schema 代替 PRAGMA）。
func (s *PGStore) GetSchema(ctx context.Context, sessionID string) (SchemaInfo, error) {
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

func (s *PGStore) getTableSchema(ctx context.Context, tbl string) (TableSchema, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT column_name, data_type FROM information_schema.columns
		WHERE table_name = $1 ORDER BY ordinal_position`, tbl)
	if err != nil {
		return TableSchema{}, err
	}
	defer rows.Close()
	ts := TableSchema{Name: tbl}
	for rows.Next() {
		var name, dtype string
		if err := rows.Scan(&name, &dtype); err != nil {
			return ts, err
		}
		ts.Columns = append(ts.Columns, ColumnSchema{Name: name, Type: dtype})
	}
	return ts, rows.Err()
}

// RawQuery 执行任意 SQL 查询（逃生舱）。注意：PG 后端下调用方须使用 $N 占位符语法。
func (s *PGStore) RawQuery(ctx context.Context, query string, args ...any) ([]map[string]any, error) {
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

// ===== EventPager =====

func (s *PGStore) QueryEventPage(ctx context.Context, q EventPageQuery, limit, offset int) ([]*event.Event, int, error) {
	if limit <= 0 {
		return nil, 0, fmt.Errorf("query event page: limit must be > 0")
	}
	where, wargs := s.eventPageWhere(q)

	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events "+where, wargs...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count events: %w", err)
	}

	n := len(wargs)
	pageQuery := `SELECT ` + eventColsPG + s.eventSelectSuffix() + `
	FROM events ` + where + `
	ORDER BY timestamp DESC LIMIT $` + fmt.Sprintf("%d", n+1) + ` OFFSET $` + fmt.Sprintf("%d", n+2)
	pageArgs := append(append([]any{}, wargs...), limit, offset)

	rows, err := s.db.QueryContext(ctx, pageQuery, pageArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("query event page: %w", err)
	}
	defer rows.Close()
	events := make([]*event.Event, 0, limit)
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, 0, err
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate event page: %w", err)
	}
	return events, total, nil
}

func (s *PGStore) StreamEventsDesc(ctx context.Context, q EventPageQuery, batch int, yield func([]*event.Event) (bool, error)) error {
	if batch <= 0 {
		batch = 500
	}
	where, wargs := s.eventPageWhere(q)
	streamQuery := `SELECT ` + eventColsPG + s.eventSelectSuffix() + `
	FROM events ` + where + `
	ORDER BY timestamp DESC`
	rows, err := s.db.QueryContext(ctx, streamQuery, wargs...)
	if err != nil {
		return fmt.Errorf("stream events: %w", err)
	}
	defer rows.Close()
	buf := make([]*event.Event, 0, batch)
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return err
		}
		buf = append(buf, e)
		if len(buf) >= batch {
			if cont, err := yield(buf); !cont || err != nil {
				return err
			}
			buf = make([]*event.Event, 0, batch)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate event stream: %w", err)
	}
	if len(buf) > 0 {
		if _, err := yield(buf); err != nil {
			return err
		}
	}
	return nil
}

// eventPageWhere 组装事件分页查询的 WHERE 片段与 $N 绑定参数（PG 版）。
func (s *PGStore) eventPageWhere(q EventPageQuery) (string, []any) {
	var a pgArgs
	where := "WHERE session_id = " + a.next(q.SessionID)
	if q.TypeEq != "" {
		where += " AND type = " + a.next(q.TypeEq)
	} else if q.TypeNot != "" {
		where += " AND type != " + a.next(q.TypeNot)
	}
	return where, a.slice()
}

// ===== ProjectionWriter =====

func (s *PGStore) WriteMetrics(ctx context.Context, metrics []event.Metric) error {
	if len(metrics) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO aggregated_metrics(session_id, name, "window", group_json, value)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT(session_id, name, "window", group_json) DO UPDATE SET value = EXCLUDED.value`)
	if err != nil {
		return fmt.Errorf("prepare stmt: %w", err)
	}
	defer stmt.Close()
	for _, m := range metrics {
		gj, err := json.Marshal(m.Group)
		if err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, s.sessionID, m.Name, m.Window.UnixNano(), string(gj), m.Value); err != nil {
			return fmt.Errorf("write metric: %w", err)
		}
	}
	slog.Debug("wrote metrics", "session_id", s.sessionID, "count", len(metrics))
	return tx.Commit()
}

// WriteStateChanges 从 Event 批量写入 StateChange（PG 版）。
func (s *PGStore) WriteStateChanges(ctx context.Context, sessionID string, events []*event.Event) error {
	if len(events) == 0 {
		return nil
	}
	var enriched []EnrichedStateChange
	for _, ev := range events {
		flowID := extractFlowIDFromEvent(ev)
		for _, sc := range ev.ExtractStateChanges() {
			enriched = append(enriched, EnrichedStateChange{
				StateChange:    sc,
				EventID:        ev.Identity.ID,
				FlowID:         flowID,
				Timestamp:      ev.Identity.Timestamp,
				BeforeResolved: false,
				AfterResolved:  false,
				EntityVersion:  sc.Version,
			})
		}
	}
	return s.WriteEnrichedStateChanges(ctx, sessionID, enriched)
}

// WriteEnrichedStateChanges 写入经过语义基线解析的 StateChange（PG 版）。
func (s *PGStore) WriteEnrichedStateChanges(ctx context.Context, sessionID string, changes []EnrichedStateChange) error {
	if len(changes) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO state_changes(id, event_id, session_id, flow_id, timestamp, subject_type, subject_id, op, path, before_value, after_value, version, before_resolved, after_resolved, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`)
	if err != nil {
		return fmt.Errorf("prepare stmt: %w", err)
	}
	defer stmt.Close()

	count, skipped := 0, 0
	for _, esc := range changes {
		if err := esc.Validate(); err != nil {
			slog.Warn("skip invalid enriched state change", "event_id", esc.EventID, "error", err)
			skipped++
			continue
		}
		beforeJSON, _ := json.Marshal(esc.Before.ToAny())
		afterJSON, _ := json.Marshal(esc.After.ToAny())
		metaJSON, _ := json.Marshal(esc.Metadata.ToAny())

		var flowID sql.NullString
		if esc.FlowID != "" {
			flowID = sql.NullString{String: esc.FlowID, Valid: true}
		}
		if _, err := stmt.ExecContext(ctx,
			uuid.NewString(), string(esc.EventID), sessionID, flowID, esc.Timestamp.UnixNano(),
			esc.SubjectType, esc.SubjectID, esc.Op, esc.Path,
			string(beforeJSON), string(afterJSON), esc.Version, esc.BeforeResolved, esc.AfterResolved,
			string(metaJSON),
		); err != nil {
			return fmt.Errorf("insert state change: %w", err)
		}
		count++
	}
	if count > 0 || skipped > 0 {
		slog.Debug("wrote enriched state changes", "session_id", sessionID, "count", count, "skipped", skipped)
	}
	return tx.Commit()
}

// ===== ProjectionReader =====

func (s *PGStore) QueryMetrics(ctx context.Context, q MetricQuery) ([]MetricRow, error) {
	var a pgArgs
	query := `SELECT name, "window", group_json, value FROM aggregated_metrics WHERE session_id = ` + a.next(s.sessionID)
	if q.Name != "" {
		query += ` AND name = ` + a.next(q.Name)
	}
	query += ` ORDER BY "window" ASC` + a.limitOffset(q.Limit, q.Offset)
	rows, err := s.db.QueryContext(ctx, query, a.slice()...)
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
				r.Group = nil
			}
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func (s *PGStore) QueryStateChanges(ctx context.Context, q StateChangeQuery) ([]StateChangeRow, error) {
	var a pgArgs
	sid := q.SessionID
	if sid == "" {
		sid = s.sessionID
	}
	query := `SELECT id, event_id, session_id, flow_id, timestamp, subject_type, subject_id, op, path, before_value, after_value, version, metadata
	FROM state_changes WHERE session_id = ` + a.next(sid)
	if q.FlowID != "" {
		query += ` AND flow_id = ` + a.next(q.FlowID)
	}
	if q.SubjectType != "" {
		query += ` AND subject_type = ` + a.next(q.SubjectType)
	}
	if q.SubjectID != "" {
		query += ` AND subject_id = ` + a.next(q.SubjectID)
	}
	if q.Op != "" {
		query += ` AND op = ` + a.next(q.Op)
	}
	if q.Path != "" {
		query += ` AND path = ` + a.next(q.Path)
	}
	query += ` ORDER BY timestamp ASC` + a.limitOffset(q.Limit, q.Offset)
	rows, err := s.db.QueryContext(ctx, query, a.slice()...)
	if err != nil {
		return nil, fmt.Errorf("query state changes: %w", err)
	}
	defer rows.Close()
	var result []StateChangeRow
	for rows.Next() {
		var r StateChangeRow
		var tsNano int64
		var flowID, beforeValue, afterValue, metadata sql.NullString
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

// ===== Clearer =====

// ClearDecodedData 清空 events / state_changes / event_index（保留 raw_packets）。
func (s *PGStore) ClearDecodedData(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM events WHERE session_id = $1", s.sessionID); err != nil {
		return fmt.Errorf("clear events: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM state_changes WHERE session_id = $1", s.sessionID); err != nil {
		return fmt.Errorf("clear state_changes: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM event_index WHERE session_id = $1", s.sessionID); err != nil {
		return fmt.Errorf("clear event_index: %w", err)
	}
	return tx.Commit()
}

// ===== ConnectionQuerier =====

func (s *PGStore) QueryConnections(ctx context.Context, sessionID string, limit, offset int) ([]ConnectionSummary, error) {
	var a pgArgs
	sid := a.next(s.sessionID) // 共享库：子查询与主查询统一按 session_id 隔离
	sub := `WHERE f.conn_id = r.conn_id AND f.session_id = ` + sid + ` ORDER BY f.timestamp ASC, f.id ASC LIMIT 1`
	query := `
		SELECT r.conn_id, r.first_ts, r.last_ts, r.frame_count,
		       (SELECT f.src FROM raw_packets f ` + sub + `) AS src,
		       (SELECT f.dst FROM raw_packets f ` + sub + `) AS dst,
		       (SELECT f.protocol FROM raw_packets f ` + sub + `) AS protocol,
		       (SELECT f.metadata FROM raw_packets f ` + sub + `) AS metadata
		FROM (
			SELECT conn_id,
			       MIN(timestamp) AS first_ts,
			       MAX(timestamp) AS last_ts,
			       COUNT(*) AS frame_count
			FROM raw_packets WHERE session_id = ` + sid + ` AND conn_id IS NOT NULL AND conn_id != ''
			GROUP BY conn_id
		) r
		ORDER BY r.last_ts DESC` + a.limitOffset(limit, offset)

	rows, err := s.db.QueryContext(ctx, query, a.slice()...)
	if err != nil {
		return nil, fmt.Errorf("query connections: %w", err)
	}
	defer rows.Close()

	conns := make([]ConnectionSummary, 0)
	connIDs := make([]string, 0)
	index := make(map[string]int)
	for rows.Next() {
		var c ConnectionSummary
		var firstTS, lastTS any
		var src, dst, protocol string
		var meta sql.NullString
		if err := rows.Scan(&c.ConnID, &firstTS, &lastTS, &c.FrameCount, &src, &dst, &protocol, &meta); err != nil {
			return nil, fmt.Errorf("scan connection: %w", err)
		}
		first, err := scanPacketTime(firstTS)
		if err != nil {
			return nil, err
		}
		last, err := scanPacketTime(lastTS)
		if err != nil {
			return nil, err
		}
		c.StartTime = first
		c.EndTime = last
		c.DurationSec = c.EndTime.Sub(c.StartTime).Seconds()
		c.Client, c.Server, c.Source = endpointsFromMeta(meta.String, src, dst)
		c.Protocol = protocol
		index[c.ConnID] = len(conns)
		conns = append(conns, c)
		connIDs = append(connIDs, c.ConnID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.enrichConnEvents(ctx, sessionID, conns, connIDs, index)
	return conns, nil
}

// enrichConnEvents 从 event_index 补充连接的解码事件统计（PG 版，按 session_id 隔离）。
func (s *PGStore) enrichConnEvents(ctx context.Context, sessionID string, conns []ConnectionSummary, connIDs []string, index map[string]int) {
	if len(connIDs) == 0 {
		return
	}
	var a pgArgs
	ph := a.inList(connIDs)
	args := append([]any{sessionID}, a.slice()...)
	rows, err := s.db.QueryContext(ctx, `
		SELECT conn_id, COUNT(*),
		       (SELECT type FROM event_index ei2
		         WHERE ei2.session_id = $1 AND ei2.conn_id = event_index.conn_id AND ei2.type IS NOT NULL
		         ORDER BY ei2.timestamp ASC LIMIT 1) AS first_type
		 FROM event_index
		 WHERE session_id = $1 AND conn_id IN (`+ph+`)
		 GROUP BY conn_id`, args...)
	if err != nil {
		slog.Debug("enrich connection events failed (non-fatal)", "error", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var connID string
		var eventCount int
		var eventType sql.NullString
		if err := rows.Scan(&connID, &eventCount, &eventType); err != nil {
			continue
		}
		i, ok := index[connID]
		if !ok {
			continue
		}
		conns[i].EventCount = eventCount
		if eventType.Valid {
			conns[i].EventType = eventType.String
		}
	}
}

func (s *PGStore) QueryConnectionDetail(ctx context.Context, sessionID, connID string) (*ConnectionDetail, error) {
	var d ConnectionDetail
	d.ConnID = connID

	var firstTS, lastTS any
	if err := s.db.QueryRowContext(ctx, `
		SELECT MIN(timestamp), MAX(timestamp), COUNT(*)
		FROM raw_packets WHERE conn_id = $1 AND session_id = $2`, connID, sessionID,
	).Scan(&firstTS, &lastTS, &d.FrameCount); err != nil {
		return nil, fmt.Errorf("query connection detail: %w", err)
	}
	if firstTS != nil {
		first, err := scanPacketTime(firstTS)
		if err != nil {
			return nil, err
		}
		last, err := scanPacketTime(lastTS)
		if err != nil {
			return nil, err
		}
		d.StartTime = first
		d.EndTime = last
		d.DurationSec = d.EndTime.Sub(d.StartTime).Seconds()
	}

	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       (SELECT type FROM event_index ei2
		         WHERE ei2.session_id = $1 AND ei2.conn_id = $2 AND ei2.type IS NOT NULL
		         ORDER BY ei2.timestamp ASC LIMIT 1)
		FROM event_index WHERE session_id = $1 AND conn_id = $2`,
		sessionID, connID,
	).Scan(&d.EventCount, &d.EventType); err != nil {
		slog.Debug("query connection event stats failed (non-fatal)", "error", err)
	}

	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
			SELECT COALESCE(NULLIF(correlation_id, ''), event_id) AS k
			FROM event_index WHERE session_id = $1 AND conn_id = $2
			GROUP BY k
		)`, sessionID, connID,
	).Scan(&d.StreamCount); err != nil {
		slog.Debug("query connection stream count failed (non-fatal)", "error", err)
	}

	var src, dst, protocol string
	var meta sql.NullString
	if err := s.db.QueryRowContext(ctx, `
		SELECT src, dst, protocol, metadata FROM raw_packets
		WHERE conn_id = $1 AND session_id = $2 ORDER BY timestamp ASC LIMIT 1`,
		connID, sessionID,
	).Scan(&src, &dst, &protocol, &meta); err == nil {
		d.Client, d.Server, d.Source = endpointsFromMeta(meta.String, src, dst)
		d.Protocol = protocol
		if meta.Valid {
			var m map[string]any
			if json.Unmarshal([]byte(meta.String), &m) == nil {
				d.App, _ = m[capture.MetaAppPackage].(string)
				d.Device, _ = m[capture.MetaDevice].(string)
			}
		}
	}

	if d.FrameCount == 0 && d.EventCount == 0 {
		return nil, nil
	}
	return &d, nil
}

func (s *PGStore) QueryConnectionEvents(ctx context.Context, sessionID, connID string, limit, offset int) ([]*event.Event, error) {
	var a pgArgs
	q := `SELECT ` + eventColsPG + s.eventSelectSuffix() + `
		FROM events e JOIN event_index ei ON ei.event_id = e.id
		WHERE ei.session_id = ` + a.next(sessionID) + ` AND ei.conn_id = ` + a.next(connID) + `
		ORDER BY e.timestamp ASC` + a.limitOffset(limit, offset)
	rows, err := s.db.QueryContext(ctx, q, a.slice()...)
	if err != nil {
		return nil, fmt.Errorf("query connection events: %w", err)
	}
	defer rows.Close()
	var events []*event.Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate connection events: %w", err)
	}
	return events, nil
}

func (s *PGStore) QueryConnectionStreams(ctx context.Context, sessionID, connID string, limit, offset int) ([]ConnectionStream, error) {
	const maxStreamEvents = 2000
	events, err := s.QueryConnectionEvents(ctx, sessionID, connID, maxStreamEvents, 0)
	if err != nil {
		return nil, err
	}
	type group struct{ stream *ConnectionStream }
	byKey := make(map[string]*ConnectionStream)
	var order []string
	for _, ev := range events {
		key := ev.Trace.CorrelationID
		if key == "" {
			key = string(ev.Identity.ID)
		}
		stream, ok := byKey[key]
		if !ok {
			stream = &ConnectionStream{Key: key, CorrelID: ev.Trace.CorrelationID}
			byKey[key] = stream
			order = append(order, key)
		}
		ce := toConnectionEvent(ev)
		stream.Events = append(stream.Events, ce)
		if stream.StartTime.IsZero() || ce.Timestamp.Before(stream.StartTime) {
			stream.StartTime = ce.Timestamp
		}
		if ce.Timestamp.After(stream.EndTime) {
			stream.EndTime = ce.Timestamp
		}
		stream.EventCount = len(stream.Events)
	}
	streams := make([]ConnectionStream, 0, len(order))
	for i, key := range order {
		st := byKey[key]
		st.Seq = i + 1
		streams = append(streams, *st)
	}
	start := offset
	if start > len(streams) {
		start = len(streams)
	}
	end := len(streams)
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	return streams[start:end], nil
}

func (s *PGStore) QueryConnectionFrames(ctx context.Context, connID string, limit, offset int) ([]ConnectionFrame, error) {
	var a pgArgs
	q := `SELECT id, timestamp, src, dst, protocol, payload, link_type, metadata
		FROM raw_packets WHERE conn_id = ` + a.next(connID) + ` AND session_id = ` + a.next(s.sessionID) + `
		ORDER BY timestamp ASC` + a.limitOffset(limit, offset)
	rows, err := s.db.QueryContext(ctx, q, a.slice()...)
	if err != nil {
		return nil, fmt.Errorf("query connection frames: %w", err)
	}
	defer rows.Close()
	frames := make([]ConnectionFrame, 0)
	for rows.Next() {
		var f ConnectionFrame
		var meta sql.NullString
		var ts any
		if err := rows.Scan(&f.ID, &ts, &f.Src, &f.Dst, &f.Protocol, &f.Payload, &f.LinkType, &meta); err != nil {
			return nil, err
		}
		t, err := scanPacketTime(ts)
		if err != nil {
			return nil, err
		}
		f.Timestamp = t
		if meta.Valid {
			var m map[string]any
			if json.Unmarshal([]byte(meta.String), &m) == nil {
				if dir, _ := m["direction"].(string); dir != "" {
					f.Direction = dir
				}
			}
		}
		frames = append(frames, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return frames, nil
}

// 编译期断言：PGStore 实现完整 Store 接口。
var _ Store = (*PGStore)(nil)
