package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"gta/pkg/decode"
	"gta/pkg/internalipc/capturecontrol"
	"gta/pkg/plugindev"
	"gta/pkg/plugin/quality"
	"gta/pkg/store"
)

// sampleBytesHardCapPackets / sampleBytesHardCapBytes 是 plugin.sample_bytes 的
// 硬上限（设计 §6），不可通过参数突破。取证只给有限的事实窗口。
const (
	sampleBytesHardCapPackets = 20
	sampleBytesHardCapBytes   = 64
)

// Verify 用指定插件对离线会话的 raw_packets 解码并做契约+质量校验。
//
// 它是 Runtime Plane 的 verify 执行（设计 §1.3 / §7）：拥有真实流量与 registry，
// 调度解码、产出语料，再交给 quality.Verify 合并 SDK 违规与 gta 统计得 verdict。
// 完成后把 validated 证明回写 Developer Plane 的 Tracker（跨平面；plugindev 在本
// 进程内嵌，故直接进程内调用），使 plugin.status 的 artifact.state 升到 validated。
func (s *pipelineService) Verify(ctx context.Context, req capturecontrol.VerifyRequest) (capturecontrol.VerifyResult, error) {
	if req.SessionID == "" {
		return capturecontrol.VerifyResult{}, fmt.Errorf("session_id is required")
	}
	if req.Plugin == "" {
		return capturecontrol.VerifyResult{}, fmt.Errorf("plugin is required")
	}
	logger := s.logger.With("session_id", req.SessionID, "plugin", req.Plugin, "op", "verify")

	meta, err := s.controlStore.GetSession(ctx, req.SessionID)
	if err != nil {
		return capturecontrol.VerifyResult{}, fmt.Errorf("get session: %w", err)
	}
	if meta == nil || meta.DBPath == "" {
		return capturecontrol.VerifyResult{}, fmt.Errorf("session %s not found or has no db_path", req.SessionID)
	}

	client, schemaReg, ok := s.registry.FindByName(req.Plugin)
	if !ok {
		client, schemaReg, ok = s.registry.Find(req.Plugin)
	}
	if !ok {
		return capturecontrol.VerifyResult{}, fmt.Errorf("plugin %s not found or not a decoder", req.Plugin)
	}

	st, err := store.NewSQLiteStoreReadOnly(meta.DBPath, schemaReg)
	if err != nil {
		return capturecontrol.VerifyResult{}, fmt.Errorf("open sqlite: %w", err)
	}
	defer st.Close()

	dispatcher, err := decode.NewDispatcher(client, req.SessionID, logger, schemaReg, decode.WithServerPort(meta.Port))
	if err != nil {
		return capturecontrol.VerifyResult{}, fmt.Errorf("new dispatcher: %w", err)
	}
	defer dispatcher.Close()

	var corpus []quality.DecodeIO
	loopErr := forEachRawDecoded(ctx, st, dispatcher, decodeRawOptions{
		Protocol: req.Protocol,
		Src:      req.Src,
		Dst:      req.Dst,
		Limit:    req.Limit,
	}, func(r rawDecodeResult) {
		corpus = append(corpus, decodeIOsFromResult(r)...)
	})
	if loopErr != nil {
		return capturecontrol.VerifyResult{}, loopErr
	}

	result := quality.Verify(corpus)

	// 跨平面回写：把 validated 证明落到 Developer Plane 的 Tracker。
	runID := fmt.Sprintf("verify_%d", time.Now().UnixNano())
	plugindev.DefaultTracker().SetValidated(req.Plugin, &plugindev.ValidatedProof{
		VerifyRunID: runID,
		SessionID:   req.SessionID,
		Verdict:     result.Verdict,
		At:          time.Now(),
	})
	plugindev.RecordVerify(req.Plugin, result)

	out := capturecontrol.VerifyResult{
		Verdict:     result.Verdict,
		VerifyRunID: runID,
		SessionID:   req.SessionID,
		AtUnix:      time.Now().Unix(),
	}
	for _, v := range result.Violations {
		out.Violations = append(out.Violations, capturecontrol.ViolationView{
			RuleID:    v.RuleID,
			Topic:     v.Topic,
			Severity:  v.Severity,
			Statement: v.Statement,
			DocRef:    v.DocRef,
			Count:     v.Count,
			Sample:    v.Sample,
		})
	}
	if q := result.Quality; q != nil {
		out.Quality = capturecontrol.QualityView{
			TotalInputs:          q.TotalInputs,
			UnknownInputs:        q.UnknownInputs,
			UnknownRatio:         q.UnknownRatio,
			CorrelatedInputs:     q.CorrelatedInputs,
			LongPacketErrors:     q.LongPacketErrors,
			EntropyEstimate:      q.EntropyEstimate,
			SchemaVersionedRatio: q.SchemaVersionedRatio,
			DecodeErrors:         q.DecodeErrors,
		}
	}
	logger.Info("verify completed", "verdict", out.Verdict, "violations", len(out.Violations), "corpus", len(corpus))
	return out, nil
}

