package main

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/grpc"
	pb "gametrace/pkg/internalipc/proto"
)

// fakeCaptureClient 是 pb.CaptureControlClient 的轻量桩：仅覆写 Verify 与
// SampleBytes，其余接口方法由嵌入的 nil 接口提供（测试不会调用）。
type fakeCaptureClient struct {
	pb.CaptureControlClient
	verifyReq      *pb.VerifyRequest
	verifyResp     *pb.VerifyResponse
	verifyErr      error
	sampleReq      *pb.SampleBytesRequest
	sampleResp     *pb.SampleBytesResponse
	sampleErr      error
	startReq       *pb.StartCaptureRequest
	dbDir          string
	listPluginsReq *pb.ListPluginsRequest
	manifestReq    *pb.GetPluginManifestRequest
	// liveSessions 预设 ListCaptureSessions 的返回值（nil = 无 live 会话）。
	liveSessions *pb.ListCaptureSessionsResponse
}

func (f *fakeCaptureClient) Verify(ctx context.Context, in *pb.VerifyRequest, _ ...grpc.CallOption) (*pb.VerifyResponse, error) {
	f.verifyReq = in
	return f.verifyResp, f.verifyErr
}
func (f *fakeCaptureClient) SampleBytes(ctx context.Context, in *pb.SampleBytesRequest, _ ...grpc.CallOption) (*pb.SampleBytesResponse, error) {
	f.sampleReq = in
	return f.sampleResp, f.sampleErr
}

// TestHandleVerifyPluginForwards locks in P4: the MCP layer is a pure forwarder
// to the Runtime Plane — it maps arguments onto the gRPC VerifyRequest and
// relays the verdict/violations back. No attribution logic lives here.
func TestHandleVerifyPluginForwards(t *testing.T) {
	fc := &fakeCaptureClient{
		verifyResp: &pb.VerifyResponse{
			Verdict:     "warn",
			VerifyRunId: "verify_1",
			SessionId:   "s1",
			Violations:  []*pb.VerifyViolation{{RuleId: "payload-non-empty", Severity: "error", Count: 2}},
			Quality:     &pb.VerifyQuality{TotalInputs: 10, UnknownInputs: 5},
		},
	}
	m := &mcpCapture{pipelineClient: fc}
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"session_id": "s1", "plugin": "http", "limit": 5}

	res, err := m.handleVerifyPlugin(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if fc.verifyReq == nil || fc.verifyReq.GetSessionId() != "s1" ||
		fc.verifyReq.GetPlugin() != "http" || fc.verifyReq.GetLimit() != 5 {
		t.Fatalf("verify not forwarded correctly: %+v", fc.verifyReq)
	}
	text := res.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "warn") || !strings.Contains(text, "payload-non-empty") {
		t.Fatalf("response missing verdict/rule_id: %s", text)
	}
}

func TestHandleVerifyPluginMissingArgs(t *testing.T) {
	fc := &fakeCaptureClient{}
	m := &mcpCapture{pipelineClient: fc}
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"plugin": "http"} // missing session_id

	res, _ := m.handleVerifyPlugin(context.Background(), req)
	if fc.verifyReq != nil {
		t.Fatal("should not call pipeline when session_id is missing")
	}
	if !strings.Contains(res.Content[0].(mcp.TextContent).Text, "session_id is required") {
		t.Fatalf("expected required-arg error")
	}
}

// TestHandleSampleBytesPluginForwards locks in P4: sample_bytes is a pure
// forwarder; the response carries the audit_id so the caller can prove the
// access was recorded (design §6).
func TestHandleSampleBytesPluginForwards(t *testing.T) {
	fc := &fakeCaptureClient{
		sampleResp: &pb.SampleBytesResponse{
			SessionId:        "s1",
			RequestedPackets: 20,
			ReturnedPackets:  3,
			ReturnedBytes:    64,
			Truncated:        true,
			MeanEntropy:      5.5,
			AuditId:          42,
			Packets:          []*pb.SampledPacket{{RawPacketId: "r1"}},
			LengthHistogram:  map[int32]int64{64: 3},
		},
	}
	m := &mcpCapture{pipelineClient: fc}
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"session_id": "s1", "plugin": "http", "limit": 20, "max_bytes": 64}

	res, err := m.handleSampleBytesPlugin(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if fc.sampleReq == nil || fc.sampleReq.GetSessionId() != "s1" ||
		fc.sampleReq.GetLimit() != 20 || fc.sampleReq.GetMaxBytes() != 64 {
		t.Fatalf("sample_bytes not forwarded correctly: %+v", fc.sampleReq)
	}
	text := res.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "audit_id") || !strings.Contains(text, "42") {
		t.Fatalf("response missing audit_id: %s", text)
	}
}
