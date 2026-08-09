package event

// Relation 描述事件的因果关系
// 回答"这个事件从哪里来？"的问题
type Relation struct {
	// CausationID 是直接原因事件的 ID
	// 例如：http.request 的 CausationID 是触发它的 network.packet 的 ID
	CausationID EventID

	// CorrelationID 是同一业务流程的标识
	// 例如：login-flow-001 可以关联 packet -> request -> response -> login-result
	CorrelationID string

	// OriginID 是原始来源事件的 ID
	// 用于快速追溯到最初的输入源
	// 例如：所有从 network.packet 派生的事件，OriginID 都指向该 packet 的 ID
	OriginID EventID
}

// NewRelation 创建新的 Relation
func NewRelation(causationID EventID, correlationID string, originID EventID) Relation {
	return Relation{
		CausationID:   causationID,
		CorrelationID: correlationID,
		OriginID:      originID,
	}
}

// IsZero 检查 Relation 是否为零值
func (r Relation) IsZero() bool {
	return r.CausationID == "" && r.CorrelationID == "" && r.OriginID == ""
}

// HasCausation 检查是否有直接原因
func (r Relation) HasCausation() bool {
	return r.CausationID != ""
}

// HasCorrelation 检查是否有关联的业务流程
func (r Relation) HasCorrelation() bool {
	return r.CorrelationID != ""
}

// HasOrigin 检查是否有原始来源
func (r Relation) HasOrigin() bool {
	return r.OriginID != ""
}

// WithCausation 设置直接原因事件 ID
func (r Relation) WithCausation(causationID EventID) Relation {
	r.CausationID = causationID
	return r
}

// WithCorrelation 设置关联的业务流程 ID
func (r Relation) WithCorrelation(correlationID string) Relation {
	r.CorrelationID = correlationID
	return r
}

// WithOrigin 设置原始来源事件 ID
func (r Relation) WithOrigin(originID EventID) Relation {
	r.OriginID = originID
	return r
}
