// Package capturecontrol 实现 CaptureControl gRPC server。
// 由 gta-pipeline 嵌入使用，处理 start/stop/status/list_interfaces 控制命令。
// 实际抓包逻辑由 gta-pipeline 的 captureEngine 提供，server 仅做 RPC 适配。
package capturecontrol

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc"

	"gta/pkg/auth"
	"gta/pkg/capture/mobile"
	pb "gta/pkg/internalipc/proto"
)

// CaptureEngine 是 gta-pipeline 实现的抓包引擎接口。
// capturecontrol.Server 委托实际操作给此接口，保持 server 与引擎解耦。
type CaptureEngine interface {
	// StartSession 启动抓包会话。返回 capture.sqlite 的 db_path。
	StartSession(ctx context.Context, req StartSessionRequest) (StartSessionResult, error)
	// StopSession 停止抓包会话，返回最终统计。
	StopSession(ctx context.Context, sessionID string) (StopSessionResult, error)
	// GetStatus 查询会话状态。
	GetStatus(ctx context.Context, sessionID string) (StatusResult, error)
	// ListSessions 列出当前活跃的抓包会话。
	ListSessions(ctx context.Context) ([]SessionSummary, error)
	// ListInterfaces 列出可用网卡。
	ListInterfaces(ctx context.Context) ([]string, error)
	// DecodeRawPackets 用指定插件对离线会话的 raw_packets 批量解码，
	// 结果写入该 session 的 events 表。
	DecodeRawPackets(ctx context.Context, req DecodeRawPacketsRequest) (DecodeRawPacketsResult, error)
	// ListPlugins 列出当前已注册的插件摘要。
	ListPlugins(ctx context.Context) ([]PluginSummary, error)
	// GetPluginManifest 获取指定插件的 manifest bytes。
	GetPluginManifest(ctx context.Context, name string) ([]byte, error)
	// DeregisterPlugin 注销指定插件（按 instance_id 或 name）。
	DeregisterPlugin(ctx context.Context, instanceID, name string) (string, error)
	// SetSessionPlugin 运行中热切换某抓包会话的解码插件绑定。
	// 返回切换后的实际插件名。
	SetSessionPlugin(ctx context.Context, sessionID, plugin string) (string, error)
	// SubscribePlugins 订阅插件注册表状态变化事件流（register/deregister/online/offline）。
	// 返回的通道在 ctx 取消时关闭。抓包侧借此即时感知插件上下线，避免轮询。
	SubscribePlugins(ctx context.Context) (<-chan PluginEvent, error)
	// TestPlugin 用指定插件对离线会话的 raw_packets 解码并采样返回，用于验证插件解码质量。
	// 原始包字节仅进程内使用，不回传；结果不落库（隔离测试）。
	TestPlugin(ctx context.Context, req TestPluginRequest) (TestPluginResult, error)
	// Verify 用指定插件对离线会话的 raw_packets 解码并做契约+质量校验，
	// 产出 violations + quality + verdict，并把 validated 证明回写 Developer Plane。
	Verify(ctx context.Context, req VerifyRequest) (VerifyResult, error)
	// SampleBytes 读取会话原始包前若干字节（事实），并在 plugin_debug_access 留审计。
	SampleBytes(ctx context.Context, req SampleBytesRequest) (SampleBytesResult, error)
	// GetRegistryAddr 返回插件应连接的注册中心地址（即 -registry-addr 的值）。
	GetRegistryAddr(ctx context.Context) (string, error)
	// CreateProxyLease 为调用方创建一个常驻代理出口：独立 gta-singbox-agent 进程
	// + 固定的手机 CONNECT 端口。端口在租约生命周期内不变，可反复在其上开停
	// 抓包会话。身份经 withRequestOwner 注入 ctx（同 StartSession）。
	CreateProxyLease(ctx context.Context, req CreateProxyLeaseRequest) (ProxyLease, error)
	// ListProxyLeases 列出调用方可见的租约（admin 全可见）。
	ListProxyLeases(ctx context.Context) ([]ProxyLease, error)
	// GetProxyLease 查询单个租约状态快照（owner 校验，不匹配按不存在处理）。
	GetProxyLease(ctx context.Context, leaseID string) (ProxyLease, error)
	// ReleaseProxyLease 释放租约：停抓包、杀 agent、回收端口（幂等）。
	ReleaseProxyLease(ctx context.Context, leaseID string) (ReleaseProxyLeaseResult, error)
	// StartLeaseCapture 在常驻租约上开一次新的抓包会话（独立 session_id）。
	// 代理出口与手机连接不受影响；租约已在抓包中时返回错误。
	StartLeaseCapture(ctx context.Context, req StartLeaseCaptureRequest) (StartLeaseCaptureResult, error)
	// StopLeaseCapture 停止租约当前抓包并回到 idle（出口保留，agent 停止上报）。
	StopLeaseCapture(ctx context.Context, leaseID string) (StopLeaseCaptureResult, error)
}

