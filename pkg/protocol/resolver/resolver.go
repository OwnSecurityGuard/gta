// Package resolver 是 Protocol Behavior Resolver：把已解码 JSON 解释为通信语义事件。
//
// 只回答两个问题：
//  1. 这条消息是什么？（message identity / role / delivery）
//  2. 它和其他消息有什么通信关系？（correlation / error）
//
// 纯函数、无状态，便于并发与单测。跨消息的 Request/Response 状态由 CorrelationStore 负责。
package resolver

import (
	"fmt"

	"gta/pkg/protocol"
	"gta/pkg/protocol/config"
	"gta/pkg/protocol/matcher"
)

// ProtocolResolver 持有编译后的 protocol.yaml 规则，对单条 JSON 消息做语义解析。
type ProtocolResolver struct {
	messageIDExpr string
	msgs          map[string]config.MessageDefinition // key: 规范化的消息 value
	correlations  []config.CorrelationRule
	pushes        []config.PushRule
	errCfg        *config.ErrorConfig
}

// New 从 protocol.yaml 模型构建解析器。角色取值在此校验。
func New(cfg *config.File) (*ProtocolResolver, error) {
	r := &ProtocolResolver{msgs: make(map[string]config.MessageDefinition)}
	if cfg == nil {
		return r, nil
	}

	if cfg.Message != nil {
		r.messageIDExpr = cfg.Message.ID.Expr
		for _, d := range cfg.Message.Definitions {
			if d.Role != "" && !protocol.ValidRole(d.Role) {
				return nil, fmt.Errorf("message %q has invalid role %q (allowed: request|response|push)", d.Name, d.Role)
			}
			key := valueKey(d.Value)
			if _, dup := r.msgs[key]; dup {
				return nil, fmt.Errorf("duplicate message definition value %v", d.Value)
			}
			r.msgs[key] = d
		}
	}
	if cfg.Correlation != nil {
		r.correlations = cfg.Correlation.Rules
	}
	if cfg.Push != nil {
		r.pushes = cfg.Push.Rules
	}
	r.errCfg = cfg.Error
	return r, nil
}

// Resolve 对单条已解码 JSON 消息做语义解析。
// json 应为无业务前缀的原始 JSON（如 {"header":{"cmd":1001},"body":{"seq":10}}）。
func (r *ProtocolResolver) Resolve(json string) protocol.Result {
	res := protocol.Result{
		Role:     protocol.RoleUnknown,
		Delivery: string(protocol.RoleUnknown),
	}

	// 1. Message Identity + Role
	if r.messageIDExpr != "" {
		if id, ok := matcher.Get(json, r.messageIDExpr); ok {
			if def, ok := r.msgs[id]; ok {
				res.HasMessage = true
				res.MessageID = id
				res.Message = def.Name
				if def.Role != "" {
					res.Role = protocol.MessageRole(def.Role)
					res.Delivery = def.Role
				}
			}
		}
	}

	// 2. Delivery — push 规则可以覆盖（无 message 定义时的补充识别）
	if r.isPush(json) {
		res.Delivery = string(protocol.RolePush)
	}

	// 3. Correlation — 方向跟随消息角色；角色未知时按 request 优先回退探测
	c := res.Role
	for _, rule := range r.correlations {
		if corr, ok := r.matchCorrelation(json, rule, c); ok {
			res.Correlation = corr
			break
		}
	}

	// 4. Error
	if r.errCfg != nil {
		res.Error = r.resolveError(json)
	}

	return res
}

// isPush 判定是否命中任一 push 规则。
// 规则：expr 存在，且（若配置了 equals）命中任一等值。
func (r *ProtocolResolver) isPush(json string) bool {
	for _, pr := range r.pushes {
		rv, ok := matcher.Raw(json, pr.When.Expr)
		if !ok {
			continue
		}
		eq := pr.When.EqualsList()
		if len(eq) == 0 {
			return true // 仅按字段存在判定
		}
		for _, want := range eq {
			if matcher.Equals(rv, want) {
				return true
			}
		}
	}
	return false
}

// matchCorrelation 对一条消息尝试匹配相关性规则。
// 方向不靠路径推断（请求与响应可能用同一路径，如 body.seq），而是跟随消息角色：
//   - request   → 取 request.expr
//   - response  → 取 response.expr
//   - 未知角色  → 依次尝试 request/response（request 优先，作为兜底）
func (r *ProtocolResolver) matchCorrelation(json string, rule config.CorrelationRule, role protocol.MessageRole) (*protocol.Correlation, bool) {
	if role == protocol.RoleRequest {
		return r.corrFrom(json, rule, rule.Request.Expr, "request", rule.Name)
	}
	if role == protocol.RoleResponse {
		return r.corrFrom(json, rule, rule.Response.Expr, "response", rule.Name)
	}
	// 未知角色：request 优先，其次 response
	if c, ok := r.corrFrom(json, rule, rule.Request.Expr, "request", rule.Name); ok {
		return c, true
	}
	return r.corrFrom(json, rule, rule.Response.Expr, "response", rule.Name)
}

// corrFrom 按给定 expr 提取关联值并构造 Correlation。
func (r *ProtocolResolver) corrFrom(json string, rule config.CorrelationRule, expr, direction, name string) (*protocol.Correlation, bool) {
	v, ok := matcher.Get(json, expr)
	if !ok {
		return nil, false
	}
	return &protocol.Correlation{
		Direction: direction,
		Rule:      name,
		Key:       matcher.KeyOf(expr),
		Value:     v,
	}, true
}

// resolveError 提取错误码并判定是否失败。
func (r *ProtocolResolver) resolveError(json string) *protocol.ProtocolError {
	code, ok := matcher.Get(json, r.errCfg.Code.Expr)
	if !ok {
		return nil
	}
	failed := true
	if raw, ok := matcher.Raw(json, r.errCfg.Code.Expr); ok {
		for _, want := range r.errCfg.Success.Values {
			if matcher.Equals(raw, want) {
				failed = false
				break
			}
		}
	}
	return &protocol.ProtocolError{Failed: failed, Code: code}
}

// valueKey 把配置的 message value 规范化为可比较的字符串键。
func valueKey(v any) string {
	if v == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%v", v)
}
