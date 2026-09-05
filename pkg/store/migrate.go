package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"gametrace/pkg/event"
)

// MigrateLegacyEvents 将旧版 decoded_events 表的数据迁移到新版 events 表
func (s *SQLiteStore) MigrateLegacyEvents(ctx context.Context) (int, error) {
	// 查询所有旧版事件
	query := `
		SELECT id, timestamp, session_id, protocol, raw_len, json,
		       flow_id, direction, msg_name, msg_id, is_push, src, dst, tcp_flags
		FROM decoded_events
		ORDER BY timestamp ASC
	`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("query legacy events: %w", err)
	}
	defer rows.Close()

	migrated := 0
	for rows.Next() {
		var (
			id        string
			timestamp string
			sessionID string
			protocol  string
			rawLen    int
			jsonData  []byte
			flowID    *int64
			direction *string
			msgName   *string
			msgID     *int64
			isPush    *int
			src       *string
			dst       *string
			tcpFlags  *string
		)

		err := rows.Scan(
			&id, &timestamp, &sessionID, &protocol, &rawLen, &jsonData,
			&flowID, &direction, &msgName, &msgID, &isPush, &src, &dst, &tcpFlags,
		)
		if err != nil {
			return migrated, fmt.Errorf("scan legacy event: %w", err)
		}

		// 解析 JSON 数据
		var payloadMap map[string]any
		if len(jsonData) > 0 {
			if err := json.Unmarshal(jsonData, &payloadMap); err != nil {
				slog.Warn("unmarshal legacy event JSON failed, skipping", "id", id, "error", err)
				continue
			}
		} else {
			payloadMap = make(map[string]any)
		}

		// 添加结构化字段到 payload
		if flowID != nil {
			payloadMap["flow_id"] = *flowID
		}
		if direction != nil {
			payloadMap["direction"] = *direction
		}
		if msgName != nil {
			payloadMap["msg_name"] = *msgName
		}
		if msgID != nil {
			payloadMap["msg_id"] = *msgID
		}
		if isPush != nil {
			payloadMap["is_push"] = *isPush != 0
		}
		if src != nil {
			payloadMap["src"] = *src
		}
		if dst != nil {
			payloadMap["dst"] = *dst
		}
		if tcpFlags != nil {
			payloadMap["tcp_flags"] = *tcpFlags
		}

		// 转换为 Event
		payloadValue := event.ValueFromAny(payloadMap)

		e := &event.Event{
			Identity: event.Identity{
				ID:        event.EventID(id),
				SessionID: sessionID,
				Type:      event.EventType(protocol),
				SchemaID:  protocol + ".v1",
				Source:    event.SourceID(protocol),
			},
			Trace: event.TraceContext{},
			Payload: event.Payload{
				SchemaID: protocol + ".v1",
				Value:    payloadValue,
			},
		}

		// 解析时间戳
		if ts, err := parseTimestamp(timestamp); err == nil {
			e.Identity.Timestamp = ts
		}

		// 写入新版 events 表
		payloadBytes, err := e.Payload.Value.MarshalMsgpack()
		if err != nil {
			slog.Warn("marshal payload failed, skipping", "id", id, "error", err)
			continue
		}

		_, err = s.db.ExecContext(ctx, `
			INSERT OR IGNORE INTO events (
				id, session_id, type, schema_id, source, timestamp,
				causation_id, correlation_id, origin_id, payload, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			string(e.Identity.ID),
			e.Identity.SessionID,
			string(e.Identity.Type),
			e.Identity.SchemaID,
			string(e.Identity.Source),
			e.Identity.Timestamp.UnixNano(),
			nil, // causation_id
			nil, // correlation_id
			nil, // origin_id
			payloadBytes,
			e.Identity.Timestamp.UnixNano(),
		)
		if err != nil {
			slog.Warn("insert migrated event failed, skipping", "id", id, "error", err)
			continue
		}

		migrated++
	}

	if err := rows.Err(); err != nil {
		return migrated, fmt.Errorf("iterate rows: %w", err)
	}

	slog.Info("migrated legacy events", "count", migrated)
	return migrated, nil
}

// parseTimestamp 解析时间戳字符串
func parseTimestamp(ts string) (time.Time, error) {
	// 尝试多种格式
	formats := []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	}

	for _, format := range formats {
		if t, err := time.Parse(format, ts); err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("unable to parse timestamp: %s", ts)
}
