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

	sdkevidence "github.com/OwnSecurityGuard/gta-plugin-sdk/evidence"
	sdkrule "github.com/OwnSecurityGuard/gta-plugin-sdk/rule"
)

// ─────────────────────────────────────────────────────────────────────────────
// Semantic Layer v1 (contract)
//
// SemanticEvent 描述一个 Event 的"语义身份"——它在协议/业务上意味着什么。
// 它是对 Event 的投影（projection），不是 Event 本身。Event 保持 immutable 事实层不变。
// Semantic Contract 只规定 Agent/UI 如何理解 Event 的语义身份，不规定任何业务 Schema
// （Player/Inventory/HP/Gold/Quest 等均属于具体领域，不在本契约内）。
// ─────────────────────────────────────────────────────────────────────────────

// SemanticContractVersion 是 Semantic/Evidence Contract 的版本标识。
// 所有 v1 类型定义与 JSON 结构以此为稳定边界；任何破坏性变更将进入 v2。
const SemanticContractVersion = "v1"

// Direction 表示事件/语义事件的网络方向。
// 直接复用 Event 已有的网络上下文语义（Context.Direction），不另起一套。
const (
	DirectionClientToServer string = "client_to_server"
	DirectionServerToClient string = "server_to_client"
	DirectionUnknown        string = "unknown"
)

// SemanticKind 是语义事件种类。v1 固定为以下枚举，不再扩充。
type SemanticKind string

const (
	// SemanticMessage 仅有消息身份，尚未进一步判断语义。
	// 例如无法判定请求/响应的心跳包，或仅透传的 CS_Login。
	SemanticMessage SemanticKind = "message"
	// SemanticRequest 客户端发起的请求。
	SemanticRequest SemanticKind = "request"
	// SemanticResponse 对请求的响应。
	SemanticResponse SemanticKind = "response"
	// SemanticPush 服务端主动推送（非请求-响应模型）。
	SemanticPush SemanticKind = "push"
	// SemanticStateChange 实体状态变更。
	SemanticStateChange SemanticKind = "state_change"
	// SemanticTransaction 时间聚类产生的逻辑事务组。
	SemanticTransaction SemanticKind = "transaction"
)

// SemanticSubject 是事件作用的业务实体引用。
// v1 不规定 Type 的具体含义（player/guild/item/npc/match/session 等由具体领域决定）。
type SemanticSubject struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// SemanticSource 标识语义判定的来源，供 Agent 区分"事实"与"推断"。
//
// v1 实际产生者（Actual Producer）：
//
//	engine  ← 当前唯一实际产生 SemanticEvent 的来源（Phase 2 投影器固定为 engine）
//
// 以下为保留（reserved / future），v1 不实际产生，仅供契约前向兼容与未来扩展：
//
//	plugin  ← reserved（未来：插件显式声明语义，需引入明确标记如 _meta.semantic_source）
//	rule    ← reserved（未来：显式规则映射）
//	user    ← reserved（未来：用户/外部标注）
//
// 文档 SSOT（docs/semantic-evidence-v1.md §2.7）对四者的角色有完整说明；
// 任何"实际运行数据"示例的 source 均为 "engine"，AI 不应据此推断 v1 会输出 plugin/rule/user。
type SemanticSource string

const (
	// SourcePlugin 由解码插件明确告知 GTA（如 decoder 声明 operation=login）。
	// ⚠️ reserved / future：v1 实际不产生此来源（见类型注释）。
	SourcePlugin SemanticSource = "plugin"
	// SourceRule 由显式规则映射得到（如 name→operation 规则）。
	// ⚠️ reserved / future：v1 实际不产生此来源（见类型注释）。
	SourceRule SemanticSource = "rule"
	// SourceEngine 由语义引擎推导得到。当前 v1 唯一实际产生者（Phase 2 投影器固定）。
	SourceEngine SemanticSource = "engine"
	// SourceUser 由用户/外部标注。
	// ⚠️ reserved / future：v1 实际不产生此来源（见类型注释）。
	SourceUser SemanticSource = "user"
)

