package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"time"

	"gta/pkg/auth"
	"gta/pkg/decode"
	"gta/pkg/event"
	"gta/pkg/internalipc/capturecontrol"
	"gta/pkg/state"
	"gta/pkg/store"
)

// rawBatchSize 离线解码时分批读取 raw_packets 的批大小。
// 避免一次性把全部 raw packets 加载进内存。
const rawBatchSize = 1000

// testPluginDefaultSample 是 test_plugin 返回的解码事件采样上限默认值。
const testPluginDefaultSample int64 = 50

// testPluginMaxDataJSON 是单个采样事件 data_json 的最大字节数（预览截断，避免大载荷爆炸）。
const testPluginMaxDataJSON = 4096

// decodeRawOptions 控制离线解码循环的过滤与上限。
type decodeRawOptions struct {
	Protocol string
	Src      string
	Dst      string
	Limit    int64 // 0 = 全部
}

// rawDecodeResult 是单个原始包的解码结果（成功携带事件，失败携带错误）。
type rawDecodeResult struct {
	RawID  string
	Src    string
	Dst    string
	Events []*event.Event
	Err    error
	// Payload 是该原始包的未解码字节，仅供进程内统计（熵估计）使用，
	// 绝不外传前端（隐私安全，见 TestPlugin 注释）。
	Payload []byte
}

// forEachRawDecoded 分批读取会话的 raw_packets 并在进程内解码，
// 对每个原始包调用 onResult。原始字节仅在此函数内存在，绝不外传前端。
// opts.Limit>0 时最多处理 Limit 个原始包。
func forEachRawDecoded(ctx context.Context, st *store.SQLiteStore, dispatcher *decode.Dispatcher, opts decodeRawOptions, onResult func(rawDecodeResult)) error {
	offset := 0
	var totalRaw int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batchLimit := rawBatchSize
		if opts.Limit > 0 {
			remaining := opts.Limit - totalRaw
			if remaining <= 0 {
				break
			}
			if remaining < int64(batchLimit) {
				batchLimit = int(remaining)
			}
		}
		rows, qerr := st.QueryRawPackets(ctx, store.RawPacketQuery{
			Protocol: opts.Protocol,
			Src:      opts.Src,
			Dst:      opts.Dst,
			Limit:    batchLimit,
			Offset:   offset,
		})
		if qerr != nil {
			return fmt.Errorf("query raw batch at offset %d: %w", offset, qerr)
		}
		if len(rows) == 0 {
			break
		}
		totalRaw += int64(len(rows))
		for _, r := range rows {
			pkt, perr := rawRowToPacket(r)
			if perr != nil {
				onResult(rawDecodeResult{RawID: r.ID, Src: r.Src, Dst: r.Dst, Payload: r.Payload, Err: perr})
				continue
			}
			ev, derr := dispatcher.DecodeV2(ctx, pkt)
			if derr != nil {
				onResult(rawDecodeResult{RawID: r.ID, Src: r.Src, Dst: r.Dst, Payload: r.Payload, Err: derr})
				continue
			}
			onResult(rawDecodeResult{RawID: r.ID, Src: r.Src, Dst: r.Dst, Payload: r.Payload, Events: ev})
		}
		offset += len(rows)
		if len(rows) < batchLimit {
			break
		}
	}
	return nil
}

