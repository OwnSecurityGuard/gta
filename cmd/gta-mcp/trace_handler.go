package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	mcp "github.com/mark3labs/mcp-go/mcp"
)

// handleTraceProtocolFlow 构建一次操作的时序证据链。
func (m *mcpCapture) handleTraceProtocolFlow(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.runRegistry == nil {
		return errorResult(fmt.Errorf("run registry not initialized")), nil
	}

	// 1. 解析输入
	runID, err := req.RequireString("run_id")
	if err != nil {
		return errorResult(err), nil
	}
	flowID, err := req.RequireString("flow_id")
	if err != nil {
		return errorResult(err), nil
	}
	featureName, err := req.RequireString("feature_name")
	if err != nil {
		return errorResult(err), nil
	}

	noiseFilter := parseNoiseFilter(req)
	entityDiffCfg := parseEntityDiffConfig(req)

	// 2. 加载 run 边界
	rec, err := m.runRegistry.Get(runID)
	if err != nil {
		return errorResult(fmt.Errorf("run not found: %s", runID)), nil
	}
	timeFrom := rec.TimeFrom
	var timeTo time.Time
	if rec.TimeTo != nil {
		timeTo = *rec.TimeTo
	} else {
		timeTo = time.Now()
	}

	// 3. 查询 flow 内消息
	reader, err := m.openReader(rec.SessionID)
	if err != nil {
		return errorResult(fmt.Errorf("open reader: %w", err)), nil
	}
	defer reader.Close()

	messages, err := queryFlowMessages(ctx, reader, rec.SessionID, flowID, timeFrom, timeTo)
	if err != nil {
		return errorResult(fmt.Errorf("query flow messages: %w", err)), nil
	}

	// 在 noise filter 之前计算 close_info，避免 tcp_close 事件被误过滤
	closeInfo := computeCloseInfo(messages)

	if len(messages) == 0 {
		return successResult(TraceResult{
			RunID:         runID,
			FlowID:        flowID,
			FeatureName:   featureName,
			TimeWindow:    TimeWindow{From: timeFrom, To: timeTo},
			Steps:         []TraceStep{},
			CloseInfo:     closeInfo,
			Uncertainties: []string{"no messages in flow window"},
		}), nil
	}

	// 4. noise filtering
	messages = applyNoiseFilter(messages, noiseFilter)
	if len(messages) == 0 {
		return successResult(TraceResult{
			RunID:         runID,
			FlowID:        flowID,
			FeatureName:   featureName,
			TimeWindow:    TimeWindow{From: timeFrom, To: timeTo},
			Steps:         []TraceStep{},
			CloseInfo:     closeInfo,
			Uncertainties: []string{"all messages filtered by noise_filter"},
		}), nil
	}

	// 5. request/response 配对（三级规则）
	pairs := pairByMsgName(messages, 2000*time.Millisecond)
	pairs = pairByDirection(messages, pairs, 2000*time.Millisecond)
	var uncertainties []string
	pairs, uncertainties = finalizePairs(messages, pairs)

	// 6. push 识别
	pushesByReq := identifyPushes(messages, pairs, 5000*time.Millisecond)

	// 7. key_fields 提取器
	extractor, err := m.loadKeyFieldExtractor(rec.SessionID)
	if err != nil {
		uncertainties = append(uncertainties, fmt.Sprintf("load key_fields failed: %v", err))
		extractor = nil
	}

	// 8. 组装 steps
	var steps []TraceStep
	for i, pair := range pairs {
		step := TraceStep{
			StepID:       fmt.Sprintf("s%d", i+1),
			RequestMsgID: pair.Request.MsgID,
			Request: RequestSummary{
				Name:      pair.Request.MsgName,
				Direction: pair.Request.Direction,
			},
		}

		// key_fields
		if extractor != nil {
			kf, err := extractor.Extract(pair.Request.JSON)
			if err == nil {
				step.Request.KeyFields = kf
			}
		}

		// response
		if pair.Response != nil {
			resp := &ResponseSummary{
				MsgID: pair.Response.MsgID,
				Name:  pair.Response.MsgName,
			}
			if extractor != nil {
				kf, err := extractor.Extract(pair.Response.JSON)
				if err == nil {
					resp.KeyFields = kf
				}
			}
			step.Response = resp
		}

		// pushes
		if pushes, ok := pushesByReq[pair.Request.Index]; ok {
			for _, p := range pushes {
				step.Pushes = append(step.Pushes, PushSummary{
					MsgID:   p.MsgID,
					Name:    p.MsgName,
					Summary: summarizeMessage(p.JSON),
				})
			}
		}

		// entity diffs
		if entityDiffCfg.Enabled {
			diffs, err := computeEntityDiffs(ctx, reader, rec.SessionID, flowID, pair.Request.Timestamp,
				time.Duration(entityDiffCfg.WindowMs)*time.Millisecond)
			if err != nil {
				uncertainties = append(uncertainties, fmt.Sprintf("entity diff for step %s failed: %v", step.StepID, err))
			} else {
				step.EntityDiffs = diffs
			}
		}

		steps = append(steps, step)
	}

	// 9. why_related
	computeWhyRelated(steps)

	// 10. entity diff 全空时标注 uncertainty
	if entityDiffCfg.Enabled && !hasAnyEntityDiff(steps) {
		uncertainties = append(uncertainties, "no entity snapshots found; entity_diffs empty (plugin may not produce entity events)")
	}

	// 11. 组装结果
	result := TraceResult{
		RunID:         runID,
		FlowID:        flowID,
		FeatureName:   featureName,
		TimeWindow:    TimeWindow{From: timeFrom, To: timeTo},
		Steps:         steps,
		CloseInfo:     closeInfo,
		Uncertainties: uncertainties,
	}

	// 12. 大结果写文件
	if len(steps) > 50 {
		path, err := m.writeTraceFile(runID, result)
		if err != nil {
			uncertainties = append(uncertainties, fmt.Sprintf("write trace file failed: %v", err))
		} else {
			result.FilePath = path
			// 返回摘要
			return successResult(map[string]any{
				"run_id":        runID,
				"flow_id":       flowID,
				"feature_name":  featureName,
				"step_count":    len(steps),
				"file_path":     path,
				"uncertainties": uncertainties,
				"summary":       buildTraceSummary(steps),
			}), nil
		}
	}

	// 13. 返回完整结果
	slog.Info("trace_protocol_flow", "run_id", runID, "flow_id", flowID, "steps", len(steps), "uncertainties", len(uncertainties))
	return successResult(result), nil
}

