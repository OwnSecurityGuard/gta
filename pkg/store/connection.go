package store

// 代理抓包连接聚合查询（Connections 页面数据层）。
//
// 概念映射：
//   - Connection（连接）= 移动代理的 conn_id（同一连接的两个方向共享）；
//   - Stream（流）= 连接内按关联键（correlation_id）划分的对话；
//     request/response 通过 CorrelationID 配对成一条流；未关联事件（如 push）各自成流；
//   - Frame（帧）= 连接内重组后的应用层原始帧（raw_packets 行）。
//
// capture.sqlite 为单 session 库，所有查询以 session_id + conn_id 为边界。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gta/pkg/capture"
	"gta/pkg/event"
)

// ConnectionSummary 连接列表行（Connections 页面）。
type ConnectionSummary struct {
	ConnID      string    `json:"conn_id"`
	Client      string    `json:"client"`
	Server      string    `json:"server"`
	Protocol    string    `json:"protocol"`     // 原始网络协议（如 tcp）
	EventType   string    `json:"event_type"`   // 连接内首个解码事件类型（如 http_req），可用于展示 HTTPS/HTTP
	Source      string    `json:"source"`       // 抓包来源（mobile / pcap-live / ...）
	StartTime   time.Time `json:"start_time"`
	EndTime     time.Time `json:"end_time"`
	DurationSec float64   `json:"duration_sec"`
	EventCount  int       `json:"event_count"`
	FrameCount  int       `json:"frame_count"`
}

// ConnectionDetail 连接详情（Connection Detail 页面头部 + 统计）。
type ConnectionDetail struct {
	ConnID      string    `json:"conn_id"`
	Client      string    `json:"client"`
	Server      string    `json:"server"`
	Protocol    string    `json:"protocol"`
	EventType   string    `json:"event_type"`
	Source      string    `json:"source"`
	App         string    `json:"app"`
	Device      string    `json:"device"`
	StartTime   time.Time `json:"start_time"`
	EndTime     time.Time `json:"end_time"`
	DurationSec float64   `json:"duration_sec"`
	EventCount  int       `json:"event_count"`
	StreamCount int       `json:"stream_count"`
	FrameCount  int       `json:"frame_count"`
}

// ConnectionEvent 连接内单个解码事件（Stream View 与 Events 子页共用）。
type ConnectionEvent struct {
	ID            string         `json:"id"`
	Timestamp     time.Time      `json:"timestamp"`
	Type          string         `json:"type"`
	Direction     string         `json:"direction"`
	CorrelationID string         `json:"correlation_id"`
	FlowID        string         `json:"flow_id"`
	MsgName       string         `json:"msg_name"`
	Data          map[string]any `json:"data"`
}

// ConnectionStream 连接内一条流（按 correlation_id 分组；未关联事件各自成流）。
type ConnectionStream struct {
	Seq        int               `json:"seq"` // 流序号（1-based，按首事件时间排序）
	Key        string            `json:"key"`
	CorrelID   string            `json:"correlation_id"` // 非空表示该流来自关联对话
	StartTime  time.Time         `json:"start_time"`
	EndTime    time.Time         `json:"end_time"`
	EventCount int               `json:"event_count"`
	Events     []ConnectionEvent `json:"events"`
}

// ConnectionFrame 连接内原始帧（Frames / Raw 子页共用）。
type ConnectionFrame struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Direction string    `json:"direction"`
	Src       string    `json:"src"`
	Dst       string    `json:"dst"`
	Protocol  string    `json:"protocol"`
	LinkType  int       `json:"link_type"`
	Payload   []byte    `json:"payload"`
}