// ProxyLease 是一个常驻代理出口的配置 + 运行时状态快照（与 proto ProxyLeaseState 对应）。
// LeaseID 在租约生命周期内恒定，与单次抓包会话的 session_id 不再等同。
type ProxyLease struct {
	LeaseID         string
	Owner           string
	ProjectID       string
	Plugin          string
	IncludeHosts    []string
	IncludePorts    []int
	Device          string
	ListenAddr      string // agent HTTP CONNECT 监听（手机连这里）
	AgentListenPort int
	MobileGRPCPort  int // 当前抓包会话的 mobile source gRPC 端口（0=未抓包）
	AgentRunning    bool
	AgentPID        int32
	SessionRunning  bool // 当前是否有活跃抓包会话
	SessionID       string
	CreatedAt       time.Time
	ActiveConns     int64
	TotalConns      uint64
	LastDataUnix    int64
	TotalBytes      uint64
	// ---- 常驻出口相关 ----
	ControlPort       int    // agent 本地控制接口端口（pipeline → agent）
	CaptureRunning    bool   // agent 是否正在上报
	CaptureCount      int    // 本租约累计开始过多少次抓包
	LastCaptureAtUnix int64  // 最近一次 start/stop capture 时间（unix 秒，0=从未）
	StickyPort        bool   // 端口是否为 (owner,device) 复用端口（二维码长期有效）
}

// CreateProxyLeaseRequest 是创建代理出口的参数（与 proto 对应但不含 protobuf 类型）。
// 不含分帧参数：帧边界判定是协议语义，由解码插件按连接自行处理（见 mobile.MobileConfig）。
type CreateProxyLeaseRequest struct {
	Plugin       string
	IncludeHosts []string
	IncludePorts []int32
	Device       string
	ProjectID    string
	// NoAutoStart 为 true 时只创建出口、不立即开始抓包（等 StartLeaseCapture）。
	NoAutoStart bool
	// NoSticky 为 true 时不复用该 (owner, device) 上次用过的端口。
	NoSticky bool
}

// StartLeaseCaptureRequest 是在租约上开一次抓包的参数。
// Plugin/IncludeHosts/IncludePorts 留空表示沿用租约创建时的配置。
type StartLeaseCaptureRequest struct {
	LeaseID      string
	Plugin       string
	IncludeHosts []string
	IncludePorts []int32
}

// StartLeaseCaptureResult 是开始抓包的结果：本次会话 id + 租约快照。
type StartLeaseCaptureResult struct {
	OK       bool
	Message  string
	SessionID string
	Lease    ProxyLease
}

// StopLeaseCaptureResult 是停止抓包的结果：被停会话 id + 本次统计。
type StopLeaseCaptureResult struct {
	OK          bool
	Message     string
	SessionID   string
	RawPackets  int64
	Events      int64
	DurationSec float64
}

// ReleaseProxyLeaseResult 是释放租约的结果。
type ReleaseProxyLeaseResult struct {
	OK        bool
	Message   string
	SessionID string
}

