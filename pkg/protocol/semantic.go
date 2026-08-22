package protocol

// Correlation 描述一条消息与其对端消息的关联关系（Request/Response 配对）。
//
// Direction 表示本消息在关联中扮演的角色：
//   - request  本消息是请求侧，应被"记住"，等待后续响应匹配
//   - response 本消息是响应侧，用它去"查询"之前记住的请求
//
// Key 是关联的通信字段名（取配置 expr 的最后一段，如 body.seq → seq）。
// Value 是从消息中按 expr 提取到的实际值，是配对双方共用的匹配键。
type Correlation struct {
	Direction string `json:"direction"`
	Rule      string `json:"rule,omitempty"` // 命中的规则名
	Key       string `json:"key"`            // 通信字段名
	Value     string `json:"value"`          // 提取到的值（规范化字符串）
}

// ProtocolError 描述消息的错误语义（可选）。
type ProtocolError struct {
	Failed bool   `json:"failed"`
	Code   string `json:"code,omitempty"`
}

// ProtocolContext 是放入 Semantic Event Context 的通信语义块。
// 对应设计中的 context.protocol。
type ProtocolContext struct {
	Message     string         `json:"message,omitempty"`   // 消息名（如 LoginRequest）
	Role        MessageRole    `json:"role"`                // 通信角色
	Correlation *Correlation   `json:"correlation,omitempty"` // 请求/响应关联
	Delivery    string         `json:"delivery,omitempty"`  // 投递类型（request/response/push）
	Error       *ProtocolError `json:"error,omitempty"`     // 错误语义
}

// Result 是 ProtocolResolver.Resolve 的返回值，描述一条消息的通信语义。
type Result struct {
	// HasMessage 表示该消息命中了 message.definitions 中的已知消息。
	HasMessage bool
	// MessageID 是 message.id.expr 提取的原始值（规范化字符串）。
	MessageID string
	// Message 是定义的消息名。
	Message string
	// Role 是消息角色。
	Role MessageRole
	// Delivery 是投递类型（未命中时为 "unknown"）。
	Delivery string
	// Correlation 是该消息的关联描述；未命中则为 nil。
	Correlation *Correlation
	// Error 是该消息的错误语义；未配置或无 ErrorCode 时为 nil。
	Error *ProtocolError
}

// ProtocolContext 返回该结果对应的 context.protocol 结构。
func (r Result) ProtocolContext() ProtocolContext {
	return ProtocolContext{
		Message:     r.Message,
		Role:        r.Role,
		Correlation: r.Correlation,
		Delivery:    r.Delivery,
		Error:       r.Error,
	}
}