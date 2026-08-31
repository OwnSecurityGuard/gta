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
	// QueryEventsDesc 时间倒序版本，供展示层"最新在前"。
	QueryEventsDesc(ctx context.Context, sessionID string, limit, offset int) ([]*event.Event, error)
	GetEventByID(ctx context.Context, id string) (*event.Event, error)
	QueryEventsByType(ctx context.Context, sessionID, eventType string, limit, offset int) ([]*event.Event, error)
	QueryEventsByCorrelation(ctx context.Context, correlationID string, limit, offset int) ([]*event.Event, error)
	QueryRawPackets(ctx context.Context, q RawPacketQuery) ([]RawPacketRow, error)
	GetSchema(ctx context.Context, sessionID string) (SchemaInfo, error)
	// RawQuery 逃生舱：临时查询用；不同后端方言可能不兼容。
	RawQuery(ctx context.Context, query string, args ...any) ([]map[string]any, error)
	Close() error
}

// EventPager 是展示层分页查询扩展：SQL 层完成 LIMIT/OFFSET/COUNT 与
// 可下推的结构化谓词（type 过滤），payload 仅对页内行解码。
// 由 list_decoded_data 等高频轮询路径使用，避免全量加载。
type EventPager interface {
	// QueryEventPage 返回按 timestamp DESC 的一页事件与 SQL 条件命中总数。
	QueryEventPage(ctx context.Context, q EventPageQuery, limit, offset int) ([]*event.Event, int, error)
	// StreamEventsDesc 以时间倒序分批流式遍历事件，供应用层表达式过滤
	//（内存 O(batch)，精确 total 需遍历全部候选行）。
	StreamEventsDesc(ctx context.Context, q EventPageQuery, batch int, yield func([]*event.Event) (bool, error)) error
}

// ===== 第 2 层：投影 =====

// ProjectionWriter 由 gta-pipeline 使用，写派生数据。
// 聚合逻辑在 Pipeline 内，Store 只负责存取。
type ProjectionWriter interface {
	WriteMetrics(ctx context.Context, metrics []event.Metric) error
	WriteStateChanges(ctx context.Context, sessionID string, events []*event.Event) error
	WriteEnrichedStateChanges(ctx context.Context, sessionID string, changes []EnrichedStateChange) error
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
	// GetSessionFor 是 owner 感知的 GetSession：按 SessionOwnerFilter 过滤，
	// 目标会话不属于该 owner（且未授予 AllOwners）时返回 not found。
	GetSessionFor(ctx context.Context, sessionID string, f SessionOwnerFilter) (*SessionMeta, error)
	ListSessions(ctx context.Context) ([]SessionMeta, error)
	// ListSessionsFor 是 owner 感知的 ListSessions：只返回该 owner 的会话。
	ListSessionsFor(ctx context.Context, f SessionOwnerFilter) ([]SessionMeta, error)
	UpdateSession(ctx context.Context, meta SessionMeta) error
	DeleteSession(ctx context.Context, sessionID string) error
}

// SessionOwnerFilter 描述会话查询的 owner 可见性。
//
// Owner 为空串表示匿名（本地单机用法）：只看 owner='' 的会话。
// AllOwners 为 true（admin）时不过滤，可看到所有 owner 的会话。
// 兼容约定：无过滤（ListSessions/GetSession）等价于 AllOwners=true，
// 保证既有调用方行为完全不变。
type SessionOwnerFilter struct {
	Owner     string
	AllOwners bool
}

// Matches 判断 meta 是否对该过滤器可见。
func (f SessionOwnerFilter) Matches(meta SessionMeta) bool {
	return f.AllOwners || meta.Owner == f.Owner
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

// StateChangeRow 对应 state_changes 表的一行。
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

// EnrichedStateChange 是带基线解析标记的 StateChange。
// Store 层独立定义，避免依赖 analyze/semantic 包。
type EnrichedStateChange struct {
	event.StateChange

	// EventID 是产生该变更的事件 ID。
	EventID event.EventID `json:"event_id"`
	// FlowID 是事件所属的流/上下文 ID，用于投影查询。
	FlowID string `json:"flow_id,omitempty"`
	// Timestamp 是变更发生时间。
	Timestamp time.Time `json:"timestamp"`
	// BeforeResolved 表示 Before 是否来自真实基线；false 表示无基线。
	BeforeResolved bool `json:"before_resolved"`
	// AfterResolved 表示 After 是否已写入基线。
	AfterResolved bool `json:"after_resolved"`
	// EntityVersion 是变更后的实体版本。
	EntityVersion int64 `json:"entity_version"`
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
	// Owner 是会话归属者（pkg/auth 的 Principal.Owner）。
	// 空串表示匿名（本地单机用法）。一等字段，持久化到 sessions.owner 列。
	Owner        string         `json:"owner,omitempty"`
	// ProjectID 是会话所属的项目（projects.id）。空串表示未归属任何项目。
	// 一等字段，持久化到 sessions.project_id 列。
	ProjectID    string         `json:"project_id,omitempty"`
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

	// ManifestSnapshot 是会话创建时的插件 manifest 快照（plugin.yaml 原文）。
	// 用于 MCP 层查询插件声明的 Schema/State 契约声明，
	// 使 Agent 无需连接插件即可了解会话的契约能力。
	// 空字符串表示会话创建时未获取到 manifest（如插件未注册）。
	ManifestSnapshot string `json:"manifest_snapshot,omitempty"`
}
