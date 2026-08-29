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

	callCtx := ctx
	if c.token != "" {
		callCtx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+c.token)
	}
	// streamCtx 独立可取消：流结束后要短暂存活以收 PushAck。
	streamCtx, cancel := context.WithCancel(callCtx)
	defer cancel()

	client := proto.NewAgentIngestClient(conn)
	stream, err := client.Push(streamCtx)
	if err != nil {
		return err
	}

	batcher := newBatchAccumulator(c.batchSize)
	timer := time.NewTimer(c.batchInterval)
	defer timer.Stop()

	flush := func() error {
		batch := batcher.Flush()
		if batch == nil {
			return nil
		}
		err := stream.Send(&proto.PacketBatch{SessionId: c.sessionID, Iface: c.iface, Packets: batch})
		if err != nil {
			slog.Warn("ingest: batch dropped on send failure", "packets", len(batch), "error", err)
		}
		return err
	}

	for {
		select {
		case <-ctx.Done():
			// 优雅停机：刷出剩余半批并收服务端汇总。
			_ = flush()
			c.finishStream(stream, cancel)
			return nil
		case p := <-packets:
			if batch := batcher.Push(p); batch != nil {
				if err := stream.Send(&proto.PacketBatch{SessionId: c.sessionID, Iface: c.iface, Packets: batch}); err != nil {
					return err
				}
			}
			// 低流量兜底：重置时间阈值，半批在 batchInterval 内必然刷出。
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(c.batchInterval)
		case <-timer.C:
			if err := flush(); err != nil {
				return err
			}
			timer.Reset(c.batchInterval)
		}
	}
}

// finishStream 刷尾批、CloseSend 后等 PushAck，把服务端汇总打日志。
func (c *ingestClient) finishStream(stream proto.AgentIngest_PushClient, cancel context.CancelFunc) {
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
	cancel()
}