// PluginEvent 是插件注册表状态变化通知（与 proto PluginEvent 对应，但用 Go 原生类型）。
type PluginEvent struct {
	Type       string
	InstanceID string
	Name       string
	Online     bool
	Timestamp  time.Time
}

// DecodeRawPacketsRequest 离线解码请求（与 proto 对应但不含 protobuf 类型）。
type DecodeRawPacketsRequest struct {
	SessionID     string
	Plugin        string
	Protocol      string
	Src           string
	Dst           string
	Limit         int64
	ClearExisting bool
}

// DecodeRawPacketsResult 离线解码结果统计。
type DecodeRawPacketsResult struct {
	TotalRaw     int64
	Decoded      int64
	DecodeErrors int64
}

// TestPluginRequest 插件测试请求（与 proto 对应但不含 protobuf 类型）。
type TestPluginRequest struct {
	SessionID   string
	Plugin      string
	Protocol    string
	Src         string
	Dst         string
	Limit       int64 // 测试包上限，0=全部
	SampleLimit int64 // 返回的解码事件采样上限，0=默认 50
}

// TestEventLite 采样解码事件（插件解出来的相关数据，不含原始字节）。
type TestEventLite struct {
	ID           string
	TimestampUnix int64
	Type         string
	SchemaID     string
	DataJSON     string // 拍平后的关键 data.* 字段 JSON（可能截断）
}

// TestErrorLite 单个解码失败样例（仅含定位信息，不含原始字节）。
type TestErrorLite struct {
	RawPacketID string
	Src         string
	Dst         string
	Error       string
}

// TestPluginResult 插件测试结果：计数 + 类型分布 + 采样事件 + 错误样例。
type TestPluginResult struct {
	TotalRaw      int64
	Decoded       int64
	DecodeErrors  int64
	TypeHistogram map[string]int64
	SampleEvents  []TestEventLite
	ErrorSamples  []TestErrorLite
}

// StartSessionRequest 是启动抓包的参数（与 proto 对应但不含 protobuf 类型）。
type StartSessionRequest struct {
	SessionID string
	Plugin    string
	Port      int
	Live      *LiveConfig
	File      *FileConfig
	Mobile    *MobileConfig
	// Agent 为 true 时会话订阅 agent capture source（可与其他 source 组合，
	// 单独为 true 表示纯 agent 会话）。
	Agent bool
	// ProjectID 会话归属的项目（projects.id），可选，透传自 proto StartCaptureRequest.project_id。
	ProjectID string
}

// LiveConfig 对应 proto PcapLiveConfig。
type LiveConfig struct {
	Device  string
	BPF     string
	SnapLen int32
	Promisc bool
}

// FileConfig 对应 proto PcapFileConfig。
type FileConfig struct {
	Path string
}

// MobileConfig 对应 proto MobileSourceConfig。
type MobileConfig struct {
	ListenAddr string
	// Activity 可选的运行时活动追踪器（mobile.Activity），由 pipeline 代理抓包
	// 常驻会话注入，用于向控制面暴露手机连接实时状态（活跃连接/累计字节等）。
	// 非代理抓包场景（普通 mobile 会话）为 nil。
	Activity *mobile.Activity
}

// StartSessionResult 是启动结果。
type StartSessionResult struct {
	SessionID string
	State     string
	DBPath    string
}

// StopSessionResult 是停止结果。
type StopSessionResult struct {
	State        string
	RawPackets   int64
	Events       int64
	Metrics      int64
	DecodeErrors int64
	DurationSec  float64
}

// StatusResult 是状态查询结果。
type StatusResult struct {
	State        string
	SourceName   string
	PacketsIn    uint64
	PacketsOut   uint64
	BytesIn      uint64
	BytesOut     uint64
	Drops        uint64
	Errors       uint64
	Err          string
	RawCount     int64
	EventCount   int64
	MetricCount  int64
	DecodeErrors int64
	// AgentConnected：agent 源会话当前是否有活跃的 agent 推流连接。
	// true 只代表"连上了"，不代表有流量 —— 让上层能把"已连接但零流量"
	// 与"没连上"分开呈现。非 agent 源恒为 false。
	AgentConnected bool
	// AgentLastSeenUnix：最近一次收到该会话 agent 数据包的 Unix 秒；0 = 从未收到。
	AgentLastSeenUnix int64
}

