// Package semantic 实现链路规则语义分析引擎。
//
// 核心能力：
//   - 按实体键和版本维护基线，生成带 before/after 的 StateChange
//   - 根据事件上下文与关联键建立高置信/低置信关系
//   - 输出统一证据图模型（节点 + 固定边类型）
//
// 设计约束：
//   - 不修改原始事件；语义关系是分析产物
//   - 无基线时明确标记，不伪造旧值
//   - 默认事件独立；只有强证据时才建立 caused_by/response_to
package semantic

import (
	"time"

	"gta/pkg/event"
)

// RelationType 是证据图中固定边类型。
type RelationType string

const (
	// DecodedFrom 表示解码结果来自某个原始包。
	DecodedFrom RelationType = "decoded_from"
	// Contains 表示批量/容器消息包含逻辑消息。
	Contains RelationType = "contains"
	// ResponseTo 表示 RPC 响应对应请求。
	ResponseTo RelationType = "response_to"
	// CausedBy 表示高置信因果关系。
	CausedBy RelationType = "caused_by"
	// CorrelatedWith 表示统计或规则相关性。
	CorrelatedWith RelationType = "correlated_with"
	// Updates 表示增量更新实体字段。
	Updates RelationType = "updates"
	// ParameterFrom 表示参数来源。
	ParameterFrom RelationType = "parameter_from"
	// PossibleFollowup 表示低置信时间邻近关系，避免误导为真实因果。
	PossibleFollowup RelationType = "possible_followup"
)

// EvidenceNodeKind 是证据图节点类型。
type EvidenceNodeKind string

const (
	// NodeRawPacket 原始抓包。
	NodeRawPacket EvidenceNodeKind = "raw_packet"
	// NodeEvent 解码后的事件。
	NodeEvent EvidenceNodeKind = "event"
	// NodeEntity 业务实体（由 StateChange 聚合而成）。
	NodeEntity EvidenceNodeKind = "entity"
	// NodeStateChange 单个状态变更投影。
	NodeStateChange EvidenceNodeKind = "state_change"
	// NodeTransaction 时间聚类产生的事务组。
	NodeTransaction EvidenceNodeKind = "transaction"
)

// EvidenceNode 是证据图中的一个节点。
type EvidenceNode struct {
	// ID 是节点全局唯一标识。
	ID string `json:"id"`
	// Kind 是节点类型。
	Kind EvidenceNodeKind `json:"kind"`
	// SessionID 是会话标识，用于隔离不同会话/连接的节点。
	SessionID string `json:"session_id"`
	// FlowID 是五元组流标识；仅对包/事件节点有效。
	FlowID string `json:"flow_id,omitempty"`
	// Timestamp 是节点时间戳。
	Timestamp time.Time `json:"timestamp"`
	// Labels 是节点标签，供展示/过滤使用。
	Labels map[string]string `json:"labels,omitempty"`
	// Properties 是节点附加属性。
	Properties map[string]any `json:"properties,omitempty"`
}

// EvidenceEdge 是证据图中的一条有向边。
type EvidenceEdge struct {
	// ID 是边全局唯一标识。
	ID string `json:"id"`
	// Source 是源节点 ID。
	Source string `json:"source"`
	// Target 是目标节点 ID。
	Target string `json:"target"`
	// Type 是固定边类型。
	Type RelationType `json:"type"`
	// Confidence 是置信度 [0.0, 1.0]；高置信关系（如 response_to）为 1.0。
	Confidence float64 `json:"confidence"`
	// Reason 是建立该关系的证据说明。
	Reason string `json:"reason,omitempty"`
	// Properties 是边附加属性。
	Properties map[string]any `json:"properties,omitempty"`
}

