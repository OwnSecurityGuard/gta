package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gametrace/pkg/decode"
	"gametrace/pkg/store"
)

// queryFlowMessages 从 reader 查询 flow 内的消息（按 timestamp 排序）。
// 通过 ev.Context.FlowID 过滤。
func queryFlowMessages(ctx context.Context, reader captureReader, sessionID string, flowID string, from, to time.Time) ([]Message, error) {
	// 查询 events 表（限制 10000 条，避免内存溢出）
	events, err := reader.QueryEvents(ctx, sessionID, 10000, 0)
	if err != nil {
		return nil, err
	}

	// 转换为 Message 并通过 Context.FlowID 过滤（回退到 payload 中提取）
	var messages []Message
	for _, ev := range events {
		// 时间窗口过滤
		if ev.Identity.Timestamp.Before(from) || ev.Identity.Timestamp.After(to) {
			continue
		}

		// 通过 Context.FlowID 过滤，回退到 payload 中的 flow_id 字段
		evFlowID := ev.Context.FlowID
		if evFlowID == "" {
			if payloadObj, ok := ev.Payload.Value.AsObject(); ok {
				if flowIDVal, exists := payloadObj["flow_id"]; exists {
					if s, ok := flowIDVal.AsString(); ok {
						evFlowID = s
					}
				}
			}
		}
		if evFlowID != flowID {
			continue
		}

		msg, err := eventToMessage(ev)
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}

	// 设置 Index
	for i := range messages {
		messages[i].Index = i
	}
	return messages, nil
}

// computeCloseInfo 从 flow 内的 tcp_close 事件推断连接关闭方。
// 推断逻辑：
//  1. 找到第一个携带 FIN/RST 的 tcp_close 消息（最早发起关闭的一方）
//  2. 根据 flow 内已知消息的 direction 推断 client/server 角色：
//     - 第一个 direction=client_to_server 的消息的 src 视为 client
//     - 第一个 direction=server_to_client 的消息的 src 视为 server
//  3. 比较 tcp_close 消息的 src 与 client/server 地址，判定 closer
//
// 若无法推断角色（flow 内无应用层消息），返回 closer="unknown"。
func computeCloseInfo(messages []Message) *CloseInfo {
	// 找第一个 tcp_close 事件
	var closeMsg *Message
	for i := range messages {
		if messages[i].TCPFlags != "" {
			closeMsg = &messages[i]
			break
		}
	}
	if closeMsg == nil {
		return nil
	}

	// 推断 client/server 角色
	clientAddr := ""
	serverAddr := ""
	for _, m := range messages {
		if m.TCPFlags != "" {
			continue // 跳过 tcp_close 事件
		}
		if m.Direction == "client_to_server" && clientAddr == "" {
			clientAddr = m.Src
		}
		if m.Direction == "server_to_client" && serverAddr == "" {
			serverAddr = m.Src
		}
		if clientAddr != "" && serverAddr != "" {
			break
		}
	}

	closer := "unknown"
	note := ""
	if clientAddr != "" || serverAddr != "" {
		if closeMsg.Src == clientAddr && clientAddr != "" {
			closer = "client"
		} else if closeMsg.Src == serverAddr && serverAddr != "" {
			closer = "server"
		} else {
			note = "cannot determine closer role: tcp_close src does not match known client/server"
		}
	} else {
		note = "no application-layer messages to determine client/server roles"
	}

	return &CloseInfo{
		Closer:    closer,
		Method:    closeMsg.TCPFlags,
		Timestamp: closeMsg.Timestamp,
		Src:       closeMsg.Src,
		Dst:       closeMsg.Dst,
		Note:      note,
	}
}

// applyNoiseFilter 应用噪声过滤。
func applyNoiseFilter(messages []Message, cfg NoiseFilter) []Message {
	var filtered []Message
	for _, msg := range messages {
		// drop_names: 精确匹配
		if len(cfg.DropNames) > 0 {
			for _, name := range cfg.DropNames {
				if msg.MsgName == name {
					goto skip
				}
			}
		}
		// drop_heartbeats: 启发式匹配心跳消息
		if cfg.DropHeartbeats {
			name := strings.ToLower(msg.MsgName)
			if strings.Contains(name, "heartbeat") || strings.Contains(name, "ping") || strings.Contains(name, "pong") {
				goto skip
			}
		}
		filtered = append(filtered, msg)
	skip:
	}
	// 重新设置 Index，确保过滤后 Index 与 slice 索引一致。
	// 配对逻辑（pairByDirection/finalizePairs/identifyPushes）依赖 Message.Index 字段
	// 与 messages slice 索引一致来标记已配对的 request/response。
	for i := range filtered {
		filtered[i].Index = i
	}
	return filtered
}