// SessionSummary 是 ListSessions 返回的活跃会话摘要。
type SessionSummary struct {
	SessionID  string
	State      string
	SourceName string
	Port       int
	Plugin     string
	Interface  string
	PCAPFile   string
	Start      time.Time
}

// PluginSummary 是 ListPlugins 返回的插件摘要。
type PluginSummary struct {
	InstanceID     string
	Name           string
	Protocol       string
	Type           string
	APIVersion     string
	SocketPath     string
	Online         bool
	LastHeartbeat  time.Time
	Owner          string
}

// Server 实现 pb.CaptureControlServer，委托给 CaptureEngine。
type Server struct {
	pb.UnimplementedCaptureControlServer
	mu     sync.Mutex
	engine CaptureEngine
}

// NewServer 创建 CaptureControl server。
func NewServer(engine CaptureEngine) *Server {
	return &Server{engine: engine}
}

// StartCapture 处理启动抓包 RPC。
func (s *Server) StartCapture(ctx context.Context, req *pb.StartCaptureRequest) (*pb.StartCaptureResponse, error) {
	// 透传调用方身份：engine（pipeline_service）用它记录会话归属与 owner 作用域插件路由。
	ctx = withRequestOwner(ctx, req.GetOwner(), req.GetAllOwners())
	engineReq := StartSessionRequest{
		SessionID: req.GetSessionId(),
		Plugin:    req.GetPlugin(),
		Port:      int(req.GetPort()),
		Agent:     req.GetAgent(),
		ProjectID: req.GetProjectId(),
	}
	switch src := req.GetSource().(type) {
	case *pb.StartCaptureRequest_Live:
		engineReq.Live = &LiveConfig{
			Device:  src.Live.GetDevice(),
			BPF:     src.Live.GetBpf(),
			SnapLen: src.Live.GetSnapLen(),
			Promisc: src.Live.GetPromisc(),
		}
	case *pb.StartCaptureRequest_File:
		engineReq.File = &FileConfig{Path: src.File.GetPath()}
	case *pb.StartCaptureRequest_Mobile:
		engineReq.Mobile = &MobileConfig{
			ListenAddr: src.Mobile.GetListenAddr(),
		}
	}
	res, err := s.engine.StartSession(ctx, engineReq)
	if err != nil {
		return nil, err
	}
	return &pb.StartCaptureResponse{
		SessionId: res.SessionID,
		State:     res.State,
		DbPath:    res.DBPath,
	}, nil
}

// StopCapture 处理停止抓包 RPC。
func (s *Server) StopCapture(ctx context.Context, req *pb.StopCaptureRequest) (*pb.StopCaptureResponse, error) {
	res, err := s.engine.StopSession(ctx, req.GetSessionId())
	if err != nil {
		return nil, err
	}
	return &pb.StopCaptureResponse{
		State:        res.State,
		RawPackets:   res.RawPackets,
		Events:       res.Events,
		Metrics:      res.Metrics,
		DecodeErrors: res.DecodeErrors,
		DurationSec:  res.DurationSec,
	}, nil
}

// GetCaptureStatus 处理状态查询 RPC。
func (s *Server) GetCaptureStatus(ctx context.Context, req *pb.GetCaptureStatusRequest) (*pb.GetCaptureStatusResponse, error) {
	res, err := s.engine.GetStatus(ctx, req.GetSessionId())
	if err != nil {
		return nil, err
	}
	return &pb.GetCaptureStatusResponse{
		State:        res.State,
		SourceName:   res.SourceName,
		PacketsIn:    res.PacketsIn,
		PacketsOut:   res.PacketsOut,
		BytesIn:      res.BytesIn,
		BytesOut:     res.BytesOut,
		Drops:        res.Drops,
		Errors:       res.Errors,
		Err:          res.Err,
		RawCount:           res.RawCount,
		EventCount:         res.EventCount,
		MetricCount:        res.MetricCount,
		DecodeErrors:       res.DecodeErrors,
		AgentConnected:     res.AgentConnected,
		AgentLastSeenUnix:  res.AgentLastSeenUnix,
	}, nil
}

