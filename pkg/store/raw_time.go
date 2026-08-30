package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"gta/pkg/event"
)

// rawTsUserVersion 是 raw_packets.timestamp INTEGER 迁移的 user_version 标记。
// 约定：0 = 未迁移（或空库），1 = 已完成文本 → INTEGER(unix nano) 迁移。
// 后续新增库级迁移时按 2、3… 递增，在 init 中按版本顺序执行。
const rawTsUserVersion = 1

// packetTimeZero 是零值时间戳的存储哨兵。
// time.Time{}.UnixNano() 会得到巨大负数（公元前），既难看又破坏数值排序，
// 故写入侧把零值时间归一为 0，读取侧把 0 还原为零值时间。
const packetTimeZero int64 = 0

// rawTimestamp 存入 raw_packets.timestamp 的 INTEGER 值（unix nano）。
func rawTimestamp(t time.Time) int64 {
	if t.IsZero() {
		return packetTimeZero
	}
	return t.UnixNano()
}

// scanPacketTime 把 raw_packets.timestamp 的取值转换为 time.Time，
// 兼容多种驱动返回形态：
//   - INTEGER（unix nano）：迁移后的新格式，SQL 层可直接聚合/比较；
//   - time.Time：旧文本格式（"2006-01-02 15:04:05.999999999 -0700 MST"）被
//     modernc.org/sqlite 识别并在读取时自动解析为 time.Time（仅历史库出现）；
//   - string / []byte：驱动无法识别的时间文本（异常/手工数据），走 parsePacketTime。
//
// 0（零值哨兵）还原为零值时间；无法解析的文本返回零值时间。
func scanPacketTime(src any) (time.Time, error) {
	switch v := src.(type) {
	case nil:
		return time.Time{}, nil
	case time.Time:
		return v, nil
	case int64:
		return fromRawTimestamp(v), nil
	case int:
		return fromRawTimestamp(int64(v)), nil
	case float64:
		return fromRawTimestamp(int64(v)), nil
	case string:
		return parsePacketTime(v), nil
	case []byte:
		return parsePacketTime(string(v)), nil
	default:
		return time.Time{}, fmt.Errorf("scan raw packet timestamp: unsupported type %T", src)
	}
}

// fromRawTimestamp 把存储的 INTEGER 时间戳还原为 time.Time。
func fromRawTimestamp(ns int64) time.Time {
	if ns == packetTimeZero {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// migrateRawTimestamps 把 raw_packets.timestamp 的文本行迁移为 INTEGER（unix nano）。
// 由 PRAGMA user_version 门控，整体只执行一次；批内 UPDATE 使同一批文本行转为
// INTEGER 后不再命中 WHERE，下一批自然推进（无需 OFFSET，避免 O(n²) 扫描）。
// 解析失败的文本行以 0 落库并告警（保证批处理收敛，值异常但可辨识）。
// 幂等：迁移完成后写入 user_version，重开库不再扫描。
func (s *SQLiteStore) migrateRawTimestamps(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if version >= rawTsUserVersion {
		return nil
	}

	const batchSize = 5000
	migrated := 0
	for {
		rows, err := s.db.QueryContext(ctx,
			"SELECT id, timestamp FROM raw_packets WHERE typeof(timestamp) != 'integer' LIMIT "+strconv.Itoa(batchSize))
		if err != nil {
			return fmt.Errorf("scan legacy raw timestamps: %w", err)
		}
		type legacyRow struct {
			id string
			ts any
		}
		var batch []legacyRow
		for rows.Next() {
			var r legacyRow
			if err := rows.Scan(&r.id, &r.ts); err != nil {
				rows.Close()
				return fmt.Errorf("scan legacy raw timestamp row: %w", err)
			}
			batch = append(batch, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate legacy raw timestamps: %w", err)
		}
		if len(batch) == 0 {
			break
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		stmt, err := tx.PrepareContext(ctx, "UPDATE raw_packets SET timestamp = ? WHERE id = ?")
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		for _, r := range batch {
			t, err := scanPacketTime(r.ts)
			if err != nil || t.IsZero() {
				slog.Warn("migrate raw timestamp: unparseable legacy value, zeroed",
					"row_id", r.id, "value", fmt.Sprintf("%v", r.ts), "error", err)
				t = time.Time{}
			}
			if _, err := stmt.ExecContext(ctx, rawTimestamp(t), r.id); err != nil {
				stmt.Close()
				_ = tx.Rollback()
				return fmt.Errorf("update raw timestamp: %w", err)
			}
			migrated++
		}
		stmt.Close()
		if err := tx.Commit(); err != nil {
			return err
		}
	}

	if migrated > 0 {
		slog.Info("migrated raw packet timestamps to INTEGER", "rows", migrated)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(rawTsUserVersion)); err != nil {
		return fmt.Errorf("set user_version: %w", err)
	}
	return nil
}

// appendRawPacketArgs 组装 AppendRawPackets 单行的绑定参数，
// 供时间戳 INTEGER 化统一走 rawTimestamp（含零值哨兵）。
func appendRawPacketArgs(p event.Packet, id string, connID, metaJSON sql.NullString) []any {
	return []any{
		id,
		rawTimestamp(p.Timestamp),
		p.Src.String(),
		p.Dst.String(),
		p.Protocol,
		p.Raw,
		int32(p.LinkType),
		connID,
		metaJSON,
	}
}
