// Package store 提供事件存储的抽象接口与 SQLite 实现。
//
// 接口按语义分三层：
//   - 事件流（EventWriter/EventReader）：不可变事实，Pipeline 追加，MCP 查询
//   - 投影（ProjectionWriter/ProjectionReader）：派生数据，Pipeline 聚合产生
//   - 控制元数据（SessionStore）：会话生命周期管理
//
// SQLiteStore 同时实现全部接口。未来替换后端时按需实现部分接口。
//
// 接口使用边界：
//   - gta-pipeline 用 EventWriter + ProjectionWriter + SessionStore.Update
//   - gta-mcp 用 EventReader + ProjectionReader + SessionStore
//   - Pipeline 只写不读，MCP 只读不写（SessionStore 双向例外）
package store

import (
	"context"
	"time"

	"gta/pkg/event"
)

// ===== 第 1 层：事件流 =====

// EventWriter 由 gta-pipeline 使用，追加不可变事件。
type EventWriter interface {
	AppendRawPackets(ctx context.Context, packets []event.Packet) error
	AppendEvents(ctx context.Context, events []*event.Event) error
	Flush() error
	Close() error
}

// EventReader 由 gta-mcp 使用，查询事件流。
type EventReader interface {
	QueryEvents(ctx context.Context, sessionID string, limit, offset int) ([]*event.Event, error)
	GetEventByID(ctx context.Context, id string) (*event.Event, error)
	QueryEventsByType(ctx context.Context, sessionID, eventType string, limit, offset int) ([]*event.Event, error)
	QueryEventsByCorrelation(ctx context.Context, correlationID string, limit, offset int) ([]*event.Event, error)
	QueryRawPackets(ctx context.Context, q RawPacketQuery) ([]RawPacketRow, error)
	GetSchema(ctx context.Context, sessionID string) (SchemaInfo, error)
	// RawQuery 逃生舱：临时查询用；不同后端方言可能不兼容。
	RawQuery(ctx context.Context, query string, args ...any) ([]map[string]any, error)
	Close() error
}

// ===== 第 2 层：投影 =====

// ProjectionWriter 由 gta-pipeline 使用，写派生数据。
// 聚合逻辑在 Pipeline 内，Store 只负责存取。
type ProjectionWriter interface {
	WriteMetrics(ctx context.Context, metrics []event.Metric) error
	WriteStateChanges(ctx context.Context, sessionID string, events []*event.Event) error
}

// ProjectionReader 由 gta-mcp 使用，查询派生数据。
type ProjectionReader interface {
	QueryMetrics(ctx context.Context, q MetricQuery) ([]MetricRow, error)
	QueryStateChanges(ctx context.Context, q StateChangeQuery) ([]StateChangeRow, error)
}

// ===== 第 3 层：控制元数据 =====

// SessionStore 管理会话生命周期元数据（control metadata，非事件数据）。
// MCP 创建/查询/删除，Pipeline 更新状态。
type SessionStore interface {
	CreateSession(ctx context.Context, meta SessionMeta) error
	GetSession(ctx context.Context, sessionID string) (*SessionMeta, error)
	ListSessions(ctx context.Context) ([]SessionMeta, error)
	UpdateSession(ctx context.Context, meta SessionMeta) error
	DeleteSession(ctx context.Context, sessionID string) error
}

// ===== 查询参数类型 =====

// RawPacketQuery 查询 raw_packets。
// capture.sqlite 是单 session 数据库，因此不暴露 SessionID 过滤；
// 如需多 session 支持，应先在 raw_packets 表增加 session_id 列。
type RawPacketQuery struct {
	Protocol string
	Src      string
	Dst      string
	Limit    int
	Offset   int
}

// MetricQuery 查询 aggregated_metrics。
type MetricQuery struct {
	SessionID string
	Name      string
	Limit     int
	Offset    int
}

// StateChangeQuery 查询 state_changes 投影表。
type StateChangeQuery struct {
	SessionID   string
	FlowID      string
	SubjectType string
	SubjectID   string
	Op          string
	Path        string
	Limit       int
	Offset      int
}

// ===== 返回行类型 =====

// RawPacketRow 对应 raw_packets 的一行。
type RawPacketRow struct {
	ID        string
	Timestamp time.Time
	Src       string
	Dst       string
	Protocol  string
	Payload   []byte
	LinkType  int
}

// MetricRow 对应 aggregated_metrics 的一行。
type MetricRow struct {
	Name   string
	Window time.Time
	Group  map[string]string
	Value  float64
}

// StateChangeRow 对应 state_changes 的一行。
type StateChangeRow struct {
	ID          string
	EventID     string
	SessionID   string
	FlowID      string
	Timestamp   time.Time
	SubjectType string
	SubjectID   string
	Op          string
	Path        string
	Before      string // JSON 字符串
	After       string // JSON 字符串
	Version     int64
	Metadata    string // JSON 字符串
}

// SchemaInfo 描述事件数据库的表结构（供 MCP get_capture_schema 工具使用）。
type SchemaInfo struct {
	Tables []TableSchema
}

// TableSchema 描述一张表的结构。
type TableSchema struct {
	Name    string
	Columns []ColumnSchema
}

// ColumnSchema 描述一列。
type ColumnSchema struct {
	Name string
	Type string
}

// ===== 控制元数据类型 =====

// SessionMeta 是会话生命周期元数据（control metadata，非事件数据）。
// 持久化在 control.sqlite 的 sessions 表。
type SessionMeta struct {
	SessionID    string         `json:"session_id"`
	StartedAt    time.Time      `json:"started_at"`
	StoppedAt    *time.Time     `json:"stopped_at,omitempty"`
	Status       string         `json:"status"` // "running" | "stopped" | "error"
	Port         int            `json:"port"`
	Plugin       string         `json:"plugin"`
	Interface    string         `json:"interface"`
	PCAPFile     string         `json:"pcap_file,omitempty"`
	RawPackets   int64          `json:"raw_packets,omitempty"`
	Events       int64          `json:"events,omitempty"`
	Metrics      int64          `json:"metrics,omitempty"`
	DecodeErrors int64          `json:"decode_errors,omitempty"`
	DurationSec  float64        `json:"duration_sec,omitempty"`
	DBPath       string         `json:"db_path"` // 该 session 的 capture.sqlite 绝对路径
	Extra        map[string]any `json:"extra,omitempty"`
}