// ListCaptureSessions 处理列出活跃会话 RPC。
func (s *Server) ListCaptureSessions(ctx context.Context, req *pb.ListCaptureSessionsRequest) (*pb.ListCaptureSessionsResponse, error) {
	sessions, err := s.engine.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*pb.CaptureSessionSummary, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, &pb.CaptureSessionSummary{
			SessionId:     sess.SessionID,
			State:         sess.State,
			SourceName:    sess.SourceName,
			Port:          int32(sess.Port),
			Plugin:        sess.Plugin,
			Interface:     sess.Interface,
			PcapFile:      sess.PCAPFile,
			StartedAtUnix: sess.Start.Unix(),
		})
	}
	return &pb.ListCaptureSessionsResponse{Sessions: out}, nil
}

// ListInterfaces 处理网卡列表 RPC。
func (s *Server) ListInterfaces(ctx context.Context, req *pb.ListInterfacesRequest) (*pb.ListInterfacesResponse, error) {
	names, err := s.engine.ListInterfaces(ctx)
	if err != nil {
		return nil, err
	}
	return &pb.ListInterfacesResponse{Names: names}, nil
}

// DecodeRawPackets 处理离线解码 RPC。
func (s *Server) DecodeRawPackets(ctx context.Context, req *pb.DecodeRawPacketsRequest) (*pb.DecodeRawPacketsResponse, error) {
	res, err := s.engine.DecodeRawPackets(ctx, DecodeRawPacketsRequest{
		SessionID:     req.GetSessionId(),
		Plugin:        req.GetPlugin(),
		Protocol:      req.GetProtocol(),
		Src:           req.GetSrc(),
		Dst:           req.GetDst(),
		Limit:         req.GetLimit(),
		ClearExisting: req.GetClearExisting(),
	})
	if err != nil {
		return nil, err
	}
	return &pb.DecodeRawPacketsResponse{
		TotalRaw:     res.TotalRaw,
		Decoded:      res.Decoded,
		DecodeErrors: res.DecodeErrors,
	}, nil
}

// TestPlugin 处理插件测试 RPC（隐私安全：不回传原始包，结果不落库）。
func (s *Server) TestPlugin(ctx context.Context, req *pb.TestPluginRequest) (*pb.TestPluginResponse, error) {
	res, err := s.engine.TestPlugin(ctx, TestPluginRequest{
		SessionID:   req.GetSessionId(),
		Plugin:      req.GetPlugin(),
		Protocol:    req.GetProtocol(),
		Src:         req.GetSrc(),
		Dst:         req.GetDst(),
		Limit:       req.GetLimit(),
		SampleLimit: req.GetSampleLimit(),
	})
	if err != nil {
		return nil, err
	}
	sampleEvents := make([]*pb.TestEventLite, 0, len(res.SampleEvents))
	for _, e := range res.SampleEvents {
		sampleEvents = append(sampleEvents, &pb.TestEventLite{
			Id:            e.ID,
			TimestampUnix: e.TimestampUnix,
			Type:          e.Type,
			SchemaId:      e.SchemaID,
			DataJson:      e.DataJSON,
		})
	}
	errorSamples := make([]*pb.TestErrorLite, 0, len(res.ErrorSamples))
	for _, e := range res.ErrorSamples {
		errorSamples = append(errorSamples, &pb.TestErrorLite{
			RawPacketId: e.RawPacketID,
			Src:         e.Src,
			Dst:         e.Dst,
			Error:       e.Error,
		})
	}
	return &pb.TestPluginResponse{
		TotalRaw:      res.TotalRaw,
		Decoded:       res.Decoded,
		DecodeErrors:  res.DecodeErrors,
		TypeHistogram: res.TypeHistogram,
		SampleEvents:  sampleEvents,
		ErrorSamples:  errorSamples,
	}, nil
}

