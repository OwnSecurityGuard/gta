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
// 此方法不经过语义基线解析，before_resolved / after_resolved 均写入 false。
func (s *SQLiteStore) WriteStateChanges(ctx context.Context, sessionID string, events []*event.Event) error {
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

// WriteEnrichedStateChanges 写入经过语义基线解析的 StateChange。
func (s *SQLiteStore) WriteEnrichedStateChanges(ctx context.Context, sessionID string, changes []EnrichedStateChange) error {
	if len(changes) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO state_changes(id, event_id, session_id, flow_id, timestamp, subject_type, subject_id, op, path, before_value, after_value, version, before_resolved, after_resolved, metadata)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	count := 0
	skipped := 0
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
			uuid.NewString(),
			string(esc.EventID),
			sessionID,
			flowID,
			esc.Timestamp.UnixNano(),
			esc.SubjectType,
			esc.SubjectID,
			esc.Op,
			esc.Path,
			string(beforeJSON),
			string(afterJSON),
			esc.Version,
			esc.BeforeResolved,
			esc.AfterResolved,
			string(metaJSON),
		); err != nil {
			return err
		}
		count++
	}
	if count > 0 || skipped > 0 {
		slog.Debug("wrote enriched state changes", "count", count, "skipped", skipped)
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
