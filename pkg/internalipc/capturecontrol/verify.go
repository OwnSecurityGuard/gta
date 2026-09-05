package capturecontrol

import (
	"context"

	pb "gametrace/pkg/internalipc/proto"
)

// VerifyRequest 是 plugin.verify 的请求（契约+质量校验）。参数与 TestPlugin 同源。
type VerifyRequest struct {
	SessionID string
	Plugin    string
	Protocol  string
	Src       string
	Dst       string
	Limit     int64 // 0 = 全部
}

// ViolationView 单条契约违规（源自 SDK contract checker，带 rule_id）。
type ViolationView struct {
	RuleID    string
	Topic     string
	Severity  string
	Statement string
	DocRef    string
	Count     int
	Sample    string
}

// QualityView 是 gametrace 侧语料级统计（与 plugin.explain 共享判据）。
type QualityView struct {
	TotalInputs          int
	UnknownInputs        int
	UnknownRatio         float64
	CorrelatedInputs     int
	LongPacketErrors     int
	EntropyEstimate      float64
	SchemaVersionedRatio float64
	DecodeErrors         int
}

// VerifyResult 是 plugin.verify 的结论：violations + quality + verdict + 溯源。
type VerifyResult struct {
	Verdict     string
	Violations  []ViolationView
	Quality     QualityView
	VerifyRunID string
	SessionID   string
	AtUnix      int64
}

// SampleBytesRequest 是 plugin.sample_bytes 的取证取样请求。
type SampleBytesRequest struct {
	SessionID string
	Plugin    string // 仅用于审计 actor 标注
	Limit     int64  // 请求包上限（服务端封顶 20）
	MaxBytes  int32  // 每包字节上限（服务端封顶 64）
}

// SampledPacket 单个原始包的事实快照（无解释，仅事实）。
type SampledPacket struct {
	RawPacketID string
	Src         string
	Dst         string
	Length      int64
	Hex         string // 截断后的 hexdump
	Entropy     float64
	FirstByte   int32
}

// SampleBytesResult 是取证事实 + 审计回执。
type SampleBytesResult struct {
	SessionID        string
	RequestedPackets int64
	ReturnedPackets  int64
	ReturnedBytes    int64
	Truncated        bool
	Packets          []SampledPacket
	LengthHistogram  map[int32]int64 // 长度桶(16B) → 计数
	FirstByteDist    map[int32]int64 // 首字节 → 计数
	MeanEntropy      float64
	AuditID          int64
}

// Verify 处理插件验证 RPC，委托引擎执行，返回 violations + quality + verdict。
func (s *Server) Verify(ctx context.Context, req *pb.VerifyRequest) (*pb.VerifyResponse, error) {
	res, err := s.engine.Verify(ctx, VerifyRequest{
		SessionID: req.GetSessionId(),
		Plugin:    req.GetPlugin(),
		Protocol:  req.GetProtocol(),
		Src:       req.GetSrc(),
		Dst:       req.GetDst(),
		Limit:     req.GetLimit(),
	})
	if err != nil {
		return nil, err
	}
	vr := &pb.VerifyResponse{
		Verdict:     res.Verdict,
		VerifyRunId: res.VerifyRunID,
		SessionId:   res.SessionID,
		AtUnix:      res.AtUnix,
	}
	for _, v := range res.Violations {
		vr.Violations = append(vr.Violations, &pb.VerifyViolation{
			RuleId:    v.RuleID,
			Topic:     v.Topic,
			Severity:  v.Severity,
			Statement: v.Statement,
			DocRef:    v.DocRef,
			Count:     int32(v.Count),
			Sample:    v.Sample,
		})
	}
	vr.Quality = &pb.VerifyQuality{
		TotalInputs:          int64(res.Quality.TotalInputs),
		UnknownInputs:        int64(res.Quality.UnknownInputs),
		UnknownRatio:         res.Quality.UnknownRatio,
		CorrelatedInputs:     int64(res.Quality.CorrelatedInputs),
		LongPacketErrors:     int64(res.Quality.LongPacketErrors),
		EntropyEstimate:      res.Quality.EntropyEstimate,
		SchemaVersionedRatio: res.Quality.SchemaVersionedRatio,
		DecodeErrors:         int64(res.Quality.DecodeErrors),
	}
	return vr, nil
}

// SampleBytes 处理取证取样 RPC，委托引擎读取原始包并写审计。
func (s *Server) SampleBytes(ctx context.Context, req *pb.SampleBytesRequest) (*pb.SampleBytesResponse, error) {
	res, err := s.engine.SampleBytes(ctx, SampleBytesRequest{
		SessionID: req.GetSessionId(),
		Plugin:    req.GetPlugin(),
		Limit:     req.GetLimit(),
		MaxBytes:  req.GetMaxBytes(),
	})
	if err != nil {
		return nil, err
	}
	resp := &pb.SampleBytesResponse{
		SessionId:             res.SessionID,
		RequestedPackets:      res.RequestedPackets,
		ReturnedPackets:       res.ReturnedPackets,
		ReturnedBytes:         res.ReturnedBytes,
		Truncated:             res.Truncated,
		MeanEntropy:           res.MeanEntropy,
		AuditId:               res.AuditID,
		LengthHistogram:       map[int32]int64{},
		FirstByteDistribution: map[int32]int64{},
	}
	for _, p := range res.Packets {
		resp.Packets = append(resp.Packets, &pb.SampledPacket{
			RawPacketId: p.RawPacketID,
			Src:         p.Src,
			Dst:         p.Dst,
			Length:      p.Length,
			Hex:         p.Hex,
			Entropy:     p.Entropy,
			FirstByte:   p.FirstByte,
		})
	}
	for k, v := range res.LengthHistogram {
		resp.LengthHistogram[k] = v
	}
	for k, v := range res.FirstByteDist {
		resp.FirstByteDistribution[k] = v
	}
	return resp, nil
}
