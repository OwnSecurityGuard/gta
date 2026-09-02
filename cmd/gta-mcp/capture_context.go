package main

import (
	"context"
	"database/sql"
	"log/slog"

	"gta/pkg/event"
)

// captureCtxRow 是捕获上下文计算的输入行：来自 events JOIN event_index 的
// 轻量查询（时间倒序），context 仅解码 EventContext（小 map），
// 不触碰 payload——对比旧路径的全量事件解码。
type captureCtxRow struct {
	ID            string
	Source        string
	ConnID        string
	CorrelationID string
}

// buildCaptureContextFromIndex 基于事件索引行计算每个事件的连接/流序号，
// 语义与旧 buildCaptureContext（基于全量解码事件）完全一致：
//   - 连接序号：按连接最新事件时间倒序，最新连接为 1；
//   - 流序号：连接内按流首事件时间正序，每流从 1 递增；
//   - 流分组键：correlation_id 非空同组；未关联事件各自成流。
//
// 查询走 idx_event_index_conn（仅代理抓包 conn_id 非空事件命中），
// 网卡抓包会话无 conn 行时零成本返回空 map。
// 失败时降级为空 map 并记 Debug 日志（capture 字段缺失不影响事件列表）。
func buildCaptureContextFromIndex(ctx context.Context, reader captureReader, sessionID string) map[string]captureContextJSON {
	db, ok := reader.(interface{ DB() *sql.DB })
	if !ok {
		return map[string]captureContextJSON{}
	}
	rows, err := db.DB().QueryContext(ctx, `
		SELECT e.id, e.context, ei.conn_id, COALESCE(ei.correlation_id, '')
		FROM events e
		JOIN event_index ei ON ei.event_id = e.id
		WHERE e.session_id = ? AND ei.conn_id IS NOT NULL AND ei.conn_id != ''
		ORDER BY ei.timestamp DESC`,
		sessionID,
	)
	if err != nil {
		slog.Debug("query capture context rows failed (non-fatal)", "error", err)
		return map[string]captureContextJSON{}
	}
	defer rows.Close()

	list := make([]captureCtxRow, 0, 64)
	for rows.Next() {
		var r captureCtxRow
		var contextBytes []byte
		if err := rows.Scan(&r.ID, &contextBytes, &r.ConnID, &r.CorrelationID); err != nil {
			slog.Debug("scan capture context row failed (non-fatal)", "error", err)
			continue
		}
		// context 只承载 flow/raw_packet_id/conn_id/source 等小字段，
		// 解码成本远低于 payload；解码失败按空 context 容错（旧数据兼容）。
		if ec, err := event.UnmarshalContextMsgpack(contextBytes); err == nil {
			r.Source = ec.Source
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		slog.Debug("iterate capture context rows failed (non-fatal)", "error", err)
	}
	return buildCaptureContextFromRows(list)
}

// buildCaptureContextFromRows 在轻量行上执行连接/流序号算法（时间倒序输入）。
func buildCaptureContextFromRows(rows []captureCtxRow) map[string]captureContextJSON {
	out := make(map[string]captureContextJSON, len(rows))
	connSeqByID := make(map[string]int)
	streamSeqByConn := make(map[string]int)
	connRows := make(map[string][]captureCtxRow)

	for _, r := range rows {
		if _, ok := connSeqByID[r.ConnID]; !ok {
			connSeqByID[r.ConnID] = len(connSeqByID) + 1
			streamSeqByConn[r.ConnID] = 0
		}
		connRows[r.ConnID] = append(connRows[r.ConnID], r)
	}

	for connID, rs := range connRows {
		// rs 为时间倒序，反转为正序以符合流首事件时间正序。
		asc := make([]captureCtxRow, len(rs))
		for i, r := range rs {
			asc[len(rs)-1-i] = r
		}
		seenStream := make(map[string]bool)
		for _, r := range asc {
			key := r.CorrelationID
			if key == "" {
				key = r.ID
			}
			if !seenStream[key] {
				seenStream[key] = true
				streamSeqByConn[connID]++
			}
			out[r.ID] = captureContextJSON{
				CapturedBy: captureDisplayName(r.Source),
				ConnID:     connID,
				ConnSeq:    connSeqByID[connID],
				StreamID:   key,
				StreamSeq:  streamSeqByConn[connID],
				Source:     r.Source,
			}
		}
	}
	return out
}