// withRequestOwner 把 MCP 透传来的调用方身份（owner/all_owners）注入 ctx，
// 使 engine 侧能经 auth.OwnerFrom/auth.PrincipalFrom 做 owner 作用域过滤。
// 两者均为空表示匿名/本地语境，不注入（engine 侧 OwnerFrom 返回 ""，行为不变）。
//
// 信任边界：owner/all_owners 是 RPC 请求字段，不由 gRPC 层校验——本 server
// 假定 CaptureControl 监听在 localhost、唯一客户端是同机的 gta-mcp（管道内
// 已有 HTTP Bearer 鉴权）。任何能直接连上该端口的进程都可伪造身份；若要把
// 监听开放到非回环地址，必须先接入与 HTTP 侧同级的 gRPC Bearer 拦截器
// （pkg/auth.UnaryInterceptor）。
func withRequestOwner(ctx context.Context, owner string, allOwners bool) context.Context {
	if owner == "" && !allOwners {
		return ctx
	}
	return auth.WithPrincipal(ctx, &auth.Principal{Owner: owner, IsAdmin: allOwners})
}

// ListPlugins 处理列出已注册插件 RPC。
func (s *Server) ListPlugins(ctx context.Context, req *pb.ListPluginsRequest) (*pb.ListPluginsResponse, error) {
	plugins, err := s.engine.ListPlugins(withRequestOwner(ctx, req.GetOwner(), req.GetAllOwners()))
	if err != nil {
		return nil, err
	}
	out := make([]*pb.PluginSummary, 0, len(plugins))
	for _, p := range plugins {
		out = append(out, &pb.PluginSummary{
			InstanceId:        p.InstanceID,
			Name:              p.Name,
			Protocol:          p.Protocol,
			Type:              p.Type,
			ApiVersion:        p.APIVersion,
			SocketPath:        p.SocketPath,
			Online:            p.Online,
			LastHeartbeatUnix: p.LastHeartbeat.Unix(),
			Owner:             p.Owner,
		})
	}
	return &pb.ListPluginsResponse{Plugins: out}, nil
}

// GetPluginManifest 处理获取插件 manifest RPC。
func (s *Server) GetPluginManifest(ctx context.Context, req *pb.GetPluginManifestRequest) (*pb.GetPluginManifestResponse, error) {
	manifest, err := s.engine.GetPluginManifest(withRequestOwner(ctx, req.GetOwner(), req.GetAllOwners()), req.GetName())
	if err != nil {
		return nil, err
	}
	return &pb.GetPluginManifestResponse{
		Manifest: manifest,
		Name:     req.GetName(),
	}, nil
}

// GetRegistryAddr 处理获取注册中心地址 RPC。返回插件应连接的注册中心地址
// （即 gta-pipeline 的 -registry-addr，如 :9091），供 gta-mcp / 插件启动时填入
// GTA_REGISTRY_ADDR。
func (s *Server) GetRegistryAddr(ctx context.Context, req *pb.GetRegistryAddrRequest) (*pb.GetRegistryAddrResponse, error) {
	addr, err := s.engine.GetRegistryAddr(ctx)
	if err != nil {
		return nil, err
	}
	return &pb.GetRegistryAddrResponse{RegistryAddr: addr}, nil
}

// DeregisterPlugin 处理注销插件 RPC。
func (s *Server) DeregisterPlugin(ctx context.Context, req *pb.DeregisterPluginRequest) (*pb.DeregisterPluginResponse, error) {
	instanceID, err := s.engine.DeregisterPlugin(ctx, req.GetInstanceId(), req.GetName())
	if err != nil {
		return nil, err
	}
	return &pb.DeregisterPluginResponse{
		Ok:         true,
		InstanceId: instanceID,
		Name:       req.GetName(),
	}, nil
}

