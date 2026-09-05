package store

import (
	"context"
	"fmt"

	"gametrace/pkg/event"
)

// EventPageQuery 是事件分页查询（时间倒序）条件，
// 供 list_decoded_data 的 SQL 下推路径使用。
type EventPageQuery struct {
	SessionID string
	// TypeEq 是 type 等值下推（非空时仅返回该类型）。与 TypeNot 互斥；
	// 两者同时设置时 TypeEq 优先（分类器不会产生该组合）。
	TypeEq string
	// TypeNot 是 type 不等下推（非空时排除该类型）。
	TypeNot string
}

// QueryEventPage 按 timestamp DESC 分页返回事件与 SQL 条件命中总数。
// 与 QueryEventsDesc(sessionID, 0, 0) + 应用层切片的差别：
//   - LIMIT/OFFSET 在 SQL 层完成，payload msgpack 仅对页内行解码；
//   - total 来自同条件 COUNT(*)，无需全量物化。
//
// limit 必须 > 0；offset 允许超出总数（返回空页 + 精确 total）。
func (s *SQLiteStore) QueryEventPage(ctx context.Context, q EventPageQuery, limit, offset int) ([]*event.Event, int, error) {
	if limit <= 0 {
		return nil, 0, fmt.Errorf("query event page: limit must be > 0")
	}
	where, args := eventPageWhere(q)

	var total int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM events "+where, args...,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count events: %w", err)
	}

	pageQuery := `SELECT id, session_id, type, schema_id, source, timestamp,
	       causation_id, correlation_id, origin_id, context, payload` + s.eventSelectSuffix() + `
FROM events ` + where + `
ORDER BY timestamp DESC LIMIT ? OFFSET ?`
	pageArgs := append(append([]any{}, args...), limit, offset)

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

// StreamEventsDesc 以时间倒序流式遍历满足条件的事件，每累积 batch 条解码后
// 调用 yield；yield 返回 false 时提前终止。
// 用于应用层表达式过滤：逐批求值、逐批释放，内存 O(batch) 而非 O(全量)，
// 代价与全量路径相同的 CPU（精确 total 要求遍历完所有候选行）。
func (s *SQLiteStore) StreamEventsDesc(ctx context.Context, q EventPageQuery, batch int, yield func([]*event.Event) (bool, error)) error {
	if batch <= 0 {
		batch = 500
	}
	where, args := eventPageWhere(q)
	streamQuery := `SELECT id, session_id, type, schema_id, source, timestamp,
	       causation_id, correlation_id, origin_id, context, payload` + s.eventSelectSuffix() + `
FROM events ` + where + `
ORDER BY timestamp DESC`

	rows, err := s.db.QueryContext(ctx, streamQuery, args...)
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

// eventPageWhere 组装事件分页查询的 WHERE 片段与绑定参数。
func eventPageWhere(q EventPageQuery) (string, []any) {
	where := "WHERE session_id = ?"
	args := []any{q.SessionID}
	if q.TypeEq != "" {
		where += " AND type = ?"
		args = append(args, q.TypeEq)
	} else if q.TypeNot != "" {
		where += " AND type != ?"
		args = append(args, q.TypeNot)
	}
	return where, args
}