// SemanticEvent 是 v1 Semantic Contract 的核心类型。
//
// 它回答"这个 Event 在协议/业务上意味着什么"，而非"发生了什么"（那是 Event 的职责）。
// Confidence 统一取值 [0,1]，但仅表示判定可信度，不表示证据强度（见 EvidenceStrength）。
// 猜不到就不填：Operation 允许为空字符串，绝不为"完整"而强行推断。
type SemanticEvent struct {
	EventID   event.EventID `json:"event_id"`
	SessionID string        `json:"session_id"`
	FlowID    string        `json:"flow_id,omitempty"`

	Kind      SemanticKind `json:"kind"`
	Name      string       `json:"name,omitempty"`
	Operation string       `json:"operation,omitempty"`

	Direction string `json:"direction,omitempty"`

	Subject *SemanticSubject `json:"subject,omitempty"`

	Confidence float64 `json:"confidence"`

	Source SemanticSource `json:"source"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Evidence Layer v1 (contract)
//
// Evidence 回答"为什么认为两个 Event 有关系"。它是语义层的分析产物，不修改原始 Event。
// 关键设计：Confidence（判定可信度）与 Strength（证据强度：observed/derived/inferred）
// 必须分开；每条边必须可定位到 method + rule_id + evidence_ids，保证可解释。
// ─────────────────────────────────────────────────────────────────────────────

// EvidenceStrength 区分证据强度，与 Confidence 独立。
type EvidenceStrength string

const (
	// EvidenceObserved 直接事实。例如 event A decoded_from raw packet B。
	EvidenceObserved EvidenceStrength = "observed"
	// EvidenceDerived 由确定规则得到。例如同一 correlation_id 关联两事件。
	EvidenceDerived EvidenceStrength = "derived"
	// EvidenceInferred 推测。例如仅凭名字与时间推断 LoginReq → LoginResp。
	EvidenceInferred EvidenceStrength = "inferred"
)

// EvidenceMethod 标识建立关系所使用的方法。
type EvidenceMethod string

const (
	MethodPlugin          EvidenceMethod = "plugin"
	MethodCorrelation     EvidenceMethod = "correlation"
	MethodNamePattern     EvidenceMethod = "name_pattern"
	MethodTemporal        EvidenceMethod = "temporal"
	MethodStateProjection EvidenceMethod = "state_projection"
	MethodTransaction     EvidenceMethod = "transaction"
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
	//
	// ⚠️ v1 标记为 experimental：属于更细的语义关系，容易与业务领域绑定，
	// v1 不将其标准化。当前实现继续保留，但外部消费者不应依赖其稳定性。
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

	// EventID 是节点对应的事件 ID；仅 event 类节点有效（raw_packet/entity/transaction 等可能为空）。
	EventID event.EventID `json:"event_id,omitempty"`
	// SessionID 是会话标识，用于隔离不同会话/连接的节点。
	SessionID string `json:"session_id"`
	// FlowID 是五元组流标识；仅对包/事件节点有效。
	FlowID string `json:"flow_id,omitempty"`

	// Semantic 是该节点对应 Event 的语义投影；仅在存在语义判定时填充。
	Semantic *SemanticEvent `json:"semantic,omitempty"`

	// Timestamp 是节点时间戳。
	Timestamp time.Time `json:"timestamp"`
	// Labels 是节点标签，供展示/过滤使用。
	//
	// ⚠️ DEPRECATED（v1 契约外）：v1 对外输出已不再包含 labels（见 MCP v1 转换层
	// v1EvidenceNodeEntry）。保留该字段仅为“内部兼容读取”旧数据；后续 v2 / migration
	// 将彻底删除。Plugin / Agent / UI 不应依赖其存在或内容。
	Labels map[string]string `json:"labels,omitempty"`
	// Properties 是节点附加属性（自由 map[string]any）。
	//
	// ⚠️ DEPRECATED（v1 契约外）：v1 已由 EvidenceStrength / Method / RuleID / Reason /
	// EvidenceIDs / Semantic 等结构化字段承载全部可解释性需求；自由 Properties 最终会
	// 重新变成“大家各自塞字段”的逃生通道，故 v1 对外输出已不再包含它。
	// 保留该字段仅为“内部兼容读取”旧数据；后续 v2 / migration 将彻底删除。
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
	// Type 是固定边类型（v1 稳定集合见 RelationType 常量）。
	Type RelationType `json:"type"`

	// Confidence 是判定可信度，统一取值 [0.0, 1.0]。
	// 注意：它只表示"判定有多可信"，不表示证据强度（见 Strength）。
	Confidence float64 `json:"confidence"`

	// Strength 是证据强度，与 Confidence 独立：
	// observed=直接事实，derived=确定规则得到，inferred=推测。
	Strength EvidenceStrength `json:"strength"`
	// Method 是建立该关系使用的方法（plugin/correlation/name_pattern/...）。
	Method EvidenceMethod `json:"method"`
	// RuleID 是产生该关系的规则 ID，与 Plugin Contract 的 rule 体系贯通，
	// 使 Agent 可 rule_id → explain → documentation 追溯。
	RuleID string `json:"rule_id,omitempty"`
	// Reason 是建立该关系的证据说明（人类可读，需说明依据而非"matched by name"）。
	Reason string `json:"reason,omitempty"`
	// EvidenceIDs 是支撑该关系的底层 Evidence 引用（如参与的 event ID 列表）。
	//
	// 完整性约束：每个 EvidenceID 都必须能在 Graph 中解析到真实存在的 Event 节点 /
	// 原始包节点 / 节点 ID（见 Engine.addEdgeFromNode 第二层不变量），否则该边不生成。
	EvidenceIDs []string `json:"evidence_ids,omitempty"`

	// Properties 是边附加属性（自由 map[string]any）。
	//
	// ⚠️ DEPRECATED（v1 契约外）：v1 对外输出已不再包含它（见 MCP v1 转换层
	// v1EvidenceEdgeEntry）。保留该字段仅为“内部兼容读取”旧数据；后续 v2 / migration
	// 将彻底删除。Plugin / Agent / UI 不应依赖其存在或内容。
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

// ─────────────────────────────────────────────────────────────────────────────
// SDK Evidence / Rule 类型桥接 — Semantic Contract v1
//
// 以下类型别名将 SDK 的 evidence 与 rule 包类型桥接到 gta 内部语义分析引擎，
// 使 MCP 查询层、证据图存储层与插件证据产出层共用同一套契约定义。
// ─────────────────────────────────────────────────────────────────────────────

// SDKEvidence 是 SDK evidence.Evidence 的类型别名，表示一条领域证据主张。
type SDKEvidence = sdkevidence.Evidence

// SDKEvidenceKind 是证据性质（observation/derivation/assessment）。
type SDKEvidenceKind = sdkevidence.Kind

// SDKEvidenceSourceKind 是证据来源种类（raw_packet/event/state_change/external）。
type SDKEvidenceSourceKind = sdkevidence.SourceKind

// SDKEvidenceRef 引用一个证据来源。
type SDKEvidenceRef = sdkevidence.Ref

// SDKEvidenceMethod 是证据产生方法（decode/correlate/threshold/model/manual）。
type SDKEvidenceMethod = sdkevidence.Method

// SDKEvidenceRelation 是证据图边关系（produces/changes/supports/contradicts/refines/precedes/references）。
type SDKEvidenceRelation = sdkevidence.Relation

// SDKEvidenceEdge 是证据图中的一条边。
type SDKEvidenceEdge = sdkevidence.Edge

// SDKEvidenceGraph 是证据图（节点 + 边）。
type SDKEvidenceGraph = sdkevidence.Graph

// SDKRule 是 SDK rule.Rule 的类型别名。
type SDKRule = sdkrule.Rule

// SDKRuleSeverity 是规则严重级别。
type SDKRuleSeverity = sdkrule.Severity

// SDKRuleLayer 是规则所属的语义契约层。
type SDKRuleLayer = sdkrule.Layer

// SDKRuleRegistry 是规则注册表。
type SDKRuleRegistry = sdkrule.Registry

// EvidenceStrengthToSDK 将内部 EvidenceStrength 映射到 SDK evidence 的强度概念。
// 注意：内部 EvidenceStrength (observed/derived/inferred) 描述的是证据可靠性层次，
// 而 SDK 的 evidence.Kind (observation/derivation/assessment) 描述的是证据性质。
// 此函数提供语义映射，用于 MCP 输出层转换。
func EvidenceStrengthToSDK(s EvidenceStrength) SDKEvidenceKind {
	switch s {
	case EvidenceObserved:
		return sdkevidence.KindObservation
	case EvidenceDerived:
		return sdkevidence.KindDerivation
	case EvidenceInferred:
		return sdkevidence.KindDerivation // inferred → derivation (SDK 无 inferred kind)
	default:
		return sdkevidence.KindObservation
	}
}

// EvidenceMethodToSDK 将内部 EvidenceMethod 映射到 SDK evidence.Method。
func EvidenceMethodToSDK(m EvidenceMethod) SDKEvidenceMethod {
	switch m {
	case MethodPlugin:
		return sdkevidence.MethodDecode
	case MethodCorrelation:
		return sdkevidence.MethodCorrelate
	case MethodTemporal:
		return sdkevidence.MethodThreshold
	case MethodStateProjection:
		return sdkevidence.MethodModel
	default:
		return sdkevidence.MethodCorrelate
	}
}

// RelationTypeToSDK 将内部 RelationType 映射到 SDK evidence.Relation。
func RelationTypeToSDK(r RelationType) SDKEvidenceRelation {
	switch r {
	case DecodedFrom:
		return sdkevidence.RelProduces
	case ResponseTo:
		return sdkevidence.RelPrecedes
	case CausedBy:
		return sdkevidence.RelSupports
	case CorrelatedWith:
		return sdkevidence.RelReferences
	case Updates:
		return sdkevidence.RelChanges
	case Contains:
		return sdkevidence.RelRefines
	default:
		return sdkevidence.RelReferences
	}
}
