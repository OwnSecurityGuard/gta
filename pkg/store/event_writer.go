package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"gta/pkg/event"
)

// AppendEvents 追加 Event 到 events 表
func (s *SQLiteStore) AppendEvents(ctx context.Context, events []*event.Event) error {
	if len(events) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO events (
			id, session_id, type, schema_id, source, timestamp,
			causation_id, correlation_id, origin_id, context, payload, created_at,
			scenario_id, replay_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("prepare stmt: %w", err)
	}
	defer stmt.Close()

	now := time.Now().UnixNano()

	for i, e := range events {
		// 编码 Context 为 MsgPack
		contextBytes, err := e.Context.MarshalMsgpack()
		if err != nil {
			return fmt.Errorf("marshal context for event[%d]: %w", i, err)
		}

		// 编码 Payload 为 MsgPack
		payloadBytes, err := e.Payload.Value.MarshalMsgpack()
		if err != nil {
			return fmt.Errorf("marshal payload for event[%d]: %w", i, err)
		}

		// 转换时间戳为 Unix 纳秒
		timestamp := e.Identity.Timestamp.UnixNano()

		// 处理可选的 Trace 字段
		var causationID, correlationID, originID sql.NullString
		if e.Trace.CausationID != "" {
			causationID = sql.NullString{String: string(e.Trace.CausationID), Valid: true}
		}
		if e.Trace.CorrelationID != "" {
			correlationID = sql.NullString{String: e.Trace.CorrelationID, Valid: true}
		}
		if e.Trace.OriginID != "" {
			originID = sql.NullString{String: string(e.Trace.OriginID), Valid: true}
		}
		var scenarioID, replayID sql.NullString
		if e.Identity.ScenarioID != "" {
			scenarioID = sql.NullString{String: e.Identity.ScenarioID, Valid: true}
		}
		if e.Identity.ReplayID != "" {
			replayID = sql.NullString{String: e.Identity.ReplayID, Valid: true}
		}

		_, err = stmt.ExecContext(ctx,
			string(e.Identity.ID),
			e.Identity.SessionID,
			string(e.Identity.Type),
			e.Identity.SchemaID,
			string(e.Identity.Source),
			timestamp,
			causationID,
			correlationID,
			originID,
			contextBytes,
			payloadBytes,
			now,
			scenarioID,
			replayID,
		)
		if err != nil {
			return fmt.Errorf("insert event[%d]: %w", i, err)
		}
	}

	if err := s.appendEventIndex(ctx, tx, events); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}

	slog.Debug("appended events v2", "count", len(events))
	return nil
}

// appendEventIndex 写入 event_index 投影索引表。
func (s *SQLiteStore) appendEventIndex(ctx context.Context, tx *sql.Tx, events []*event.Event) error {
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO event_index (event_id, session_id, type, timestamp, flow_id, direction, correlation_id, projection_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`)
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

		var flowID, direction, correlationID sql.NullString
		if e.Context.FlowID != "" {
			flowID = sql.NullString{String: e.Context.FlowID, Valid: true}
		}
		if e.Context.Direction != "" {
			direction = sql.NullString{String: e.Context.Direction, Valid: true}
		}
		if e.Trace.CorrelationID != "" {
			correlationID = sql.NullString{String: e.Trace.CorrelationID, Valid: true}
		}

		_, err = stmt.ExecContext(ctx,
			string(e.Identity.ID),
			e.Identity.SessionID,
			string(e.Identity.Type),
			e.Identity.Timestamp.UnixNano(),
			flowID,
			direction,
			correlationID,
			string(projJSON),
		)
		if err != nil {
			return err
		}
	}
	return nil
}