// pairByMsgName 按 msg_name 去后缀 + 时间窗口配对。
// pairWindow: 配对时间窗口（默认 2000ms）
func pairByMsgName(messages []Message, pairWindow time.Duration) []RequestResponsePair {
	var pairs []RequestResponsePair
	used := make(map[int]bool) // 已配对的 response 索引

	for i, req := range messages {
		if req.Direction != "client_to_server" || req.IsPush {
			continue
		}
		reqBase := decode.StripReqRespSuffix(req.MsgName)

		// 在 req 之后的 pairWindow 内寻找匹配的 response
		for j := i + 1; j < len(messages); j++ {
			if used[j] {
				continue
			}
			resp := messages[j]
			if resp.Direction != "server_to_client" || resp.IsPush {
				continue
			}
			if resp.Timestamp.Sub(req.Timestamp) > pairWindow {
				break // 超出时间窗口，停止搜索
			}
			respBase := decode.StripReqRespSuffix(resp.MsgName)
			if reqBase == respBase && reqBase != req.MsgName {
				// 基名匹配且原名有后缀（避免误配对无后缀的同名消息）
				pairs = append(pairs, RequestResponsePair{
					Request:  req,
					Response: &resp,
					PairRule: "msg_name_suffix",
				})
				used[j] = true
				break
			}
		}
	}
	return pairs
}

// pairByDirection 对未配对的 request，按方向 + 时间邻近寻找最近的 response。
func pairByDirection(messages []Message, pairs []RequestResponsePair, pairWindow time.Duration) []RequestResponsePair {
	pairedReqs := make(map[int]bool)
	for _, p := range pairs {
		pairedReqs[p.Request.Index] = true
	}

	for i, req := range messages {
		if pairedReqs[i] {
			continue
		}
		if req.Direction != "client_to_server" || req.IsPush {
			continue
		}

		// 寻找时间窗口内最近的 response（不要求 msg_name 匹配）
		var bestResp *Message
		var bestDelta time.Duration
		for j := i + 1; j < len(messages); j++ {
			resp := messages[j]
			if resp.Direction != "server_to_client" || resp.IsPush {
				continue
			}
			delta := resp.Timestamp.Sub(req.Timestamp)
			if delta > pairWindow {
				break
			}
			if bestResp == nil || delta < bestDelta {
				idx := j
				bestResp = &messages[idx]
				bestDelta = delta
			}
		}
		if bestResp != nil {
			pairs = append(pairs, RequestResponsePair{
				Request:  req,
				Response: bestResp,
				PairRule: "direction_temporal",
			})
		}
	}
	return pairs
}

// finalizePairs 补充未配对的 request，标记 uncertainty。
func finalizePairs(messages []Message, pairs []RequestResponsePair) ([]RequestResponsePair, []string) {
	var uncertainties []string
	pairedReqs := make(map[int]bool)
	for _, p := range pairs {
		pairedReqs[p.Request.Index] = true
	}

	for i, msg := range messages {
		if pairedReqs[i] {
			continue
		}
		if msg.Direction != "client_to_server" || msg.IsPush {
			continue
		}
		pairs = append(pairs, RequestResponsePair{
			Request:  msg,
			Response: nil,
			PairRule: "unpaired",
		})
		uncertainties = append(uncertainties, fmt.Sprintf("request msg_id=%d (%s) has no matched response within pair_window", msg.MsgID, msg.MsgName))
	}
	return pairs, uncertainties
}

// identifyPushes 识别与 request 相关的 push 消息。
// relatedWindow: request 后多久内的 push 视为相关（默认 5000ms）
func identifyPushes(messages []Message, pairs []RequestResponsePair, relatedWindow time.Duration) map[int][]Message {
	// request_index -> related pushes
	pushesByReq := make(map[int][]Message)

	pairedResps := make(map[int]bool)
	for _, p := range pairs {
		if p.Response != nil {
			pairedResps[p.Response.Index] = true
		}
	}

	for _, p := range pairs {
		req := p.Request
		// 在 request 之后的 relatedWindow 内寻找 push
		for j := req.Index + 1; j < len(messages); j++ {
			msg := messages[j]
			if msg.Timestamp.Sub(req.Timestamp) > relatedWindow {
				break
			}
			if msg.Direction != "server_to_client" {
				continue
			}
			if pairedResps[j] {
				continue // 已配对为 response
			}
			// 无对应 request 的 server_to_client 视为 push
			pushesByReq[req.Index] = append(pushesByReq[req.Index], msg)
		}
	}
	return pushesByReq
}