// SampleBytes 读取会话原始包的前若干字节（事实：hexdump / 长度直方图 / 首字节分布 /
// 熵），不做任何解释，并在 plugin_debug_access 留审计（设计 §6）。
//
// 硬上限（20 包 / 64 字节）不可通过参数突破；审计记真实返回量，截断后数据不假。
func (s *pipelineService) SampleBytes(ctx context.Context, req capturecontrol.SampleBytesRequest) (capturecontrol.SampleBytesResult, error) {
	if req.SessionID == "" {
		return capturecontrol.SampleBytesResult{}, fmt.Errorf("session_id is required")
	}
	logger := s.logger.With("session_id", req.SessionID, "op", "sample_bytes")

	meta, err := s.controlStore.GetSession(ctx, req.SessionID)
	if err != nil {
		return capturecontrol.SampleBytesResult{}, fmt.Errorf("get session: %w", err)
	}
	if meta == nil || meta.DBPath == "" {
		return capturecontrol.SampleBytesResult{}, fmt.Errorf("session %s not found or has no db_path", req.SessionID)
	}

	limit := req.Limit
	if limit <= 0 || limit > sampleBytesHardCapPackets {
		limit = sampleBytesHardCapPackets
	}
	maxBytes := int(req.MaxBytes)
	if maxBytes <= 0 || maxBytes > sampleBytesHardCapBytes {
		maxBytes = sampleBytesHardCapBytes
	}

	// 只读打开；schemaReg 取默认空 registry（仅读 raw_packets，无需 schema）。
	st, err := store.NewSQLiteStoreReadOnly(meta.DBPath, nil)
	if err != nil {
		return capturecontrol.SampleBytesResult{}, fmt.Errorf("open sqlite: %w", err)
	}
	defer st.Close()

	// 多取一条以判断是否被硬上限截断。
	rows, err := st.QueryRawPackets(ctx, store.RawPacketQuery{Limit: int(limit) + 1, Offset: 0})
	if err != nil {
		return capturecontrol.SampleBytesResult{}, fmt.Errorf("query raw packets: %w", err)
	}
	truncated := int64(len(rows)) > limit
	if truncated {
		rows = rows[:limit]
	}

	out := capturecontrol.SampleBytesResult{
		SessionID:        req.SessionID,
		RequestedPackets: limit,
		ReturnedPackets:  int64(len(rows)),
		LengthHistogram:  map[int32]int64{},
		FirstByteDist:    map[int32]int64{},
	}
	var entropySum float64
	for _, r := range rows {
		payload := r.Payload
		if len(payload) > maxBytes {
			payload = payload[:maxBytes]
		}
		out.ReturnedBytes += int64(len(payload))
		ent := shannonBits(payload)
		entropySum += ent
		var firstByte int32
		if len(r.Payload) > 0 {
			firstByte = int32(r.Payload[0])
		}
		out.Packets = append(out.Packets, capturecontrol.SampledPacket{
			RawPacketID: r.ID,
			Src:         r.Src,
			Dst:         r.Dst,
			Length:      int64(len(r.Payload)),
			Hex:        hex.EncodeToString(payload),
			Entropy:    ent,
			FirstByte:  firstByte,
		})
		bucket := int32((len(r.Payload) / 16) * 16)
		out.LengthHistogram[bucket]++
		if len(r.Payload) > 0 {
			out.FirstByteDist[firstByte]++
		}
	}
	if n := len(rows); n > 0 {
		out.MeanEntropy = entropySum / float64(n)
	}

	// 审计：写入方唯一为 Runtime Plane（pipeline），记真实返回量。
	auditID, aerr := s.controlStore.RecordDebugAccess(ctx, store.DebugAccess{
		Actor:            "pipeline",
		Tool:             "sample_bytes",
		Plugin:           req.Plugin,
		SessionID:        req.SessionID,
		RequestedPackets: limit,
		ReturnedPackets:  out.ReturnedPackets,
		ReturnedBytes:    out.ReturnedBytes,
		Truncated:        truncated,
	})
	if aerr != nil {
		logger.Warn("record debug access", "error", aerr)
	}
	out.AuditID = auditID
	out.Truncated = truncated

	logger.Info("sample_bytes completed",
		"returned_packets", out.ReturnedPackets, "returned_bytes", out.ReturnedBytes,
		"truncated", truncated, "audit_id", auditID)
	return out, nil
}

// decodeIOsFromResult 把一个原始包的解码结果展开为 quality.DecodeIO 序列：
// 每个产出的事件对应一个非终结响应（带 event_type/schema_id/payload），
// 再追加一个终结响应（done=true，承载原始字节供熵估计）。解码失败则仅一个
// done=true 且带 DecodeError 的响应。
func decodeIOsFromResult(r rawDecodeResult) []quality.DecodeIO {
	if r.Err != nil {
		return []quality.DecodeIO{{
			InputID:     r.RawID,
			Done:        true,
			DecodeError: r.Err.Error(),
			Payload:     r.Payload,
		}}
	}
	var ios []quality.DecodeIO
	for _, ev := range r.Events {
		ios = append(ios, quality.DecodeIO{
			InputID:    r.RawID,
			Done:       false,
			EventType:  string(ev.Identity.Type),
			SchemaID:   ev.Payload.SchemaID,
			PayloadLen: 1, // 事件存在即代表响应带非空 payload
			Correlated: ev.Relation.CorrelationID != "",
		})
	}
	// 终结响应：承载原始字节供熵估计（不重复计入未知率）。
	ios = append(ios, quality.DecodeIO{InputID: r.RawID, Done: true, Payload: r.Payload})
	return ios
}

// shannonBits 返回 b 的香农熵（bits/byte，0..8）。
func shannonBits(b []byte) float64 {
	if len(b) == 0 {
		return 0
	}
	var freq [256]float64
	for _, c := range b {
		freq[c]++
	}
	var h float64
	n := float64(len(b))
	for _, f := range freq {
		if f == 0 {
			continue
		}
		p := f / n
		h -= p * math.Log2(p)
	}
	return h
}
