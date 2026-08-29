package main

import (
	"context"
	"log/slog"
	"time"

	"gta/pkg/capture/agent/proto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// ackTimeout 是流结束时等服务端 PushAck 汇总的兜底时长。
const ackTimeout = 5 * time.Second

// ingestClient 把抓包 channel 里的包按批推送到 pipeline 的 AgentIngest server。
// 断线按指数退避重连（1s→30s，与 SDK RunRegisterLoop 同策略）；
// 重连期间积压的半批随流丢失（服务端语义允许：不缓存补发）。
// 流结束（ctx 取消）时 Flush 剩余半批、CloseSend 并把 PushAck 的
// Delivered/Dropped/Rejected 汇总打日志，让用户看到归属拒绝等计数。
type ingestClient struct {
	addr          string
	token         string // 为空表示匿名
	sessionID     string
	iface         string
	batchSize     int
	batchInterval time.Duration
}

func (c *ingestClient) run(ctx context.Context, packets <-chan *proto.RawPacket) {
	bk := newBackoff()
	for ctx.Err() == nil {
		started := time.Now()
		streamErr := c.pushOnce(ctx, packets)
		if ctx.Err() != nil {
			return
		}
		if streamErr != nil {
			slog.Warn("ingest stream error, reconnecting", "addr", c.addr, "error", streamErr, "backoff", bk.cur)
		}
		// 流存活超过一个退避上限周期视为成功过，退避归位。
		if time.Since(started) > 30*time.Second {
			bk.Reset()
		}
		select {
		case <-time.After(bk.Next()):
		case <-ctx.Done():
			return
		}
	}
}

// pushOnce 建立一次连接并持续推流；返回导致流结束的错误（ctx 取消返回 nil）。
func (c *ingestClient) pushOnce(ctx context.Context, packets <-chan *proto.RawPacket) error {
	conn, err := grpc.NewClient(
		"passthrough:///"+c.addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return err
	}
	defer conn.Close()

	// 流上下文不能直接挂在 ctx 下：ctx 取消后仍需在收尾窗口内发送尾批并
	// 接收 PushAck（否则半批必然丢失、汇总不可达），因此用 WithoutCancel
	// 解绑，由本函数显式 cancel 控制生命周期。
	streamBase := context.WithoutCancel(ctx)
	if c.token != "" {
		streamBase = metadata.AppendToOutgoingContext(streamBase, "authorization", "Bearer "+c.token)
	}
	streamCtx, cancel := context.WithCancel(streamBase)
	defer cancel()

	client := proto.NewAgentIngestClient(conn)
	stream, err := client.Push(streamCtx)
	if err != nil {
		return err
	}

	batcher := newBatchAccumulator(c.batchSize)
	timer := time.NewTimer(c.batchInterval)
	defer timer.Stop()

	// stopTimer 停掉时间阈值计时器并清空可能已触发的信号。
	stopTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			// 优雅停机：刷出剩余半批并限时收服务端汇总
			//（流上下文已与 ctx 解绑，此窗口内仍可收发）。
			c.finishStream(stream, batcher, cancel)
			return nil
		case p := <-packets:
			if batch := batcher.Push(p); batch != nil {
				// 满批立即发出，时间阈值随之作废，等下一批首包重启计时。
				if err := c.sendBatch(stream, batch); err != nil {
					return err
				}
				stopTimer()
			} else if batcher.Len() == 1 {
				// 半批的首包：从批起点启动时间阈值计时。
				// 注意不能每个包都重置计时器——持续流量下时间阈值将永不触发
				//（如 2 pkt/s + 128 批大小意味着约 64s 的编码延迟）。
				stopTimer()
				timer.Reset(c.batchInterval)
			}
		case <-timer.C:
			if err := c.sendBatch(stream, batcher.Flush()); err != nil {
				return err
			}
			timer.Reset(c.batchInterval)
		}
	}
}

// sendBatch 发送一批；空批为 no-op。发送失败时记录被丢弃的包数。
func (c *ingestClient) sendBatch(stream proto.AgentIngest_PushClient, batch []*proto.RawPacket) error {
	if len(batch) == 0 {
		return nil
	}
	err := stream.Send(&proto.PacketBatch{SessionId: c.sessionID, Iface: c.iface, Packets: batch})
	if err != nil {
		slog.Warn("ingest: batch dropped on send failure", "packets", len(batch), "error", err)
	}
	return err
}

// finishStream 停机收尾：刷出剩余半批、CloseSend 后限时等待 PushAck，
// 把服务端汇总打日志（归属拒绝的 Rejected 计数在此暴露给用户）。
// 整个收尾限时 ackTimeout，超时 cancel 强制结束流，避免停机卡死。
func (c *ingestClient) finishStream(stream proto.AgentIngest_PushClient, batcher *batchAccumulator, cancel context.CancelFunc) {
	defer cancel()
	if err := c.sendBatch(stream, batcher.Flush()); err != nil {
		slog.Debug("ingest: final flush failed", "session", c.sessionID, "error", err)
	}
	ackCh := make(chan *proto.PushAck, 1)
	go func() {
		ack, err := stream.CloseAndRecv()
		if err != nil {
			ackCh <- nil
			return
		}
		ackCh <- ack
	}()
	select {
	case ack := <-ackCh:
		if ack == nil {
			slog.Info("ingest stream closed (no ack)", "session", c.sessionID)
			return
		}
		slog.Info("ingest session summary",
			"session", c.sessionID,
			"batches", ack.GetBatches(),
			"packets", ack.GetPackets(),
			"delivered", ack.GetDelivered(),
			"dropped", ack.GetDropped(),
			"rejected", ack.GetRejected(),
		)
		if ack.GetRejected() > 0 {
			slog.Warn("some batches were rejected by owner check: the agent token does not own this capture session", "session", c.sessionID)
		}
	case <-time.After(ackTimeout):
		slog.Warn("ingest: timed out waiting for PushAck", "session", c.sessionID)
	}
}
