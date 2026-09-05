package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"gametrace/pkg/auth"
	pb "gametrace/pkg/internalipc/proto"
)

// handleBeginCaptureRun 标记一次用户操作的开始，不清除/停止现有 capture。
func (m *mcpCapture) handleBeginCaptureRun(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.runRegistry == nil {
		return errorResult(fmt.Errorf("run registry not initialized")), nil
	}

	featureName, err := req.RequireString("feature_name")
	if err != nil {
		return errorResult(err), nil
	}
	projectPath, err := req.RequireString("project_path")
	if err != nil {
		return errorResult(err), nil
	}
	pluginName := req.GetString("plugin_name", "")
	device := req.GetString("device", "")
	filter := req.GetString("filter", "")
	port := req.GetInt("port", 0)

	m.mu.Lock()
	current, err := m.sessionMgr.readCurrent(auth.OwnerFrom(ctx))
	m.mu.Unlock()

	// 决定 capture_isolation_mode
	var (
		isolationMode string
		captureStatus string
		sessionID     string
		baseline      snapshotBaseline
		uncertainties []string
	)

	if err == nil && current != nil && current.Status == "running" && m.pipelineClient != nil {
		// 已有 capture 运行中
		sessionID = current.SessionID
		// 通过 gRPC 获取实时计数
		resp, grpcErr := m.pipelineClient.GetCaptureStatus(ctx, &pb.GetCaptureStatusRequest{SessionId: sessionID})
		if grpcErr == nil {
			baseline = snapshotBaseline{
				RawPackets:   resp.GetRawCount(),
				Events:       resp.GetEventCount(),
				Metrics:      resp.GetMetricCount(),
				DecodeErrors: resp.GetDecodeErrors(),
			}
			// 检查参数是否匹配
			paramsMatch := true
			if pluginName != "" && pluginName != current.Plugin {
				paramsMatch = false
			}
			if port != 0 && port != current.Port {
				paramsMatch = false
			}
			if paramsMatch {
				isolationMode = "reuse_existing"
			} else {
				isolationMode = "time_window_only"
				uncertainties = append(uncertainties, "existing capture params do not match requested params; using time_window_only mode")
			}
			captureStatus = "running"
		} else {
			isolationMode = "time_window_only"
			captureStatus = "not_started"
			uncertainties = append(uncertainties, "capture status query failed: "+grpcErr.Error())
		}
	} else {
		// 无运行中的 capture
		if pluginName != "" && port != 0 {
			// auto_start 模式：提示用户先调用 start_capture
			// 注：直接复用 handleStartCapture 需要构造 mcp.CallToolRequest，类型系统复杂，
			// 简化为提示用户手动启动，保持 run 窗口工具与 capture 工具解耦。
			isolationMode = "time_window_only"
			captureStatus = "not_started"
			uncertainties = append(uncertainties,
				"no running capture; please call start_capture with plugin="+pluginName+
					fmt.Sprintf(" port=%d first, then call begin_capture_run again", port))
		} else {
			// 无足够参数 auto_start
			isolationMode = "time_window_only"
			captureStatus = "not_started"
			uncertainties = append(uncertainties, "no running capture and insufficient params for auto_start; using time_window_only mode")
		}
	}

	// 如果没有 sessionID，使用当前 session 或生成临时 ID
	if sessionID == "" {
		if current != nil {
			sessionID = current.SessionID
		} else {
			sessionID = m.sessionMgr.generateSessionID()
		}
	}

	runID := m.runRegistry.GenerateRunID(sessionID)
	now := time.Now()

	rec := RunRecord{
		RunID:         runID,
		SessionID:     sessionID,
		FeatureName:   featureName,
		ProjectPath:   projectPath,
		PluginName:    pluginName,
		Device:        device,
		Filter:        filter,
		Port:          port,
		TimeFrom:      now,
		IsolationMode: isolationMode,
		CaptureStatus: captureStatus,
		Baseline:      baseline,
	}

	if err := m.runRegistry.Begin(rec); err != nil {
		return errorResult(err), nil
	}

	slog.Info("begin_capture_run", "run_id", runID, "session_id", sessionID, "feature", featureName, "isolation", isolationMode)

	result := map[string]any{
		"run_id":                 runID,
		"time_from":              now.Format(time.RFC3339Nano),
		"capture_status":         captureStatus,
		"capture_isolation_mode": isolationMode,
		"session_id":             sessionID,
	}
	if len(uncertainties) > 0 {
		result["uncertainties"] = uncertainties
	}
	return successResult(result), nil
}