// DecodeRawPackets 用指定插件对离线会话的 raw_packets 批量解码，
// 结果写入该 session 的 events 表与 state_changes 投影表。
//
// 约束：
//   - 仅允许解码已停止的 session（不在 tasks map 中的）。
//   - 解码在 pipeline 进程内执行（RegistryServer 在此进程）。
//   - 结果写回原 session 的 capture.sqlite（EventWriter 语义）。
func (s *pipelineService) DecodeRawPackets(ctx context.Context, req capturecontrol.DecodeRawPacketsRequest) (capturecontrol.DecodeRawPacketsResult, error) {
	if req.SessionID == "" {
		return capturecontrol.DecodeRawPacketsResult{}, fmt.Errorf("session_id is required")
	}
	if req.Plugin == "" {
		return capturecontrol.DecodeRawPacketsResult{}, fmt.Errorf("plugin is required")
	}

	logger := s.logger.With("session_id", req.SessionID, "plugin", req.Plugin, "op", "decode_raw")

	// 1. 拒绝解码正在运行的 session（避免与 captureTask 的写操作冲突）
	if _, ok := s.getTask(req.SessionID); ok {
		return capturecontrol.DecodeRawPacketsResult{}, fmt.Errorf("session %s is still running, stop it before decoding", req.SessionID)
	}

	// 2. 获取 db_path
	meta, err := s.controlStore.GetSession(ctx, req.SessionID)
	if err != nil {
		return capturecontrol.DecodeRawPacketsResult{}, fmt.Errorf("get session: %w", err)
	}
	if meta == nil || meta.DBPath == "" {
		return capturecontrol.DecodeRawPacketsResult{}, fmt.Errorf("session %s not found or has no db_path", req.SessionID)
	}
	dbPath := meta.DBPath

	// 3. 检查插件可用：优先按名精确路由（FindByName），退化按协议 hint（Find）。
	client, schemaReg, ok := s.registry.FindByNameFor(auth.OwnerFrom(ctx), req.Plugin)
	if !ok {
		client, schemaReg, ok = s.registry.FindFor(auth.OwnerFrom(ctx), req.Plugin)
	}
	if !ok {
		return capturecontrol.DecodeRawPacketsResult{}, fmt.Errorf("plugin %s not found or not a decoder", req.Plugin)
	}

	// 4. 打开 SQLiteStore
	st, err := store.NewSQLiteStore(dbPath, schemaReg)
	if err != nil {
		return capturecontrol.DecodeRawPacketsResult{}, fmt.Errorf("open sqlite: %w", err)
	}
	defer st.Close()

	// 5. 清空旧解码结果（可选）
	if req.ClearExisting {
		if err := st.ClearDecodedData(ctx); err != nil {
			return capturecontrol.DecodeRawPacketsResult{}, fmt.Errorf("clear decoded data: %w", err)
		}
		logger.Info("cleared existing decoded data and state_changes")
	}

	// 6. 创建 dispatcher（使用真实 sessionID，并携带服务端端口提示以辅助方向推断）
	dispatcher, err := decode.NewDispatcher(client, req.SessionID, logger, schemaReg, decode.WithServerPort(meta.Port))
	if err != nil {
		return capturecontrol.DecodeRawPacketsResult{}, fmt.Errorf("new dispatcher: %w", err)
	}
	defer dispatcher.Close()

	// 7. 分批读取 raw packets 并解码，结果写回 events 表与 state_changes 投影表。
	var totalRaw, decoded, decodeErrors int64
	var pending []*event.Event
	var enrichedSCs []store.EnrichedStateChange
	var sinceFlush int
	baseline := state.NewBaselineManager(nil)
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		if err := st.AppendEvents(ctx, pending); err != nil {
			return fmt.Errorf("append events: %w", err)
		}
		if len(enrichedSCs) > 0 {
			if err := st.WriteEnrichedStateChanges(ctx, req.SessionID, enrichedSCs); err != nil {
				logger.Warn("write enriched state changes", "error", err)
			}
			enrichedSCs = enrichedSCs[:0]
		}
		pending = nil
		sinceFlush = 0
		return nil
	}

	start := time.Now()
	loopErr := forEachRawDecoded(ctx, st, dispatcher, decodeRawOptions{
		Protocol: req.Protocol,
		Src:      req.Src,
		Dst:      req.Dst,
		Limit:    req.Limit,
	}, func(r rawDecodeResult) {
		totalRaw++
		if r.Err != nil {
			decodeErrors++
			logger.Debug("decode failed", "id", r.RawID, "src", r.Src, "dst", r.Dst, "error", r.Err)
			return
		}
		if len(r.Events) == 0 {
			return
		}
		for _, ev := range r.Events {
			if ev == nil {
				continue
			}
			pending = append(pending, ev)
			scChanges, err := baseline.Apply(ev, req.SessionID)
			if err != nil {
				logger.Warn("state projection", "event_id", ev.Identity.ID, "error", err)
				continue
			}
			enrichedSCs = append(enrichedSCs, scChanges...)
		}
		decoded += int64(len(r.Events))
		sinceFlush++
		if sinceFlush >= rawBatchSize {
			if ferr := flush(); ferr != nil {
				logger.Warn("flush events", "error", ferr)
			}
		}
	})
	if loopErr != nil {
		_ = flush()
		return capturecontrol.DecodeRawPacketsResult{TotalRaw: totalRaw, Decoded: decoded, DecodeErrors: decodeErrors}, loopErr
	}
	if err := flush(); err != nil {
		return capturecontrol.DecodeRawPacketsResult{TotalRaw: totalRaw, Decoded: decoded, DecodeErrors: decodeErrors}, err
	}

	logger.Info("decode_raw completed",
		"total_raw", totalRaw, "decoded", decoded, "decode_errors", decodeErrors,
		"duration_sec", time.Since(start).Seconds())
	return capturecontrol.DecodeRawPacketsResult{
		TotalRaw:     totalRaw,
		Decoded:      decoded,
		DecodeErrors: decodeErrors,
	}, nil
}

