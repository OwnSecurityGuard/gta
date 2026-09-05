package main

import (
	"encoding/json"

	"gametrace/pkg/event"
)

// eventToMessage 将 Event 转换为 Message
// 从 Event 的 Identity、Context 和 Payload 中提取字段映射到 Message
func eventToMessage(ev *event.Event) (Message, error) {
	// 从 Payload.Value 中提取字段
	payloadMap, _ := ev.Payload.Value.AsObject()
	if payloadMap == nil {
		payloadMap = make(map[string]event.Value)
	}

	// 辅助函数：优先从 _meta 读取系统字段，再退化到 payload 顶层（兼容手动构造的测试数据）
	getValue := func(key string) (event.Value, bool) {
		if v, ok := ev.MetaValue(key); ok {
			return v, true
		}
		v, ok := payloadMap[key]
		return v, ok
	}

	// 提取可选字段
	var src, dst string
	if v, exists := getValue("src"); exists {
		if s, ok := v.AsString(); ok {
			src = s
		}
	}
	if v, exists := getValue("dst"); exists {
		if s, ok := v.AsString(); ok {
			dst = s
		}
	}

	// 优先从 EventContext 取方向，回退到 payload 字段
	direction := ev.Context.Direction
	if direction == "" {
		if v, exists := getValue("direction"); exists {
			if d, ok := v.AsString(); ok {
				direction = d
			}
		}
	}

	var msgName string
	if v, exists := getValue("msg_name"); exists {
		if m, ok := v.AsString(); ok {
			msgName = m
		}
	}

	var isPush bool
	if v, exists := getValue("is_push"); exists {
		if b, ok := v.AsBool(); ok {
			isPush = b
		}
	}

	var tcpFlags string
	if v, exists := getValue("tcp_flags"); exists {
		if t, ok := v.AsString(); ok {
			tcpFlags = t
		}
	}

	// 将干净业务数据转回 JSON（排除 _meta / _state_changes 等内部字段）
	cleanMap := make(map[string]any, len(payloadMap))
	for k, v := range payloadMap {
		if k == "_meta" || k == "_state_changes" {
			continue
		}
		cleanMap[k] = v.ToAny()
	}
	jsonData, err := json.Marshal(cleanMap)
	if err != nil {
		return Message{}, err
	}

	// 从 Identity.Type 推断 msgName（如果未从 payload 中提取到）
	if msgName == "" {
		msgName = string(ev.Identity.Type)
	}

	msg := Message{
		FlowID:    ev.Context.FlowID,
		Timestamp: ev.Identity.Timestamp,
		Direction: direction,
		MsgName:   msgName,
		IsPush:    isPush,
		Src:       src,
		Dst:       dst,
		JSON:      jsonData,
		TCPFlags:  tcpFlags,
	}

	return msg, nil
}

// eventsToMessages 批量将 Event 转换为 Message
func eventsToMessages(events []*event.Event) ([]Message, error) {
	messages := make([]Message, 0, len(events))
	for i, ev := range events {
		msg, err := eventToMessage(ev)
		if err != nil {
			return nil, err
		}
		msg.Index = i
		messages = append(messages, msg)
	}
	return messages, nil
}
