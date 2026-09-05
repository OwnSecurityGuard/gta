package main

// control_client.go 是探针侧的远端控制通道（AgentControl.Connect）：
// outbound 双向流，指令下行 / 心跳与结果上行。探针掉线按指数退避重连；
// 重连后服务端按 desired-state 重新对齐，无需本地补偿。

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"gametrace/pkg/capture/agent/proto"
	"gametrace/pkg/version"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// ControlAgent 管理 AgentControl 连接：注册、双向流、心跳、指令执行。
type ControlAgent struct {
	ingestAddr string
	probeID    string
	probeToken string
	runner     *captureRunner
	cfg        *agentConfig
	onCfgSaved func() // 配置落盘后回调（如归档参数变化）

	archive *archiver // P1 归档查询/回放；nil = 未启用

	// lastAssign 记忆最近一次 assign（Retry 指令重放用）。
	lastAssign atomic.Pointer[CaptureParams]
}

func NewControlAgent(ingestAddr, probeID, probeToken string, runner *captureRunner, cfg *agentConfig, archive *archiver) *ControlAgent {
	return &ControlAgent{
		ingestAddr: ingestAddr,
		probeID:    probeID,
		probeToken: probeToken,
		runner:     runner,
		cfg:        cfg,
		archive:    archive,
	}
}

// Run 常驻：断线退避重连。ctx 取消时退出。
func (c *ControlAgent) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := c.connectOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("probe control stream error, reconnecting", "error", err, "backoff", backoff)
		}
		if time.Since(started) > 30*time.Second {
			backoff = time.Second
		} else {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
	}
}

// connectOnce 建立一次控制流：Hello → 心跳循环 + 指令收发，直到断流。
func (c *ControlAgent) connectOnce(ctx context.Context) error {
	conn, err := grpc.NewClient("passthrough:///"+c.ingestAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	streamCtx := metadata.AppendToOutgoingContext(ctx,
		"authorization", "Bearer "+c.probeToken)
	client := proto.NewAgentControlClient(conn)
	stream, err := client.Connect(streamCtx)
	if err != nil {
		return fmt.Errorf("open control stream: %w", err)
	}

	// 首包 Hello（服务端校验 probe_id 与凭证一致）。
	if err := stream.Send(&proto.ControlEvent{Payload: &proto.ControlEvent_Hello{
		Hello: &proto.ProbeHello{
			ProbeId:  c.probeID,
			Version:  version.String(),
			Capabilities: []string{"pcap", "plugin_host"},
		},
	}}); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	slog.Info("probe control stream connected", "ingest", c.ingestAddr, "probe_id", c.probeID)

	// 心跳：10s 周期上报三维度快照。
	hbStop := make(chan struct{})
	go func() {
		defer close(hbStop)
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				hb := c.heartbeat()
				if err := stream.Send(&proto.ControlEvent{Payload: &proto.ControlEvent_Heartbeat{Heartbeat: hb}}); err != nil {
					slog.Debug("heartbeat send failed", "error", err)
					return
				}
			}
		}
	}()

	// 指令接收与执行；结果回传。
	for {
		cmd, err := stream.Recv()
		if err != nil {
			<-hbStop
			return err
		}
		res := c.execute(ctx, cmd, func(ev *proto.ControlEvent) error {
			return stream.Send(ev)
		})
		if err := stream.Send(&proto.ControlEvent{Payload: &proto.ControlEvent_Result{Result: res}}); err != nil {
			<-hbStop
			return err
		}
	}
}

// heartbeat 组装三维度快照。connection 维度由服务端根据流活性判定，不上报。
func (c *ControlAgent) heartbeat() *proto.ProbeHeartbeat {
	state, sessionID, iface, portsCSV, lastErr, _ := c.runner.State()
	var ports []int32
	for _, s := range splitCSVInts(portsCSV) {
		ports = append(ports, int32(s))
	}
	d := c.runner.Data()
	hb := &proto.ProbeHeartbeat{
		Capture: &proto.ProbeCaptureStatus{
			State: state, SessionId: sessionID, Iface: iface,
			Ports: ports, Error: lastErr,
		},
		Data: &proto.ProbeDataStatus{
			LastPacketUnixMs: d.LastPacketMs,
			LastUploadUnixMs: d.LastUploadMs,
			PacketsCaptured:  d.PacketsCaptured,
			PacketsAcked:     d.PacketsAcked,
			SpoolDepth:       d.SpoolDepth,
			Dropped:          d.Dropped,
		},
	}
	if c.archive != nil {
		a := c.archive.Status()
		hb.Archive = &proto.ProbeArchiveStatus{
			Bytes: uint64(a.Bytes), Segments: uint32(a.Segments),
			OldestUnix: a.OldestMs / 1000, NewestUnix: a.NewestMs / 1000,
		}
	}
	return hb
}

// heartbeatInterval 是探针心跳周期（与服务端预期一致：10s）。
const heartbeatInterval = 10 * time.Second