// TestPlugin 用指定插件对离线会话的 raw_packets 解码并采样返回，用于验证插件解码质量。
// 原始包字节仅进程内使用，绝不回传前端；结果不落库（隔离测试，不污染会话真实解码数据）。
func (s *pipelineService) TestPlugin(ctx context.Context, req capturecontrol.TestPluginRequest) (capturecontrol.TestPluginResult, error) {
	if req.SessionID == "" {
		return capturecontrol.TestPluginResult{}, fmt.Errorf("session_id is required")
	}
	if req.Plugin == "" {
		return capturecontrol.TestPluginResult{}, fmt.Errorf("plugin is required")
	}
	sampleLimit := req.SampleLimit
	if sampleLimit <= 0 {
		sampleLimit = testPluginDefaultSample
	}

	logger := s.logger.With("session_id", req.SessionID, "plugin", req.Plugin, "op", "test_plugin")

	// 允许对运行中（running）会话做只读测试：test_plugin 只 SELECT raw_packets 并用独立 dispatcher 采样，
	// 不回写会话库；会话库已开启 WAL，读不阻塞 captureTask 的写、写也不阻塞读，故不存在写冲突。
	// 若会话此刻正在被停止/重建，GetSession/open 会自然报错，由上层提示。
	running := false
	if _, ok := s.getTask(req.SessionID); ok {
		running = true
	}
	logger = logger.With("running", running)

	// 获取 db_path
	meta, err := s.controlStore.GetSession(ctx, req.SessionID)
	if err != nil {
		return capturecontrol.TestPluginResult{}, fmt.Errorf("get session: %w", err)
	}
	if meta == nil || meta.DBPath == "" {
		return capturecontrol.TestPluginResult{}, fmt.Errorf("session %s not found or has no db_path", req.SessionID)
	}
	dbPath := meta.DBPath

	// 检查插件可用：优先按名精确路由（FindByName），退化按协议 hint（Find）。
	client, schemaReg, ok := s.registry.FindByNameFor(auth.OwnerFrom(ctx), req.Plugin)
	if !ok {
		client, schemaReg, ok = s.registry.FindFor(auth.OwnerFrom(ctx), req.Plugin)
	}
	if !ok {
		return capturecontrol.TestPluginResult{}, fmt.Errorf("plugin %s not found or not a decoder", req.Plugin)
	}

	// 打开 SQLiteStore（只读：与运行中 writer 并发安全，且不回写会话库）
	st, err := store.NewSQLiteStoreReadOnly(dbPath, schemaReg)
	if err != nil {
		return capturecontrol.TestPluginResult{}, fmt.Errorf("open sqlite: %w", err)
	}
	defer st.Close()

	// 创建 dispatcher（不写库，仅解码采样）
	dispatcher, err := decode.NewDispatcher(client, req.SessionID, logger, schemaReg, decode.WithServerPort(meta.Port))
	if err != nil {
		return capturecontrol.TestPluginResult{}, fmt.Errorf("new dispatcher: %w", err)
	}
	defer dispatcher.Close()

	res := capturecontrol.TestPluginResult{TypeHistogram: map[string]int64{}}
	var totalRaw, decoded, decodeErrors int64
	start := time.Now()
	loopErr := forEachRawDecoded(ctx, st, dispatcher, decodeRawOptions{
		Protocol: req.Protocol,
		Src:      req.Src,
		Dst:      req.Dst,
		Limit:    req.Limit,
	}, func(r rawDecodeResult) {
		totalRaw++
		if r.Err != nil {
			decodeErrors++
			if len(res.ErrorSamples) < int(sampleLimit) {
				res.ErrorSamples = append(res.ErrorSamples, capturecontrol.TestErrorLite{
					RawPacketID: r.RawID,
					Src:         r.Src,
					Dst:         r.Dst,
					Error:       r.Err.Error(),
				})
			}
			return
		}
		for _, ev := range r.Events {
			decoded++
			res.TypeHistogram[string(ev.Identity.Type)]++
			if len(res.SampleEvents) < int(sampleLimit) {
				res.SampleEvents = append(res.SampleEvents, eventToLite(ev))
			}
		}
	})
	res.TotalRaw, res.Decoded, res.DecodeErrors = totalRaw, decoded, decodeErrors
	if loopErr != nil {
		return res, loopErr
	}

	logger.Info("test_plugin completed",
		"total_raw", totalRaw, "decoded", decoded, "decode_errors", decodeErrors,
		"sample_events", len(res.SampleEvents), "duration_sec", time.Since(start).Seconds())
	return res, nil
}