// SetSessionPlugin 处理运行中热切换解码插件绑定 RPC。
func (s *Server) SetSessionPlugin(ctx context.Context, req *pb.SetSessionPluginRequest) (*pb.SetSessionPluginResponse, error) {
	plugin, err := s.engine.SetSessionPlugin(ctx, req.GetSessionId(), req.GetPlugin())
	if err != nil {
		return &pb.SetSessionPluginResponse{
			Ok:         false,
			SessionId:  req.GetSessionId(),
			Plugin:     req.GetPlugin(),
			Message:    err.Error(),
		}, nil
	}
	return &pb.SetSessionPluginResponse{
		Ok:        true,
		SessionId: req.GetSessionId(),
		Plugin:    plugin,
	}, nil
}

// 编译期断言。
var _ pb.CaptureControlServer = (*Server)(nil)

// WatchPlugins 服务端流式推送插件注册表状态变化。
// 委托引擎的 SubscribePlugins 获取事件通道，逐条转发为 proto PluginEvent。
// 客户端断开（stream.Context 取消）后通道关闭，本方法返回。
func (s *Server) WatchPlugins(req *pb.WatchPluginsRequest, stream grpc.ServerStreamingServer[pb.PluginEvent]) error {
	ch, err := s.engine.SubscribePlugins(stream.Context())
	if err != nil {
		return err
	}
	for ev := range ch {
		if err := stream.Send(&pb.PluginEvent{
			Type:          ev.Type,
			InstanceId:    ev.InstanceID,
			Name:          ev.Name,
			Online:        ev.Online,
			TimestampUnix: ev.Timestamp.Unix(),
		}); err != nil {
			return err
		}
	}
	return nil
}

// CreateProxyLease 处理创建代理抓包租约 RPC。
// 身份透传同 StartCapture（withRequestOwner 注入 ctx，engine 侧记录归属）。
func (s *Server) CreateProxyLease(ctx context.Context, req *pb.CreateProxyLeaseRequest) (*pb.CreateProxyLeaseResponse, error) {
	lease, err := s.engine.CreateProxyLease(withRequestOwner(ctx, req.GetOwner(), req.GetAllOwners()), CreateProxyLeaseRequest{
		Plugin:       req.GetPlugin(),
		IncludeHosts: req.GetIncludeHosts(),
		IncludePorts: req.GetIncludePorts(),
		Device:       req.GetDevice(),
		ProjectID:    req.GetProjectId(),
		NoAutoStart:  req.GetNoAutoStart(),
		NoSticky:     req.GetNoSticky(),
	})
	if err != nil {
		return nil, err
	}
	return &pb.CreateProxyLeaseResponse{Lease: leaseToProto(lease)}, nil
}

// ListProxyLeases 处理列出租约 RPC（owner 作用域过滤在 engine 侧）。
func (s *Server) ListProxyLeases(ctx context.Context, req *pb.ListProxyLeasesRequest) (*pb.ListProxyLeasesResponse, error) {
	leases, err := s.engine.ListProxyLeases(withRequestOwner(ctx, req.GetOwner(), req.GetAllOwners()))
	if err != nil {
		return nil, err
	}
	out := make([]*pb.ProxyLeaseState, 0, len(leases))
	for _, l := range leases {
		out = append(out, leaseToProto(l))
	}
	return &pb.ListProxyLeasesResponse{Leases: out}, nil
}

// GetProxyLease 处理查询单个租约 RPC（engine 侧做 owner 校验）。
func (s *Server) GetProxyLease(ctx context.Context, req *pb.GetProxyLeaseRequest) (*pb.GetProxyLeaseResponse, error) {
	lease, err := s.engine.GetProxyLease(withRequestOwner(ctx, req.GetOwner(), req.GetAllOwners()), req.GetLeaseId())
	if err != nil {
		return nil, err
	}
	return &pb.GetProxyLeaseResponse{Lease: leaseToProto(lease)}, nil
}

