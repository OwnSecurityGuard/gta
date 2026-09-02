package main

import (
	"context"
	"log/slog"
	"time"

	"gta/pkg/capture/agent"
	"gta/pkg/capture/agent/proto"
	"gta/pkg/spool"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// ackTimeout 是流结束时等服务端 PushAck 汇总的兜底时长。
const ackTimeout = 5 * time.Second

// ingestClient 把抓包 channel 里的包按批推送到 pipeline 的 AgentIngest server。
// 断线按指数退避重连（1s→30s，与 SDK RunRegisterLoop 同策略）。
//
// 上行链路不丢包的关键：所有待发送的批次都先落盘（spool），发送成功后才确认。
// 因此三种故障都不会丢数据：
//   - 断线：未确认的批次留在磁盘，重连后从断点重发（Requeue）；
//   - 重连期间的积压：继续落盘，由磁盘吸收，不再受内存 channel 容量限制；
//   - 进程重启 / 断电：重新打开同一 spool 目录即可续传。
//
// 语义是 at-least-once（失败重发，可能重复）。重复是幂等的：RawPacket.Id 由
// agent 侧生成（UUIDv7），服务端落库用 INSERT OR REPLACE。
// 唯一的丢弃点是 spool 触顶（磁盘配额用尽），此时丢新包并计数告警。
type ingestClient struct {
	addr          string
	token         string // 为空表示匿名
	sessionID     string
	iface         string
	batchSize     int
	batchInterval time.Duration

	// spool 是上行链路的磁盘缓冲队列（断电续传）。非 nil。
	spool *spool.Queue
	// dropped 是 spool 拒绝接收（配额用尽 / 写盘失败）而被丢弃的包数。
	dropped uint64
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
			slog.Warn("ingest stream error, reconnecting",
				"addr", c.addr, "error", streamErr, "backoff", bk.cur,
				"spool_depth", c.depth())
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
//
// 发送源统一是 spool：抓到的包先落盘，再从 spool 取出按批发送，成功才确认。
// 半批不会因流中断而丢失——发送失败时不做确认，重连后 Requeue 重发。
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
	// 开流即声明目标会话，让服务端在「连上但零流量」时也知道本 agent 服务于
	// 哪个 session（否则服务端要等第一个 batch——抓不到包时它永远不会来）。
	streamBase = metadata.AppendToOutgoingContext(
		streamBase, agent.StreamSessionMetadataKey, c.sessionID,
	)
	streamCtx, cancel := context.WithCancel(streamBase)
	defer cancel()

	client := proto.NewAgentIngestClient(conn)
	stream, err := client.Push(streamCtx)
	if err != nil {
		return err
	}

	// 上一条流可能是在发送失败后退出的：那些记录从未被确认，
	// 放回队头后本轮会重发（断点续传）。
	c.spool.Requeue()

	timer := time.NewTimer(c.batchInterval)
	defer timer.Stop()
	timerArmed := false
	// stopTimer 停掉时间阈值计时器并清空可能已触发的信号。
	stopTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	armTimer := func() {
		if timerArmed {
			return
		}
		timerArmed = true
		stopTimer()
		timer.Reset(c.batchInterval)
	}

	// pending 是已从 spool 取出、等待发送的批次（在途）。
	// 它始终是队头开始的连续区间，因此 sendAndAck 里 AckN(len(pending)) 与
	// 实际发送的内容严格对应，不会出现「确认了没发出去的包」。
	var pending []*proto.RawPacket

	for {
		if len(pending) == 0 {
			batch, err := c.spool.Next(c.batchSize)
			if err != nil {
				// 读盘出错说明 spool 损坏，无法继续保证不丢：
				// 返回错误让上层重连重试，不要静默跳过（跳过会丢数据）。
				slog.Error("ingest: read spool failed", "error", err)
				return err
			}
			pending = batch
			switch {
			case len(pending) >= c.batchSize:
				// 满批立即发，时间阈值随之作废。
				if err := c.sendAndAck(stream, pending); err != nil {
					return err
				}
				pending = nil
				stopTimer()
				timerArmed = false
				continue
			case len(pending) > 0:
				// 半批：启动时间阈值，低流量下不至于一直等满才发。
				armTimer()
			default:
				stopTimer()
				timerArmed = false
			}
		}

		select {
		case <-ctx.Done():
			// 优雅停机：尾批尽力发一次；失败就留在 spool 里，下次启动续传
			//（旧实现在这里丢掉半批，且进程退出后无从补救）。
			if err := c.sendAndAck(stream, pending); err != nil {
				slog.Debug("ingest: final flush failed, packets stay in spool for next run",
					"session", c.sessionID, "packets", len(pending), "error", err)
			}
			pending = nil
			c.finishStream(stream, cancel)
			return nil
		case p := <-packets:
			if err := c.spool.Append(p); err != nil {
				// spool 触顶或写盘失败：这个包没有退路了，明确计数并限频告警。
				c.dropped++
				if c.dropped == 1 || c.dropped%1000 == 0 {
					slog.Warn("ingest: packet dropped (spool full or write error)",
						"session", c.sessionID, "dropped", c.dropped, "error", err)
				}
				continue
			}
			// 有在途批次时，把刚落盘的包补进来（顺序由 spool 保证）。
			if len(pending) > 0 && len(pending) < c.batchSize {
				more, err := c.spool.Next(c.batchSize - len(pending))
				if err != nil {
					slog.Error("ingest: read spool failed", "error", err)
					return err
				}
				pending = append(pending, more...)
				if len(pending) >= c.batchSize {
					if err := c.sendAndAck(stream, pending); err != nil {
						return err
					}
					pending = nil
					stopTimer()
					timerArmed = false
				}
			}
		case <-timer.C:
			// 时间阈值到：把攒到的半批发出去（低流量兜底）。
			timerArmed = false
			if err := c.sendAndAck(stream, pending); err != nil {
				return err
			}
			pending = nil
		}
	}
}

// sendAndAck 发送一批并在成功后确认。
// 发送失败时不确认：数据留在 spool 里，重连后重发（这是不丢包的核心）。
func (c *ingestClient) sendAndAck(stream proto.AgentIngest_PushClient, batch []*proto.RawPacket) error {
	if len(batch) == 0 {
		return nil
	}
	if err := c.sendBatch(stream, batch); err != nil {
		return err
	}
	if err := c.spool.AckN(len(batch)); err != nil {
		slog.Error("ingest: ack spool failed (packets may be redelivered)", "error", err)
	}
	return nil
}

// sendBatch 发送一批；空批为 no-op。
func (c *ingestClient) sendBatch(stream proto.AgentIngest_PushClient, batch []*proto.RawPacket) error {
	if len(batch) == 0 {
		return nil
	}
	err := stream.Send(&proto.PacketBatch{SessionId: c.sessionID, Iface: c.iface, Packets: batch})
	if err != nil {
		slog.Warn("ingest: batch send failed, will retry from spool",
			"packets", len(batch), "error", err)
	}
	return err
}

// depth 返回 spool 当前积压（观测/日志用）。
func (c *ingestClient) depth() int {
	if c.spool == nil {
		return 0
	}
	n, _ := c.spool.Depth()
	return n
}

// finishStream 停机收尾：CloseSend 后限时等待 PushAck，
// 把服务端汇总打日志（归属拒绝的 Rejected 计数在此暴露给用户）。
// 整个收尾限时 ackTimeout，超时 cancel 强制结束流，避免停机卡死。
func (c *ingestClient) finishStream(stream proto.AgentIngest_PushClient, cancel context.CancelFunc) {
	defer cancel()
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
			"spool_depth", c.depth(),
		)
		if ack.GetRejected() > 0 {
			slog.Warn("some batches were rejected by owner check: the agent token does not own this capture session", "session", c.sessionID)
		}
	case <-time.After(ackTimeout):
		slog.Warn("ingest: timed out waiting for PushAck", "session", c.sessionID)
	}
}
