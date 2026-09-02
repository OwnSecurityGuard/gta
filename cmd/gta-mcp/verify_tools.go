package main

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	pb "gta/pkg/internalipc/proto"
)

// handleVerifyPlugin 用指定插件对离线会话的 raw_packets 解码并做契约+质量校验，
// 产出 violations（引 SDK checker，带 rule_id）+ quality（gta 统计）+ verdict。
// 纯转发到 Runtime Plane（gta-pipeline）；MCP 自身零归因逻辑、零 exec、零 os.WriteFile。
func (m *mcpCapture) handleVerifyPlugin(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := req.GetString("session_id", "")
	pluginName := req.GetString("plugin", "")
	protocol := req.GetString("protocol", "")
	src := req.GetString("src", "")
	dst := req.GetString("dst", "")
	limit := req.GetInt("limit", 0)

	if sessionID == "" {
		return errorResult(fmt.Errorf("session_id is required")), nil
	}
	if pluginName == "" {
		return errorResult(fmt.Errorf("plugin is required")), nil
	}
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}

	resp, err := m.pipelineClient.Verify(ctx, &pb.VerifyRequest{
		SessionId: sessionID,
		Plugin:    pluginName,
		Protocol:  protocol,
		Src:       src,
		Dst:       dst,
		Limit:     int64(limit),
	})
	if err != nil {
		return errorResult(fmt.Errorf("verify: %w", err)), nil
	}

	out := map[string]any{
		"status":        "verified",
		"session_id":    sessionID,
		"plugin":        pluginName,
		"verdict":       resp.GetVerdict(),
		"verify_run_id": resp.GetVerifyRunId(),
		"violations":    resp.GetViolations(),
		"quality":       resp.GetQuality(),
	}
	return successResult(out), nil
}

// handleSampleBytesPlugin 读取会话原始包前若干字节（事实），并在 plugin_debug_access
// 留审计。纯转发到 Runtime Plane；MCP 零读取、零落库、零归因。
func (m *mcpCapture) handleSampleBytesPlugin(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := req.GetString("session_id", "")
	pluginName := req.GetString("plugin", "")
	limit := req.GetInt("limit", 0)
	maxBytes := req.GetInt("max_bytes", 0)

	if sessionID == "" {
		return errorResult(fmt.Errorf("session_id is required")), nil
	}
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}

	resp, err := m.pipelineClient.SampleBytes(ctx, &pb.SampleBytesRequest{
		SessionId: sessionID,
		Plugin:    pluginName,
		Limit:     int64(limit),
		MaxBytes:  int32(maxBytes),
	})
	if err != nil {
		return errorResult(fmt.Errorf("sample_bytes: %w", err)), nil
	}

	out := map[string]any{
		"status":                  "sampled",
		"session_id":              sessionID,
		"requested_packets":       resp.GetRequestedPackets(),
		"returned_packets":        resp.GetReturnedPackets(),
		"returned_bytes":          resp.GetReturnedBytes(),
		"truncated":               resp.GetTruncated(),
		"mean_entropy":            resp.GetMeanEntropy(),
		"length_histogram":        resp.GetLengthHistogram(),
		"first_byte_distribution": resp.GetFirstByteDistribution(),
		"packets":                 resp.GetPackets(),
		"audit_id":                resp.GetAuditId(),
	}
	return successResult(out), nil
}