// QueryConnections 按 conn_id 聚合返回连接列表（最新在前）。
// 数据主源为 raw_packets（原始帧，连接必然有帧）；event_index 仅用于补充解码事件统计。
// capture.sqlite 为单 session 库，raw_packets 无 session_id 列，故仅按 conn_id 分组。
func (s *SQLiteStore) QueryConnections(ctx context.Context, sessionID string, limit, offset int) ([]ConnectionSummary, error) {
	query := `
		SELECT conn_id,
		       MIN(timestamp) AS first_ts,
		       MAX(timestamp) AS last_ts,
		       COUNT(*) AS frame_count,
		       MIN(src) AS src,
		       MIN(dst) AS dst,
		       MIN(protocol) AS protocol,
		       MIN(metadata) AS metadata
		FROM raw_packets
		WHERE conn_id IS NOT NULL AND conn_id != ''
		GROUP BY conn_id
		ORDER BY MAX(timestamp) DESC`
	query, args := applyLimitOffset(query, nil, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query connections: %w", err)
	}
	defer rows.Close()

	conns := make([]ConnectionSummary, 0)
	connIDs := make([]string, 0)
	index := make(map[string]int)
	for rows.Next() {
		var c ConnectionSummary
		var firstStr, lastStr, src, dst, protocol string
		var meta sql.NullString
		if err := rows.Scan(&c.ConnID, &firstStr, &lastStr, &c.FrameCount, &src, &dst, &protocol, &meta); err != nil {
			return nil, fmt.Errorf("scan connection: %w", err)
		}
		c.StartTime = parsePacketTime(firstStr)
		c.EndTime = parsePacketTime(lastStr)
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

// enrichConnEvents 从 event_index 补充连接的解码事件统计（无解码事件时保持零值）。
func (s *SQLiteStore) enrichConnEvents(ctx context.Context, sessionID string, conns []ConnectionSummary, connIDs []string, index map[string]int) {
	if len(connIDs) == 0 {
		return
	}
	placeholders := strings.Repeat(",?", len(connIDs)-1)
	args := make([]any, len(connIDs)+1)
	args[0] = sessionID
	for i, id := range connIDs {
		args[i+1] = id
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT conn_id, COUNT(*),
		        (SELECT type FROM event_index ei2
		          WHERE ei2.session_id = event_index.session_id
		            AND ei2.conn_id = event_index.conn_id
		            AND ei2.type IS NOT NULL
		          ORDER BY ei2.timestamp ASC LIMIT 1) AS first_type
		 FROM event_index
		 WHERE session_id = ? AND conn_id IN (?`+placeholders+`)
		 GROUP BY conn_id`,
		args...,
	)
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

// QueryConnectionDetail 查询单个连接的详情（头部信息 + 统计）。
// 时间范围/帧数取自 raw_packets（连接必然有帧）；解码事件统计取自 event_index（可为空）。
func (s *SQLiteStore) QueryConnectionDetail(ctx context.Context, sessionID, connID string) (*ConnectionDetail, error) {
	var d ConnectionDetail
	d.ConnID = connID

	// 时间范围与帧数（raw_packets 总有数据；文本时间戳在应用层解析）。
	var firstStr, lastStr string
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(MIN(timestamp), ''), COALESCE(MAX(timestamp), ''), COUNT(*)
		FROM raw_packets WHERE conn_id = ?`,
		connID,
	).Scan(&firstStr, &lastStr, &d.FrameCount); err != nil {
		return nil, fmt.Errorf("query connection detail: %w", err)
	}
	if firstStr != "" {
		d.StartTime = parsePacketTime(firstStr)
		d.EndTime = parsePacketTime(lastStr)
		d.DurationSec = d.EndTime.Sub(d.StartTime).Seconds()
	}

	// 解码事件统计（无解码事件时 COUNT=0、type=NULL，均为零值）。
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       (SELECT type FROM event_index ei2
		         WHERE ei2.session_id = event_index.session_id
		           AND ei2.conn_id = event_index.conn_id
		           AND ei2.type IS NOT NULL
		         ORDER BY ei2.timestamp ASC LIMIT 1)
		FROM event_index
		WHERE session_id = ? AND conn_id = ?`,
		sessionID, connID,
	).Scan(&d.EventCount, &d.EventType); err != nil {
		slog.Debug("query connection event stats failed (non-fatal)", "error", err)
	}

	// 流数量：按 (correlation_id 或 event_id) 去重分组。
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
			SELECT COALESCE(NULLIF(correlation_id, ''), event_id) AS k
			FROM event_index WHERE session_id = ? AND conn_id = ?
			GROUP BY k
		)`,
		sessionID, connID,
	).Scan(&d.StreamCount); err != nil {
		slog.Debug("query connection stream count failed (non-fatal)", "error", err)
	}

	// 端地址 / 协议 / 来源 / 应用/设备：取连接内第一帧。
	var src, dst, protocol string
	var meta sql.NullString
	if err := s.db.QueryRowContext(ctx, `
		SELECT src, dst, protocol, metadata FROM raw_packets
		WHERE conn_id = ? ORDER BY timestamp ASC LIMIT 1`,
		connID,
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

	// 连接完全不存在（既无帧也无事件）时返回 nil，维持"not found"语义。
	if d.FrameCount == 0 && d.EventCount == 0 {
		return nil, nil
	}

	return &d, nil
}

// QueryConnectionEvents 查询连接内全部解码事件（时间正序）。
func (s *SQLiteStore) QueryConnectionEvents(ctx context.Context, sessionID, connID string, limit, offset int) ([]*event.Event, error) {
	query := `
		SELECT e.id, e.session_id, e.type, e.schema_id, e.source, e.timestamp,
		       e.causation_id, e.correlation_id, e.origin_id, e.context, e.payload` + s.eventSelectSuffix() + `
		FROM events e
		JOIN event_index ei ON ei.event_id = e.id
		WHERE ei.session_id = ? AND ei.conn_id = ?
		ORDER BY e.timestamp ASC`
	query, args := applyLimitOffset(query, []any{sessionID, connID}, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
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
		return nil, err
	}
	return events, nil
}

// QueryConnectionStreams 把连接内事件按关联键分组为流。
// 分组规则：correlation_id 非空的同组事件 → 同一条流；未关联事件各自成流。
// 连接通常较小，故一次加载全部事件在内存分组（上限 maxStreamEvents，超出仅保留前 N 条）。
func (s *SQLiteStore) QueryConnectionStreams(ctx context.Context, sessionID, connID string, limit, offset int) ([]ConnectionStream, error) {
	const maxStreamEvents = 2000
	events, err := s.QueryConnectionEvents(ctx, sessionID, connID, maxStreamEvents, 0)
	if err != nil {
		return nil, err
	}

	type group struct {
		stream *ConnectionStream
	}
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

	// 分页（limit/offset 作用于流本身）。
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

// QueryConnectionFrames 查询连接内的原始帧（时间正序），方向从 metadata 还原。
func (s *SQLiteStore) QueryConnectionFrames(ctx context.Context, connID string, limit, offset int) ([]ConnectionFrame, error) {
	query := `
		SELECT id, timestamp, src, dst, protocol, payload, link_type, metadata
		FROM raw_packets
		WHERE conn_id = ?
		ORDER BY timestamp ASC`
	query, args := applyLimitOffset(query, []any{connID}, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query connection frames: %w", err)
	}
	defer rows.Close()

	frames := make([]ConnectionFrame, 0)
	for rows.Next() {
		var f ConnectionFrame
		var meta sql.NullString
		if err := rows.Scan(&f.ID, &f.Timestamp, &f.Src, &f.Dst, &f.Protocol, &f.Payload, &f.LinkType, &meta); err != nil {
			return nil, err
		}
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

// toConnectionEvent 把 Event 转换为轻量 ConnectionEvent（提取 _meta.msg_name）。
func toConnectionEvent(ev *event.Event) ConnectionEvent {
	return ConnectionEvent{
		ID:            string(ev.Identity.ID),
		Timestamp:     ev.Identity.Timestamp,
		Type:          string(ev.Identity.Type),
		Direction:     ev.Context.Direction,
		CorrelationID: ev.Trace.CorrelationID,
		FlowID:        ev.Context.FlowID,
		MsgName:       eventMsgName(ev),
		Data:          payloadToAny(ev),
	}
}

// eventMsgName 从 Payload._meta.msg_name 提取消息名。
func eventMsgName(ev *event.Event) string {
	if ev == nil {
		return ""
	}
	obj, ok := ev.Payload.Value.AsObject()
	if !ok {
		return ""
	}
	meta, ok := obj["_meta"]
	if !ok {
		return ""
	}
	metaObj, ok := meta.AsObject()
	if !ok {
		return ""
	}
	if v, ok := metaObj["msg_name"]; ok {
		if s, ok := v.AsString(); ok {
			return s
		}
	}
	return ""
}

// payloadToAny 返回事件载荷的 map 表示。
func payloadToAny(ev *event.Event) map[string]any {
	data, _ := ev.Payload.Value.ToAny().(map[string]any)
	if data == nil {
		return map[string]any{}
	}
	return data
}

// parsePacketTime 解析 raw_packets.timestamp 文本时间为 time.Time。
// modernc.org/sqlite 无法将此类文本时间戳直接扫描为 time.Time，strftime 也不识别该格式，
// 故先扫描为字符串再在应用层解析（格式如 "2026-08-23 17:51:31 +0800 CST"）。
func parsePacketTime(s string) time.Time {
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05 -0700 MST",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// endpointsFromMeta 从帧 metadata 还原 client/server/source，缺失时回退到 src/dst。
func endpointsFromMeta(metaJSON, src, dst string) (client, server, source string) {
	client, server, source = src, dst, ""
	if metaJSON == "" {
		return client, server, source
	}
	var m map[string]any
	if json.Unmarshal([]byte(metaJSON), &m) != nil {
		return client, server, source
	}
	if v, _ := m[capture.MetaClientAddr].(string); v != "" {
		client = v
	}
	if v, _ := m[capture.MetaServerAddr].(string); v != "" {
		server = v
	}
	if v, _ := m[capture.MetaSource].(string); v != "" {
		source = v
	}
	return client, server, source
}
