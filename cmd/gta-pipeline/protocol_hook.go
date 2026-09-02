package main

import (
	"encoding/json"

	"gta/pkg/event"
	"gta/pkg/protocol"
)

// enrichProtocol 用 ProtocolResResolver 把单条已解码事件的 JSON payload 解释为通信语义，
// 并把结果写回事件：
//   - correlation：request 被 Remember，response 据此 Lookup 并设置 CausationID；
//     配对双方共享同一 CorrelationID（flow|rule|value）用于链路聚合。
//   - _meta.protocol：写入 ProtocolContext（message/role/delivery/correlation/error）。
//
// 纯副作用增强，不改变事件幂等性与既有 _fields / _state_changes 语义。
func (t *captureTask) enrichProtocol(ev *event.Event) {
	if ev == nil || t.protocolResolver == nil {
		return
	}
	js, err := ev.Payload.ToJSON()
	if err != nil {
		if t.logger != nil {
			t.logger.Debug("protocol: marshal payload", "error", err)
		}
		return
	}

	// 语义解析：identity / role / correlation / delivery / error
	res := t.protocolResolver.Resolve(string(js))

	// Request/Response 关联：remember 请求、匹配响应
	if res.Correlation != nil && res.Correlation.Value != "" {
		key := protocolCorrelationKey(ev.Context.FlowID, res.Correlation)
		switch protocol.MessageRole(res.Correlation.Direction) {
		case protocol.RoleRequest:
			t.corrStore.Remember(key, string(ev.GetID()))
		case protocol.RoleResponse:
			if p, ok := t.corrStore.Lookup(key); ok && p.CausationID != "" {
				ev.Trace.CausationID = event.EventID(p.CausationID)
			}
		}
		// 配对双方共享同一 group key，供 Protocol Inspector 聚合 Request/Response。
		ev.Trace.CorrelationID = key
	}

	// 把语义上下文投影进 payload._meta.protocol
	pctx, err := protocolContextValue(res)
	if err != nil {
		if t.logger != nil {
			t.logger.Debug("protocol: encode context", "error", err)
		}
		return
	}
	if updated, ok := setMetaProtocol(ev.Payload.Value, pctx); ok {
		ev.Payload.Value = updated
	}
}

// protocolContextValue 把 Result 的 ProtocolContext 编码为可入 payload 的 Value。
// 走 JSON 编码以复用 struct 上的 omitempty，避免序列化空可选字段。
func protocolContextValue(res protocol.Result) (event.Value, error) {
	b, err := json.Marshal(res.ProtocolContext())
	if err != nil {
		return event.Value{}, err
	}
	return event.ValueFromJSON(b)
}

// setMetaProtocol 把 protocol Value 写入 payload 的 _meta.protocol，返回新的不可变 Value。
func setMetaProtocol(payload, pctx event.Value) (event.Value, bool) {
	obj, ok := payload.AsObject()
	if !ok {
		return payload, false
	}
	var m map[string]event.Value
	if meta, has := obj["_meta"]; has {
		m, _ = meta.AsObject()
	}
	if m == nil {
		m = map[string]event.Value{}
	}
	m["protocol"] = pctx

	cp := make(map[string]event.Value, len(obj)+1)
	for k, v := range obj {
		cp[k] = v
	}
	cp["_meta"] = event.ValueObject(m)
	return event.ValueObject(cp), true
}

// protocolCorrelationKey 构建一个在 flow 内唯一的关联键（flow|rule|value）。
// Request/Response 同 rule、同 value，因而键一致，可完成配对。
func protocolCorrelationKey(flow string, c *protocol.Correlation) string {
	return flow + "|" + c.Rule + "|" + c.Value
}