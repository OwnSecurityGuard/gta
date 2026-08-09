package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"

	"gta/pkg/event"

	"github.com/google/uuid"
)

// WriteStateChanges 从 Event 批量写入 StateChange 到 state_changes 表。
func (s *SQLiteStore) WriteStateChanges(ctx context.Context, sessionID string, events []*event.Event) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO state_changes(id, event_id, session_id, flow_id, timestamp, subject_type, subject_id, op, path, before_value, after_value, version, metadata)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	count := 0
	skipped := 0
	for _, ev := range events {
		for _, sc := range ev.ExtractStateChanges() {
			if err := sc.Validate(); err != nil {
				slog.Warn("skip invalid state change", "event_id", ev.Identity.ID, "error", err)
				skipped++
				continue
			}
			beforeJSON, _ := json.Marshal(sc.Before.ToAny())
			afterJSON, _ := json.Marshal(sc.After.ToAny())
			metaJSON, _ := json.Marshal(sc.Metadata.ToAny())

			// flow_id 优先来自 EventContext，其次回退到 payload 中的 _meta.flow_id / flow_id，
			// 保证离线解码或手动构造的事件也能被 state_changes 查询命中。
			flowIDStr := extractFlowIDFromEvent(ev)
			var flowID sql.NullString
			if flowIDStr != "" {
				flowID = sql.NullString{String: flowIDStr, Valid: true}
			}

			if _, err := stmt.ExecContext(ctx,
				uuid.NewString(),
				string(ev.Identity.ID),
				sessionID,
				flowID,
				ev.Identity.Timestamp.UnixNano(),
				sc.SubjectType,
				sc.SubjectID,
				sc.Op,
				sc.Path,
				string(beforeJSON),
				string(afterJSON),
				sc.Version,
				string(metaJSON),
			); err != nil {
				return err
			}
			count++
		}
	}
	if count > 0 || skipped > 0 {
		slog.Debug("wrote state changes", "count", count, "skipped", skipped)
	}
	return tx.Commit()
}

// extractFlowIDFromEvent 从 Event 提取 flow_id：优先 Context，其次 _meta，最后顶层 payload。
func extractFlowIDFromEvent(ev *event.Event) string {
	if ev == nil {
		return ""
	}
	if ev.Context.FlowID != "" {
		return ev.Context.FlowID
	}
	obj, ok := ev.Payload.Value.AsObject()
	if !ok {
		return ""
	}
	// _meta.flow_id
	if meta, ok := obj["_meta"]; ok {
		if metaObj, ok := meta.AsObject(); ok {
			if v, ok := metaObj["flow_id"]; ok {
				if s, ok := v.AsString(); ok {
					return s
				}
			}
		}
	}
	// 顶层 flow_id
	if v, ok := obj["flow_id"]; ok {
		if s, ok := v.AsString(); ok {
			return s
		}
	}
	return ""
}
