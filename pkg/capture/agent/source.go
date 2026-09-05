package agent

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"time"

	"gta/pkg/capture"
	"gta/pkg/capture/agent/proto"
	"gta/pkg/capture/internal/base"
	"gta/pkg/event"
)

// SourceName 是本 Source 在 capture registry 注册的名字。
const SourceName = "agent"

func init() {
	capture.Register(SourceName, capture.FactoryFunc{
		ValidateFunc: validateConfig,
		NewFunc:      newSource,
	})
}

// Config 是 agent capture source 的配置。
// Hub 由宿主（gta-pipeline main）创建并与 AgentIngest server 共享；
// SessionID 决定本 Source 只消费哪个会话的推送。
type Config struct {
	// Hub 是包路由中枢，必填。
	Hub *Hub

	// SessionID 是本 Source 消费的抓包会话 id，必填。
	SessionID string

	// ChannelSize 是订阅缓冲容量；<=0 用 DefaultChannelSize。
	ChannelSize int

	// DeliverTimeout 是背压窗口：本 Source 的缓冲满时，Hub 最多等待这么久才丢包。
	// 压力在此期间沿 gRPC 流传回 agent（由 agent 侧磁盘 spool 吸收）。
	// <=0 用 DefaultDeliverTimeout。
	DeliverTimeout time.Duration
}

func validateConfig(cfg any) error {
	c, ok := cfg.(Config)
	if !ok {
		return errors.New("agent config type mismatch")
	}
	if c.Hub == nil {
		return errors.New("agent config hub is required")
	}
	if strings.TrimSpace(c.SessionID) == "" {
		return errors.New("agent config session_id is required")
	}
	return nil
}

func newSource(cfg any) (capture.Source, error) {
	c, ok := cfg.(Config)
	if !ok {
		return nil, errors.New("agent config type mismatch")
	}
	if err := validateConfig(c); err != nil {
		return nil, err
	}
	return newSourceFromConfig(c), nil
}

func newSourceFromConfig(c Config) *Source {
	size := c.ChannelSize
	if size <= 0 {
		size = DefaultChannelSize
	}
	s := &Source{cfg: c, ch: make(chan event.Packet, size)}
	s.StatTracker.Init()
	return s
}

// Source 是 agent 推送抓包源：消费 Hub 中属于本会话的原始完整帧。
//
// 与 mobile source 的关键区别：包带完整链路层帧（Raw）与真实 LinkType，
// 不是 LinkTypeProxyPayload 退化帧，解码路径与 pcap-live 完全一致。
//
// 生命周期：capture_task 通过 capture.Open("agent", Config{...}) 打开，
// 会话结束时 Close 取消订阅；AgentIngest server 与 Hub 的生命周期由宿主管理，
// 不随单个会话启停。
type Source struct {
	cfg Config

	// ch 是 Hub 订阅 channel，直接作为 Packets() 的输出 channel：
	// Hub 在锁外带超时投递（满则背压等待，超时才丢），Close 时先取消订阅
	//（cancel 会等在途投递结束，保证无并发写）再 close。
	ch chan event.Packet

	subMu sync.Mutex
	sub   *Subscription // Subscribe 返回的订阅句柄；Start 后非 nil（统计用）

	unsubMu sync.Mutex
	unsub   func() // Subscribe 返回的取消订阅函数；Start 前为 nil

	base.Lifecycle
	base.StatTracker
}

// NewSource 直接构造 Source（测试与宿主直连用；registry 路径走 capture.Open）。
func NewSource(cfg Config) (*Source, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	return newSourceFromConfig(cfg), nil
}

// Start 订阅 Hub 并开始向 Packets() 通道投递。
func (s *Source) Start(ctx context.Context) error {
	return s.Lifecycle.Start(ctx, s.setup, s.run)
}

func (s *Source) setup() error {
	sub, unsub := s.cfg.Hub.Subscribe(s.cfg.SessionID, s.ch, WithDeliverTimeout(s.cfg.DeliverTimeout))
	s.unsubMu.Lock()
	s.unsub = unsub
	s.sub = sub
	s.unsubMu.Unlock()
	return nil
}

func (s *Source) run(ctx context.Context) {
	<-ctx.Done()
	// 先取消订阅：Hub.Subscribe 的约定保证 cancel 返回后不再有并发写，
	// 随后 close 才安全。
	s.unsubMu.Lock()
	unsub := s.unsub
	s.unsub = nil
	s.unsubMu.Unlock()
	if unsub != nil {
		unsub()
	}
	close(s.ch)
}

func (s *Source) Packets() <-chan event.Packet { return s.ch }
func (s *Source) Err() error                   { return s.Lifecycle.Err() }
func (s *Source) Close() error                 { return s.Lifecycle.Close() }
func (s *Source) Stats() capture.Stats {
	stats := s.StatTracker.Stats()
	// 把 Hub 订阅级计数透出：PacketsIn/BytesIn = 成功投递给本 Source 的包/字节，
	// Drops = 本 Source channel 满且背压超时被丢弃的包数（慢消费者丢包，明确记录）。
	s.subMu.Lock()
	sub := s.sub
	s.subMu.Unlock()
	if sub != nil {
		delivered, dropped, bytesIn := sub.Stats()
		if stats.PacketsIn == 0 {
			stats.PacketsIn = delivered
		}
		if stats.BytesIn == 0 {
			stats.BytesIn = bytesIn
		}
		if stats.Drops == 0 {
			stats.Drops = dropped
		}
		if stats.Extra != nil {
			stats.Extra["agent_delivered"] = delivered
			stats.Extra["agent_dropped"] = dropped
			stats.Extra["agent_bytes_in"] = bytesIn
		}
	}
	return stats
}

// SessionID 返回本 Source 消费的会话 id。
func (s *Source) SessionID() string { return s.cfg.SessionID }

// PacketFromProto 把 proto.RawPacket 转成 event.Packet（包外复用：probe 回放导入路径）。
func PacketFromProto(rp *proto.RawPacket, iface string) event.Packet {
	return packetFromProto(rp, iface)
}

// packetsFromBatch 把一批 proto 包转成 event.Packet，并统计收包字节。
// 保留完整帧与 link_type（与 pcap-live 一致），不退化成 LinkTypeProxyPayload。
func packetsFromBatch(batch *proto.PacketBatch) []event.Packet {
	rps := batch.GetPackets()
	pkts := make([]event.Packet, 0, len(rps))
	for _, rp := range rps {
		pkts = append(pkts, packetFromProto(rp, batch.GetIface()))
	}
	return pkts
}

// packetFromProto 把 proto.RawPacket 转成 event.Packet。
func packetFromProto(rp *proto.RawPacket, iface string) event.Packet {
	pkt := event.Packet{
		ID:       rp.GetId(),
		Raw:      rp.GetRaw(),
		LinkType: event.LinkType(rp.GetLinkType()),
		Protocol: rp.GetProtocol(),
	}
	if ns := rp.GetTimestampNs(); ns > 0 {
		pkt.Timestamp = time.Unix(0, ns)
	}
	if ap, err := netip.ParseAddrPort(rp.GetSrc()); err == nil {
		pkt.Src = ap
	}
	if ap, err := netip.ParseAddrPort(rp.GetDst()); err == nil {
		pkt.Dst = ap
	}
	md := rp.GetMetadata()
	if iface != "" || len(md) > 0 {
		m := make(map[string]any, len(md)+1)
		for k, v := range md {
			m[k] = v
		}
		if iface != "" {
			m["interface"] = iface
		}
		pkt.Metadata = m
	}
	return pkt
}