// execute 执行一条指令并返回结果（永远返回 result，不返回 error——错误在 result 里）。
// sendEvent 允许指令在结果之外回传额外事件（如归档查询应答）。
func (c *ControlAgent) execute(ctx context.Context, cmd *proto.Command, sendEvent func(*proto.ControlEvent) error) *proto.CommandResult {
	ok := true
	errStr := ""
	if cmd == nil {
		return &proto.CommandResult{}
	}
	// assign 幂等记忆（Retry 用）：仅记录 Assign 参数。
	if a := cmd.GetAssign(); a != nil {
		p := &CaptureParams{
			SessionID: a.GetSessionId(), Iface: a.GetIface(),
			Ports: a.GetPorts(), Hosts: a.GetHosts(), BPF: a.GetBpf(),
			SnapLen: a.GetSnaplen(), Promisc: a.GetPromisc(),
		}
		c.lastAssign.Store(p)
	}
	switch p := cmd.GetPayload().(type) {
	case *proto.Command_Assign:
		a := p.Assign
		err := c.runner.Start(CaptureParams{
			SessionID: a.GetSessionId(), Iface: a.GetIface(),
			Ports: a.GetPorts(), Hosts: a.GetHosts(), BPF: a.GetBpf(),
			SnapLen: a.GetSnaplen(), Promisc: a.GetPromisc(),
		}, c.ingestAddr, c.probeToken)
		if err != nil {
			ok, errStr = false, err.Error()
		}
	case *proto.Command_Stop:
		if err := c.runner.Stop(); err != nil {
			ok, errStr = false, err.Error()
		}
	case *proto.Command_Filter:
		f := p.Filter
		err := c.runner.UpdateFilter(f.GetPorts(), f.GetHosts(), f.GetBpf())
		if err != nil {
			ok, errStr = false, err.Error()
		}
	case *proto.Command_Config:
		errStr = c.applyConfig(p.Config.GetKvs())
		ok = errStr == ""
	case *proto.Command_Retry:
		if last := c.lastAssign.Load(); last != nil {
			err := c.runner.Start(*last, c.ingestAddr, c.probeToken)
			if err != nil {
				ok, errStr = false, err.Error()
			}
		} else {
			ok, errStr = false, "no previous assign to retry"
		}
	case *proto.Command_ArchiveQuery:
		if c.archive == nil {
			ok, errStr = false, "archive disabled on this probe"
		} else {
			segs, err := c.archive.Segments(ctx, p.ArchiveQuery.GetFromUnix()*1000, p.ArchiveQuery.GetToUnix()*1000)
			if err != nil {
				ok, errStr = false, err.Error()
			} else {
				reply := &proto.ArchiveSegmentsReply{}
				for _, s := range segs {
					reply.Segments = append(reply.Segments, &proto.ArchiveSegmentInfo{
						SegId: s.SegID, FirstUnix: s.FirstMs / 1000, LastUnix: s.LastMs / 1000,
						Packets: s.Packets, Bytes: s.Bytes, LinkType: s.LinkType,
					})
				}
				// 归档查询应答经控制流回传（包头小，不挤占数据面）。
				if err := sendEvent(&proto.ControlEvent{Payload: &proto.ControlEvent_ArchiveSegments{ArchiveSegments: reply}}); err != nil {
					ok, errStr = false, "send archive segments: "+err.Error()
				}
			}
		}
	case *proto.Command_ArchiveUpload:
		if c.archive == nil {
			ok, errStr = false, "archive disabled on this probe"
		} else {
			go c.archive.Upload(ctx, p.ArchiveUpload.GetTargetSessionId(),
				p.ArchiveUpload.GetFromUnix()*1000, p.ArchiveUpload.GetToUnix()*1000,
				c.ingestAddr, c.probeToken)
		}
	default:
		ok, errStr = false, "unsupported command"
	}
	return &proto.CommandResult{Id: cmd.GetId(), Ok: ok, Error: errStr}
}

// applyConfig 应用白名单内的配置键并落盘 probe.json。
// 白名单：name / archive_enabled / archive_max_age_hours / archive_max_bytes。
// 返回空串表示成功，否则为错误描述。
func (c *ControlAgent) applyConfig(kvs map[string]string) string {
	if len(kvs) == 0 {
		return ""
	}
	for k, v := range kvs {
		switch k {
		case "name":
			c.cfg.Name = v
		case "archive_enabled":
			switch v {
			case "true", "1", "on":
				c.cfg.Archive.Enabled = true
			case "false", "0", "off":
				c.cfg.Archive.Enabled = false
			default:
				return fmt.Sprintf("archive_enabled: invalid value %q", v)
			}
		case "archive_max_age_hours":
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 0 {
				return fmt.Sprintf("archive_max_age_hours: invalid value %q", v)
			}
			c.cfg.Archive.MaxAgeHrs = n
		case "archive_max_bytes":
			var n int64
			if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 0 {
				return fmt.Sprintf("archive_max_bytes: invalid value %q", v)
			}
			c.cfg.Archive.MaxBytes = n
		default:
			return fmt.Sprintf("config key %q not allowed", k)
		}
	}
	if err := saveAgentConfig(c.cfg); err != nil {
		return fmt.Sprintf("save probe.json: %v", err)
	}
	// 归档参数变更立即生效（切留存策略）；其他键回调也无害。
	if c.onCfgSaved != nil {
		c.onCfgSaved()
	}
	return ""
}

func splitCSVInts(s string) []int {
	var out []int
	for _, part := range splitComma(s) {
		var n int
		if _, err := fmt.Sscanf(part, "%d", &n); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