// ReleaseProxyLease 处理释放租约 RPC。错误走 ok=false + message 说明。
func (s *Server) ReleaseProxyLease(ctx context.Context, req *pb.ReleaseProxyLeaseRequest) (*pb.ReleaseProxyLeaseResponse, error) {
	res, err := s.engine.ReleaseProxyLease(withRequestOwner(ctx, req.GetOwner(), req.GetAllOwners()), req.GetLeaseId())
	if err != nil {
		return &pb.ReleaseProxyLeaseResponse{
			Ok:        false,
			Message:   err.Error(),
			SessionId: req.GetLeaseId(),
		}, nil
	}
	return &pb.ReleaseProxyLeaseResponse{
		Ok:        res.OK,
		Message:   res.Message,
		SessionId: res.SessionID,
	}, nil
}

// StartLeaseCapture 处理「在常驻租约上开一次抓包」RPC。
// 失败一律转成 error（前端据此提示），成功返回本次会话 id 与租约快照。
func (s *Server) StartLeaseCapture(ctx context.Context, req *pb.StartLeaseCaptureRequest) (*pb.StartLeaseCaptureResponse, error) {
	res, err := s.engine.StartLeaseCapture(withRequestOwner(ctx, req.GetOwner(), req.GetAllOwners()), StartLeaseCaptureRequest{
		LeaseID:      req.GetLeaseId(),
		Plugin:       req.GetPlugin(),
		IncludeHosts: req.GetIncludeHosts(),
		IncludePorts: req.GetIncludePorts(),
	})
	if err != nil {
		return nil, err
	}
	return &pb.StartLeaseCaptureResponse{
		Ok:        res.OK,
		Message:   res.Message,
		SessionId: res.SessionID,
		Lease:     leaseToProto(res.Lease),
	}, nil
}

// StopLeaseCapture 处理「停止租约当前抓包」RPC。出口保留、手机连接不受影响。
func (s *Server) StopLeaseCapture(ctx context.Context, req *pb.StopLeaseCaptureRequest) (*pb.StopLeaseCaptureResponse, error) {
	res, err := s.engine.StopLeaseCapture(withRequestOwner(ctx, req.GetOwner(), req.GetAllOwners()), req.GetLeaseId())
	if err != nil {
		return nil, err
	}
	return &pb.StopLeaseCaptureResponse{
		Ok:          res.OK,
		Message:     res.Message,
		SessionId:   res.SessionID,
		RawPackets:  res.RawPackets,
		Events:      res.Events,
		DurationSec: res.DurationSec,
	}, nil
}

// leaseToProto 把 Go 侧 ProxyLease 快照转为 proto ProxyLeaseState。
func leaseToProto(l ProxyLease) *pb.ProxyLeaseState {
	ports := make([]int32, 0, len(l.IncludePorts))
	for _, p := range l.IncludePorts {
		ports = append(ports, int32(p))
	}
	return &pb.ProxyLeaseState{
		LeaseId:         l.LeaseID,
		Owner:           l.Owner,
		ProjectId:       l.ProjectID,
		Plugin:          l.Plugin,
		IncludeHosts:    l.IncludeHosts,
		IncludePorts:    ports,
		Device:          l.Device,
		ListenAddr:      l.ListenAddr,
		AgentListenPort: int32(l.AgentListenPort),
		MobileGrpcPort:  int32(l.MobileGRPCPort),
		AgentRunning:    l.AgentRunning,
		AgentPid:        l.AgentPID,
		SessionRunning:  l.SessionRunning,
		SessionId:       l.SessionID,
		CreatedAtUnix:   l.CreatedAt.Unix(),
		ActiveConns:     l.ActiveConns,
		TotalConns:      l.TotalConns,
		LastDataUnix:    l.LastDataUnix,
		TotalBytes:      l.TotalBytes,
		CaptureRunning:  l.CaptureRunning,
		ControlPort:     int32(l.ControlPort),
		CaptureCount:    int32(l.CaptureCount),
		LastCaptureAtUnix: l.LastCaptureAtUnix,
		StickyPort:      l.StickyPort,
	}
}
