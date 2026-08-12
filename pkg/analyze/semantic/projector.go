package semantic

import (
	"strings"

	"gta/pkg/event"
)

// ─────────────────────────────────────────────────────────────────────────────
// Phase 2 — Semantic 投影（确定性，纯函数）
//
// 职责边界（硬约束，违反即 bug）：
//   1. 纯函数投影：不写 DB、不修改 Event/Payload/Context、不更新 Evidence Graph、
//      不调用外部服务、不依赖时间。同一 Event → 永远得到同一 SemanticEvent。
//   2. 只做"确定性语义投影"：FlowID/Name/Direction/Kind 仅取自确定性字段。
//      禁止 response_to 推理、login/player 自动识别、operation 模糊推测、
//      confidence 推断、Evidence Edge、transaction 关系聚类。
//   3. operation 宁可为空：无确定性规则就返回 ""，绝不猜测。
//   4. confidence 严格限定：Phase 2 只产生 confidence = 1.0（事实/确定性投影）。
//      0.82 这类"推断值"属于 Phase 3，绝不允许在此出现。
//   5. 三层职责干净：Phase 2 = 事实/确定性投影（confidence=1）；
//      Phase 3 = 关系推断（confidence 0~1 + Strength）。
// ─────────────────────────────────────────────────────────────────────────────

// Projector 将 Event 投影为 SemanticEvent。
// 实现必须是纯函数（见上方硬约束）。
type Projector interface {
	Project(e *event.Event) SemanticEvent
}

// SemanticProjector 是 Projector 的默认确定性实现。
// 无状态、无配置、可并发调用。
type SemanticProjector struct{}

// NewSemanticProjector 创建默认投影器。
func NewSemanticProjector() *SemanticProjector {
	return &SemanticProjector{}
}

// Project 把 Event 投影为 SemanticEvent。
//
// 行为保证：
//   - 零副作用：不读取/修改任何外部状态，不修改入参 e。
//   - 确定性：相同入参永远产出相同结果（可在测试中验证）。
//   - 不猜测：缺字段则填空/默认中性值，绝不臆造语义。
func (p *SemanticProjector) Project(e *event.Event) SemanticEvent {
	// 防御性 nil：保证纯函数对任意输入都有确定结果。
	if e == nil {
		return SemanticEvent{Kind: SemanticMessage, Confidence: 1.0, Source: SourceEngine}
	}

	se := SemanticEvent{
		EventID:   e.Identity.ID,
		SessionID: e.Identity.SessionID,
		// FlowID 直接来自网络上下文（确定性事实）。
		FlowID: e.Context.FlowID,
		// Kind 来自 _meta.kind 或 Identity.Type 的确定性映射；缺省为中性 message。
		Kind: deriveKind(e),
		// Name 来自插件显式声明的 _meta.msg_name；缺省为空。
		Name: metaString(e, "msg_name"),
		// Direction 优先 _meta.direction（插件显式声明），回退到网络上下文 Context.Direction。
		Direction: deriveDirection(e),
		// Operation 在 Phase 2 一律为空：无确定性规则，绝不猜测（硬约束 3）。
		Operation: p.resolveOperation(e),
		// Subject 在 Phase 2 一律为 nil：不做 player/entity 自动识别（硬约束 2）。
		Subject: nil,
		// Confidence 固定 1.0：Phase 2 只做事实/确定性投影（硬约束 4）。
		Confidence: 1.0,
		// Source 在 Phase 2 固定为 engine：投影本身是 Engine 的确定性投影。
		// 未来若需表达"插件显式声明语义（如 semantic.kind/operation）"，
		// 应引入明确来源标记（如 _meta.semantic_source="plugin" 或独立字段），
		// 而不是用 _meta 是否存在来推导（见 deriveSource）。
		Source: deriveSource(e),
	}
	return se
}

// resolveOperation 解析规范化业务操作名。
//
// Phase 2 显式返回 ""：任何确定性规则表都尚未建立，猜测会污染下游 Evidence。
// 后续若引入"msg_name → operation"的显式映射表（如 C2S_Login → login），
// 必须在此以纯查表方式实现，且只对命中确定规则的输入填值，其余保持空。
func (p *SemanticProjector) resolveOperation(_ *event.Event) string {
	return ""
}

// deriveDirection 确定语义方向。
// 优先插件显式声明的 _meta.direction；缺失时回退到网络上下文 Context.Direction；
// 两者皆空则 unknown。全程为纯读取，不构成推断。
func deriveDirection(e *event.Event) string {
	if v, ok := e.MetaValue("direction"); ok {
		if s, ok := v.AsString(); ok && s != "" {
			return s
		}
	}
	if e.Context.Direction != "" {
		return e.Context.Direction
	}
	return DirectionUnknown
}

// deriveKind 确定性派生语义种类。
// 顺序：_meta.kind（插件显式声明，须为合法枚举）→ Identity.Type 后缀匹配 → 中性 message。
// 绝不猜测：未命中任何确定信号时返回 SemanticMessage。
func deriveKind(e *event.Event) SemanticKind {
	// 1) 插件显式声明的语义种类。
	if v, ok := e.MetaValue("kind"); ok {
		if s, ok := v.AsString(); ok && s != "" {
			if k := normalizeKind(s); k != "" {
				return k
			}
		}
	}
	// 2) 由事件类型做确定性后缀匹配（类型字符串是显式事实，非推断）。
	t := strings.ToLower(string(e.Identity.Type))
	switch {
	case strings.Contains(t, "request"):
		return SemanticRequest
	case strings.Contains(t, "response"):
		return SemanticResponse
	case strings.Contains(t, "push"):
		return SemanticPush
	case strings.Contains(t, "state_change"), strings.Contains(t, "statechange"):
		return SemanticStateChange
	case strings.Contains(t, "transaction"):
		// 仅当事件类型本身显式为 transaction 时映射；事务"关系聚类"属 Phase 3。
		return SemanticTransaction
	}
	// 3) 缺省中性值：一个消息身份，未进一步判定语义。
	return SemanticMessage
}

// normalizeKind 把字符串规整为合法 SemanticKind；非法值返回空串（调用方据此忽略）。
func normalizeKind(s string) SemanticKind {
	switch SemanticKind(s) {
	case SemanticMessage, SemanticRequest, SemanticResponse, SemanticPush,
		SemanticStateChange, SemanticTransaction:
		return SemanticKind(s)
	default:
		return ""
	}
}

// deriveSource 确定语义断言来源。
//
// Phase 2 冻结为 SourceEngine：SemanticProjector 本身就是 Engine 的确定性投影，
// 其输出一律由引擎产生，故语义来源恒为 engine、confidence 恒为 1.0（见 Project）。
//
// 严禁以 _meta 是否存在推导来源：当前 _meta 承担"系统附加结构化字段"
// （direction/flow_id/msg_name 等），由 Pipeline 补充，不等价于"插件声明语义来源"。
// 用 _meta 存在性推导会把"语义来源"与"元数据来源"混为一谈。
//
// 未来若需区分"插件显式声明"与"引擎派生"，应引入明确的来源标记，例如：
//   - _meta.semantic_source = "plugin"
//   - 或在 Event 输入契约中定义独立的语义来源字段
//
// 而不是复用 _meta 的存在性。
func deriveSource(_ *event.Event) SemanticSource {
	return SourceEngine
}

// metaString 从 _meta 读取字符串字段；缺失或非字符串时返回空串。
func metaString(e *event.Event, key string) string {
	if v, ok := e.MetaValue(key); ok {
		if s, ok := v.AsString(); ok {
			return s
		}
	}
	return ""
}