// parseNoiseFilter 从请求解析噪声过滤配置。
func parseNoiseFilter(req mcp.CallToolRequest) NoiseFilter {
	cfg := NoiseFilter{
		DropHeartbeats: true, // 默认开启
	}
	if v, ok := req.GetArguments()["noise_filter"].(map[string]any); ok {
		if names, ok := v["drop_names"].([]any); ok {
			for _, n := range names {
				if s, ok := n.(string); ok {
					cfg.DropNames = append(cfg.DropNames, s)
				}
			}
		}
		if hb, ok := v["drop_heartbeats"].(bool); ok {
			cfg.DropHeartbeats = hb
		}
	}
	return cfg
}

// parseEntityDiffConfig 从请求解析 entity diff 配置。
func parseEntityDiffConfig(req mcp.CallToolRequest) EntityDiffConfig {
	cfg := EntityDiffConfig{
		Enabled:  true,  // 默认开启
		WindowMs: 500,   // 默认 500ms
	}
	if v, ok := req.GetArguments()["entity_diff"].(map[string]any); ok {
		if en, ok := v["enabled"].(bool); ok {
			cfg.Enabled = en
		}
		if ms, ok := v["window_ms"].(float64); ok {
			cfg.WindowMs = int(ms)
		}
	}
	return cfg
}