// EvidenceGraph 是统一图模型输出。
type EvidenceGraph struct {
	// Nodes 是图节点集合。
	Nodes []EvidenceNode `json:"nodes"`
	// Edges 是图边集合。
	Edges []EvidenceEdge `json:"edges"`
	// Uncertainties 是无法建立强关系时的不确定性说明。
	Uncertainties []string `json:"uncertainties,omitempty"`
}

// EntityKey 唯一标识一个业务实体实例。
// 必须按 (session_id, flow_id, subject_type, subject_id) 隔离，
// 不能跨玩家或跨连接复用。
type EntityKey struct {
	SessionID   string
	FlowID      string
	SubjectType string
	SubjectID   string
}

// EntityBaseline 是某个实体在某一时刻的完整状态快照。
type EntityBaseline struct {
	Key        EntityKey
	Version    int64
	State      map[string]event.Value
	FirstSeen  time.Time
	LastSeen   time.Time
	HasHistory bool
}

// EnrichedStateChange 是在 StateChange 基础上补充了 before/after 与
// 基线状态的分析产物。
type EnrichedStateChange struct {
	event.StateChange

	// EventID 是产生该变更的事件 ID。
	EventID event.EventID `json:"event_id"`
	// FlowID 是事件所属的流/上下文 ID，用于投影查询。
	FlowID string `json:"flow_id,omitempty"`
	// Timestamp 是变更发生时间。
	Timestamp time.Time `json:"timestamp"`
	// BeforeResolved 表示 Before 是否来自真实基线。
	// false 表示无基线，Before 为空（未伪造旧值）。
	BeforeResolved bool `json:"before_resolved"`
	// AfterResolved 表示 After 是否已写入基线。
	AfterResolved bool `json:"after_resolved"`
	// EntityVersion 是变更后的实体版本。
	EntityVersion int64 `json:"entity_version"`
}

// Config 是语义引擎配置。
// Config 是语义分析引擎的配置。
type Config struct {
	// PossibleFollowupWindow 是判定 PossibleFollowup 的最大时间间隔。
	PossibleFollowupWindow time.Duration
	// MaxPendingResponses 是等待 response_to 匹配的请求缓存大小。
	MaxPendingResponses int
	// ResponseNamePatterns 是请求-响应命名模式，用于无 correlation_key 时通过命名推断。
	ResponseNamePatterns []RespNamePattern
	// TransactionClustering 控制事务级时间聚类；nil/disabled 则跳过聚类。
	TransactionClustering *TransactionClusterConfig
}

// TransactionClusterConfig 控制按时间窗口将事件聚合成逻辑事务组。
type TransactionClusterConfig struct {
	// NewTransactionOnRequest 控制每个 client_to_server 事件是否开启新事务。
	NewTransactionOnRequest bool
	// MergeGap 是合并两个相邻请求事务的最大间隔；小于此间隔则合并为同一事务。
	MergeGap time.Duration
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		PossibleFollowupWindow: 5 * time.Second,
		MaxPendingResponses:    4096,
		ResponseNamePatterns:   DefaultNamePatterns(),
	}
}

// RespNamePattern 描述一对请求-响应消息命名模式。
// 当协议未设置 correlation_key 时，引擎尝试通过命名变换推断请求-响应对。
type RespNamePattern struct {
	// RequestSuffix 是请求消息的命名后缀（如 "Req"、"Request"）。
	RequestSuffix string
	// ResponseSuffix 是响应消息的命名后缀（如 "Resp"、"Response"）。
	ResponseSuffix string
}

// DefaultNamePatterns 返回内置的常见请求-响应命名模式。
func DefaultNamePatterns() []RespNamePattern {
	return []RespNamePattern{
		{RequestSuffix: "Req", ResponseSuffix: "Resp"},
		{RequestSuffix: "Request", ResponseSuffix: "Response"},
		{RequestSuffix: "Req", ResponseSuffix: "Rsp"},
		{RequestSuffix: "C2S", ResponseSuffix: "S2C"},
		{RequestSuffix: "CS", ResponseSuffix: "SC"},
	}
}