// handleEndCaptureRun 关闭当前操作窗口。
func (m *mcpCapture) handleEndCaptureRun(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.runRegistry == nil {
		return errorResult(fmt.Errorf("run registry not initialized")), nil
	}

	runID, err := req.RequireString("run_id")
	if err != nil {
		return errorResult(err), nil
	}

	rec, err := m.runRegistry.Get(runID)
	if err != nil {
		return errorResult(err), nil
	}
	if rec.Ended {
		// 幂等：返回已存在的 summary
		slog.Info("end_capture_run idempotent", "run_id", runID)
		return successResult(map[string]any{
			"run_id":      rec.RunID,
			"time_to":     rec.TimeTo.Format(time.RFC3339Nano),
			"duration_ms": rec.DurationMs,
			"summary":     rec.Summary,
			"idempotent":  true,
		}), nil
	}

	// now 默认取当前时间；若调用方传入 time_to（RFC3339Nano），则使用指定时间作为窗口上界。
	// 这主要用于测试场景注入精确时间，生产场景一般不传。
	now := time.Now()
	if timeToStr := req.GetString("time_to", ""); timeToStr != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, timeToStr); err == nil {
			now = parsed.Local()
		}
	}

	// 计算 window 内增量
	// 通过 gRPC 获取当前计数计算 delta
	var summary RunSummary
	captureRunning := false
	current, err := m.sessionMgr.readCurrent(auth.OwnerFrom(ctx))
	if err == nil && current != nil && current.Status == "running" && current.SessionID == rec.SessionID && m.pipelineClient != nil {
		captureRunning = true
		resp, grpcErr := m.pipelineClient.GetCaptureStatus(ctx, &pb.GetCaptureStatusRequest{SessionId: rec.SessionID})
		if grpcErr == nil {
			summary.DecodeErrorCount = resp.GetDecodeErrors() - rec.Baseline.DecodeErrors
			summary.CapturedMessageCount = resp.GetEventCount() - rec.Baseline.Events
		} else {
			summary.DecodeErrorCount = -1
			summary.CapturedMessageCount = -1
			captureRunning = false // gRPC 失败，回退到 db 查询
		}
	} else {
		summary.DecodeErrorCount = -1
		summary.CapturedMessageCount = -1
	}

	// 查询 db 获取 flow_count / request_count / server_message_count
	reader, err := m.openReader(ctx, rec.SessionID)
	if err == nil {
		defer reader.Close()

		// 查询 events 表
		events, qerr := reader.QueryEvents(ctx, rec.SessionID, 100000, 0)
		if qerr == nil {
			// 统计时间窗口内的事件
			flowIDs := make(map[string]bool)
			clientCount := 0
			serverCount := 0
			totalCount := 0

			for _, ev := range events {
				// 时间窗口过滤
				if ev.Identity.Timestamp.Before(rec.TimeFrom) || ev.Identity.Timestamp.After(now) {
					continue
				}
				totalCount++

				// 提取 flow_id（优先从 Context，回退到 Payload）
				flowIDValue := ev.Context.FlowID
				if flowIDValue == "" {
					if payloadObj, ok := ev.Payload.Value.AsObject(); ok {
						if flowIDVal, exists := payloadObj["flow_id"]; exists {
							if s, ok := flowIDVal.AsString(); ok {
								flowIDValue = s
							}
						}
					}
				}
				if flowIDValue != "" {
					flowIDs[flowIDValue] = true
				}

				// 提取 direction（优先从 Context，回退到 Payload）
				direction := ev.Context.Direction
				if direction == "" {
					if payloadObj, ok := ev.Payload.Value.AsObject(); ok {
						if dirVal, exists := payloadObj["direction"]; exists {
							if d, ok := dirVal.AsString(); ok {
								direction = d
							}
						}
					}
				}
				if direction == "client_to_server" {
					clientCount++
				} else if direction == "server_to_client" {
					serverCount++
				}
			}

			summary.CapturedFlowCount = int64(len(flowIDs))
			summary.ClientRequestCount = int64(clientCount)
			summary.ServerMessageCount = int64(serverCount)
			if !captureRunning {
				summary.CapturedMessageCount = int64(totalCount)
			}
		} else {
			// 查询失败
			summary.CapturedFlowCount = -1
			summary.ClientRequestCount = -1
			summary.ServerMessageCount = -1
		}
	} else {
		// reader 不可用
		summary.CapturedFlowCount = -1
		summary.ClientRequestCount = -1
		summary.ServerMessageCount = -1
	}

	if err := m.runRegistry.End(runID, now, summary); err != nil {
		return errorResult(err), nil
	}

	slog.Info("end_capture_run", "run_id", runID, "duration_ms", now.Sub(rec.TimeFrom).Milliseconds())

	return successResult(map[string]any{
		"run_id":      runID,
		"time_to":     now.Format(time.RFC3339Nano),
		"duration_ms": now.Sub(rec.TimeFrom).Milliseconds(),
		"summary":     summary,
	}), nil
}

// handleGetRunStatus 快速检查 run 是否有有用数据。
func (m *mcpCapture) handleGetRunStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.runRegistry == nil {
		return errorResult(fmt.Errorf("run registry not initialized")), nil
	}

	runID, err := req.RequireString("run_id")
	if err != nil {
		return errorResult(err), nil
	}

	rec, err := m.runRegistry.Get(runID)
	if err != nil {
		return errorResult(err), nil
	}

	status := "stopped"
	if !rec.Ended {
		status = "active"
	}

	result := map[string]any{
		"run_id":    rec.RunID,
		"status":    status,
		"time_from": rec.TimeFrom.Format(time.RFC3339Nano),
	}

	if rec.Summary != nil {
		result["flow_count"] = rec.Summary.CapturedFlowCount
		result["client_request_count"] = rec.Summary.ClientRequestCount
		result["server_message_count"] = rec.Summary.ServerMessageCount
		result["decode_error_count"] = rec.Summary.DecodeErrorCount
	} else {
		// active run，实时查询
		result["flow_count"] = -1
		result["client_request_count"] = -1
		result["server_message_count"] = -1
		current, err := m.sessionMgr.readCurrent(auth.OwnerFrom(ctx))
		if err == nil && current != nil && current.Status == "running" && current.SessionID == rec.SessionID && m.pipelineClient != nil {
			resp, grpcErr := m.pipelineClient.GetCaptureStatus(ctx, &pb.GetCaptureStatusRequest{SessionId: rec.SessionID})
			if grpcErr == nil {
				result["decode_error_count"] = resp.GetDecodeErrors() - rec.Baseline.DecodeErrors
			} else {
				result["decode_error_count"] = -1
			}
		} else {
			result["decode_error_count"] = -1
		}
	}

	return successResult(result), nil
}
