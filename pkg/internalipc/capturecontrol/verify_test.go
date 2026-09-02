package capturecontrol

import (
	"context"
	"testing"

	pb "gta/pkg/internalipc/proto"
)

func TestServer_Verify(t *testing.T) {
	engine := &fakeEngine{
		verifyResult: VerifyResult{
			Verdict:     "warn",
			VerifyRunID: "verify_1",
			SessionID:   "s1",
			AtUnix:      123,
			Violations: []ViolationView{
				{RuleID: "payload-non-empty", Topic: "encoding", Severity: "error", Count: 2, Sample: "schema_id empty"},
			},
			Quality: QualityView{
				TotalInputs: 10, UnknownInputs: 5, UnknownRatio: 0.5,
				CorrelatedInputs: 1, EntropyEstimate: 7.9, DecodeErrors: 0,
			},
		},
	}
	srv := NewServer(engine)
	resp, err := srv.Verify(context.Background(), &pb.VerifyRequest{
		SessionId: "s1", Plugin: "http", Protocol: "tcp", Limit: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetVerdict() != "warn" {
		t.Errorf("verdict = %q, want warn", resp.GetVerdict())
	}
	if resp.GetVerifyRunId() != "verify_1" || resp.GetSessionId() != "s1" || resp.GetAtUnix() != 123 {
		t.Errorf("unexpected identifiers: run=%q session=%q at=%d", resp.GetVerifyRunId(), resp.GetSessionId(), resp.GetAtUnix())
	}
	if len(resp.GetViolations()) != 1 {
		t.Fatalf("violations = %d, want 1", len(resp.GetViolations()))
	}
	v := resp.GetViolations()[0]
	if v.GetRuleId() != "payload-non-empty" || v.GetSeverity() != "error" || v.GetCount() != 2 {
		t.Errorf("violation = %+v", v)
	}
	q := resp.GetQuality()
	if q.GetTotalInputs() != 10 || q.GetUnknownInputs() != 5 || q.GetUnknownRatio() != 0.5 || q.GetEntropyEstimate() != 7.9 {
		t.Errorf("quality = %+v", q)
	}
	// 请求参数正确传到引擎。
	if engine.verifyLastReq.SessionID != "s1" || engine.verifyLastReq.Plugin != "http" ||
		engine.verifyLastReq.Protocol != "tcp" || engine.verifyLastReq.Limit != 7 {
		t.Errorf("engine req = %+v", engine.verifyLastReq)
	}
}

func TestServer_SampleBytes(t *testing.T) {
	engine := &fakeEngine{
		sampleResult: SampleBytesResult{
			SessionID:        "s1",
			RequestedPackets: 20,
			ReturnedPackets:  3,
			ReturnedBytes:    64,
			Truncated:        true,
			MeanEntropy:      5.5,
			AuditID:          42,
			Packets: []SampledPacket{
				{RawPacketID: "r1", Src: "1.1.1.1", Dst: "2.2.2.2", Length: 64, Hex: "ab", Entropy: 5.5, FirstByte: 1},
			},
			LengthHistogram: map[int32]int64{64: 3},
			FirstByteDist:   map[int32]int64{1: 3},
		},
	}
	srv := NewServer(engine)
	resp, err := srv.SampleBytes(context.Background(), &pb.SampleBytesRequest{
		SessionId: "s1", Plugin: "http", Limit: 20, MaxBytes: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetReturnedPackets() != 3 || resp.GetReturnedBytes() != 64 || !resp.GetTruncated() {
		t.Errorf("counts = pkts=%d bytes=%d trunc=%v", resp.GetReturnedPackets(), resp.GetReturnedBytes(), resp.GetTruncated())
	}
	if resp.GetMeanEntropy() != 5.5 {
		t.Errorf("mean_entropy = %v, want 5.5", resp.GetMeanEntropy())
	}
	if resp.GetAuditId() != 42 {
		t.Errorf("audit_id = %d, want 42", resp.GetAuditId())
	}
	if len(resp.GetPackets()) != 1 || resp.GetPackets()[0].GetRawPacketId() != "r1" {
		t.Errorf("packets = %+v", resp.GetPackets())
	}
	if resp.GetLengthHistogram()[64] != 3 || resp.GetFirstByteDistribution()[1] != 3 {
		t.Errorf("histograms = len=%v fbd=%v", resp.GetLengthHistogram(), resp.GetFirstByteDistribution())
	}
	if engine.sampleLastReq.SessionID != "s1" || engine.sampleLastReq.Limit != 20 || engine.sampleLastReq.MaxBytes != 64 {
		t.Errorf("engine req = %+v", engine.sampleLastReq)
	}
}