// computeEntityDiffs 计算 request 前后窗口内的 entity 字段变更。
// 从 state_changes 投影表读取。
func computeEntityDiffs(
	ctx context.Context,
	reader captureReader,
	sessionID string,
	flowID string,
	reqTimestamp time.Time,
	window time.Duration,
) ([]EntityDiff, error) {
	from := reqTimestamp.Add(-window)
	to := reqTimestamp.Add(window)

	rows, err := reader.QueryStateChanges(ctx, store.StateChangeQuery{
		SessionID: sessionID,
		FlowID:    flowID,
	})
	if err != nil {
		return nil, err
	}

	// 按 (subject_type, subject_id, path) 分组，取窗口内的最后一次变更
	type fieldKey struct{ SubjectType, SubjectID, Path string }
	lastChange := map[fieldKey]struct {
		Op        string
		Value     any
		Timestamp time.Time
	}{}
	for _, r := range rows {
		// 时间窗口过滤（QueryStateChanges 不支持时间范围，在应用层过滤）
		if r.Timestamp.Before(from) || r.Timestamp.After(to) {
			continue
		}
		var value any
		if r.After != "" {
			_ = json.Unmarshal([]byte(r.After), &value)
		}
		fk := fieldKey{r.SubjectType, r.SubjectID, r.Path}
		// 取最后一次变更（rows 已按 timestamp ASC 排序，后到的覆盖）
		lastChange[fk] = struct {
			Op        string
			Value     any
			Timestamp time.Time
		}{Op: r.Op, Value: value, Timestamp: r.Timestamp}
	}

	// 按 (subject_type, subject_id) 聚合字段
	byEntity := map[string]*EntityDiff{}
	for fk := range lastChange {
		entityKey := fk.SubjectType + "|" + fk.SubjectID
		diff, ok := byEntity[entityKey]
		if !ok {
			diff = &EntityDiff{URI: fk.SubjectType, Key: fk.SubjectID}
			byEntity[entityKey] = diff
		}
		diff.Fields = append(diff.Fields, fk.Path)
	}

	var diffs []EntityDiff
	for _, d := range byEntity {
		diffs = append(diffs, *d)
	}
	return diffs, nil
}

// summarizeMessage 从 JSON 中提取简短摘要（用于 push summary）。
func summarizeMessage(jsonBytes []byte) string {
	if len(jsonBytes) == 0 {
		return ""
	}
	var data map[string]any
	if err := json.Unmarshal(jsonBytes, &data); err != nil {
		return "<unparseable>"
	}
	summary := ""
	if t, ok := data["type"].(string); ok {
		summary = t
	}
	if name, ok := data["name"].(string); ok {
		summary = name
	}
	if summary == "" {
		var keys []string
		for k := range data {
			keys = append(keys, k)
		}
		summary = strings.Join(keys, ",")
	}
	if len(summary) > 100 {
		summary = summary[:97] + "..."
	}
	return summary
}

// computeWhyRelated 计算每步的关联理由。
func computeWhyRelated(steps []TraceStep) {
	for i := range steps {
		var reasons []string
		step := &steps[i]

		if i > 0 {
			// time_proximity 由调用方根据 step 时间差判断，这里简化
			reasons = append(reasons, "time_proximity")
		}

		if len(step.EntityDiffs) > 0 {
			reasons = append(reasons, "entity_diff")
		}
		if step.Response != nil {
			reasons = append(reasons, "response_chain")
		}
		if len(step.Pushes) > 0 {
			reasons = append(reasons, "push_followup")
		}
		if step.Response == nil {
			reasons = append(reasons, "unpaired_request")
		}

		if len(reasons) > 0 {
			step.WhyRelated = strings.Join(reasons, " + ")
		} else {
			step.WhyRelated = "isolated"
		}
	}
	// 首步特殊处理
	if len(steps) > 0 {
		if steps[0].WhyRelated == "isolated" || strings.Contains(steps[0].WhyRelated, "time_proximity") {
			steps[0].WhyRelated = "operation_start"
		}
	}
}

// hasAnyEntityDiff 检查是否有任何 step 含 entity diff。
func hasAnyEntityDiff(steps []TraceStep) bool {
	for _, s := range steps {
		if len(s.EntityDiffs) > 0 {
			return true
		}
	}
	return false
}

// dedupStrings 去重字符串切片。
func dedupStrings(s []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			result = append(result, v)
		}
	}
	return result
}

// buildTraceSummary 返回大结果的摘要。
func buildTraceSummary(steps []TraceStep) map[string]any {
	var msgNames []string
	responseCount := 0
	pushCount := 0
	entityDiffCount := 0
	for _, s := range steps {
		msgNames = append(msgNames, s.Request.Name)
		if s.Response != nil {
			responseCount++
		}
		pushCount += len(s.Pushes)
		entityDiffCount += len(s.EntityDiffs)
	}
	return map[string]any{
		"step_count":        len(steps),
		"request_names":     dedupStrings(msgNames),
		"response_count":    responseCount,
		"push_count":        pushCount,
		"entity_diff_count": entityDiffCount,
	}
}
