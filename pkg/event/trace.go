package event

// TraceContext 描述事件的执行链路上下文（Debug Trace）。
//
// 它回答的是"这个事件在运行链路中从哪里来、属于哪次业务操作"——
// 是系统运行过程中真实存在的链路事实，不是推理出来的知识关系。
//
// 与 OpenTelemetry Trace Model 的概念对应：
//
//	OTel TraceID      ↔ CorrelationID（一次业务操作/请求-响应组的聚合键）
//	OTel SpanID       ↔ Event.ID（每个事件即一个 span）
//	OTel ParentSpanID ↔ CausationID（直接前驱事件）
//
// 典型用途：
//   - Protocol Inspector 按同一 CorrelationID 聚合 LoginRequest/LoginResponse
//   - compare_sessions 比较两个 session 的 Execution Trace
//   - Replay 场景用 OriginID 标记 "这些事件来自 replay #N"
type TraceContext struct {
	// CausationID 是直接前驱事件的 ID（OTel: parent span id）。
	// 例如：LoginResponse 的 CausationID 指向触发它的 LoginRequest。
	CausationID EventID

	// CorrelationID 是同一业务流程/请求-响应组的聚合键（OTel: trace id）。
	// 网络天然异步：一次 AttackRequest 可能对应 AttackResult、DamageNotify、
	// BuffUpdate 多条不同方向、不同类型的消息，靠 CorrelationID 聚合。
	CorrelationID string

	// OriginID 是最初来源的 ID，用于快速溯源。
	// 例如：从同一原始包派生的事件 OriginID 都指向该包 ID；
	// 回放场景下所有事件可标记为来自同一个 replay run。
	OriginID EventID
}

// NewTraceContext 创建新的 TraceContext。
func NewTraceContext(causationID EventID, correlationID string, originID EventID) TraceContext {
	return TraceContext{
		CausationID:   causationID,
		CorrelationID: correlationID,
		OriginID:      originID,
	}
}

// IsZero 检查 TraceContext 是否为零值。
func (t TraceContext) IsZero() bool {
	return t.CausationID == "" && t.CorrelationID == "" && t.OriginID == ""
}

// HasCausation 检查是否有直接前驱。
func (t TraceContext) HasCausation() bool {
	return t.CausationID != ""
}

// HasCorrelation 检查是否有关联的业务流程。
func (t TraceContext) HasCorrelation() bool {
	return t.CorrelationID != ""
}

// HasOrigin 检查是否有原始来源。
func (t TraceContext) HasOrigin() bool {
	return t.OriginID != ""
}

// WithCausation 设置直接前驱事件 ID。
func (t TraceContext) WithCausation(causationID EventID) TraceContext {
	t.CausationID = causationID
	return t
}

// WithCorrelation 设置关联的业务流程 ID。
func (t TraceContext) WithCorrelation(correlationID string) TraceContext {
	t.CorrelationID = correlationID
	return t
}

// WithOrigin 设置原始来源 ID。
func (t TraceContext) WithOrigin(originID EventID) TraceContext {
	t.OriginID = originID
	return t
}