// eventToLite 把解码事件拍平为 TestEventLite（data.* 转为 JSON 预览，截断防爆炸）。
func eventToLite(ev *event.Event) capturecontrol.TestEventLite {
	data := ev.Payload.Value.ToAny()
	js, _ := json.Marshal(data)
	if len(js) > testPluginMaxDataJSON {
		js = js[:testPluginMaxDataJSON]
	}
	return capturecontrol.TestEventLite{
		ID:            string(ev.Identity.ID),
		TimestampUnix: ev.Identity.Timestamp.Unix(),
		Type:          string(ev.Identity.Type),
		SchemaID:      ev.Payload.SchemaID,
		DataJSON:      string(js),
	}
}

// rawRowToPacket 把 RawPacketRow 转换为 event.Packet 供 Dispatcher.Decode 使用。
// Src/Dst 在 raw_packets 表中以 "ip:port" 字符串存储，需解析回 netip.AddrPort。
func rawRowToPacket(r store.RawPacketRow) (event.Packet, error) {
	src, err := netip.ParseAddrPort(r.Src)
	if err != nil {
		return event.Packet{}, fmt.Errorf("parse src %q: %w", r.Src, err)
	}
	dst, err := netip.ParseAddrPort(r.Dst)
	if err != nil {
		return event.Packet{}, fmt.Errorf("parse dst %q: %w", r.Dst, err)
	}
	protocol := r.Protocol
	if protocol == "" {
		protocol = "tcp"
	}
	return event.Packet{
		ID:        r.ID,
		Timestamp: r.Timestamp,
		Raw:       r.Payload,
		LinkType:  event.LinkType(r.LinkType),
		Src:       src,
		Dst:       dst,
		Protocol:  protocol,
		Metadata:  map[string]any{},
	}, nil
}
