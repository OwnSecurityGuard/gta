// Package capturecontrol 实现 CaptureControl gRPC server。
// 由 gta-pipeline 嵌入使用，处理 start/stop/status/list_interfaces 控制命令。
// 实际抓包逻辑由 gta-pipeline 的 captureEngine 提供，server 仅做 RPC 适配。
package capturecontrol

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc"

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
	// GetProxyConfig 返回当前代理抓包服务器配置与运行时状态。
	GetProxyConfig(ctx context.Context) (ProxyConfigState, error)
	// UpdateProxyConfig 应用新的代理抓包服务器配置（持久化 + 热重启 agent + 常驻会话）。
	UpdateProxyConfig(ctx context.Context, req ProxyConfigUpdate) (ProxyConfigState, error)
}

// ProxyConfigState 是代理抓包服务器的配置 + 运行时状态快照（与 proto 对应）。
type ProxyConfigState struct {
	ListenAddr     string
	ServerAddr     string
	FrameStyle     string
	PrefixLen      int32
	LittleEndian   bool
	AgentRunning   bool
	AgentPID       int32
	SessionRunning bool
	SessionID      string
	ConfigPath     string
	Plugin         string
	IncludeHosts   []string
	IncludePorts   []int32
}

// ProxyConfigUpdate 是新的代理抓包服务器配置（字段为空表示不修改）。
type ProxyConfigUpdate struct {
	ListenAddr   string
	ServerAddr   string
	FrameStyle   string
	PrefixLen    int32
	LittleEndian bool
	Plugin       string
	IncludeHosts []string
	IncludePorts []int32
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
	ListenAddr   string
	FrameStyle   string
	PrefixLen    int
	LittleEndian bool
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
	engineReq := StartSessionRequest{
		SessionID: req.GetSessionId(),
		Plugin:    req.GetPlugin(),
		Port:      int(req.GetPort()),
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
			ListenAddr:   src.Mobile.GetListenAddr(),
			FrameStyle:   src.Mobile.GetFrameStyle(),
			PrefixLen:    int(src.Mobile.GetPrefixLen()),
			LittleEndian: src.Mobile.GetLittleEndian(),
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
		RawCount:     res.RawCount,
		EventCount:   res.EventCount,
		MetricCount:  res.MetricCount,
		DecodeErrors: res.DecodeErrors,
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

// ListPlugins 处理列出已注册插件 RPC。
func (s *Server) ListPlugins(ctx context.Context, req *pb.ListPluginsRequest) (*pb.ListPluginsResponse, error) {
	plugins, err := s.engine.ListPlugins(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*pb.PluginSummary, 0, len(plugins))
	for _, p := range plugins {
		out = append(out, &pb.PluginSummary{
			InstanceId:      p.InstanceID,
			Name:            p.Name,
			Protocol:        p.Protocol,
			Type:            p.Type,
			ApiVersion:      p.APIVersion,
			SocketPath:      p.SocketPath,
			Online:          p.Online,
			LastHeartbeatUnix: p.LastHeartbeat.Unix(),
		})
	}
	return &pb.ListPluginsResponse{Plugins: out}, nil
}

// GetPluginManifest 处理获取插件 manifest RPC。
func (s *Server) GetPluginManifest(ctx context.Context, req *pb.GetPluginManifestRequest) (*pb.GetPluginManifestResponse, error) {
	manifest, err := s.engine.GetPluginManifest(ctx, req.GetName())
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

// GetProxyConfig 处理查询代理抓包服务器配置 RPC。
func (s *Server) GetProxyConfig(ctx context.Context, req *pb.GetProxyConfigRequest) (*pb.GetProxyConfigResponse, error) {
	st, err := s.engine.GetProxyConfig(ctx)
	if err != nil {
		return nil, err
	}
	return &pb.GetProxyConfigResponse{State: stateToProto(st)}, nil
}

// UpdateProxyConfig 处理应用代理抓包服务器配置 RPC。
// 引擎负责持久化 + 热重启 agent + 确保代理会话常驻；失败时 ok=false + message 说明。
func (s *Server) UpdateProxyConfig(ctx context.Context, req *pb.UpdateProxyConfigRequest) (*pb.UpdateProxyConfigResponse, error) {
	st, err := s.engine.UpdateProxyConfig(ctx, ProxyConfigUpdate{
		ListenAddr:   req.GetListenAddr(),
		ServerAddr:   req.GetServerAddr(),
		FrameStyle:   req.GetFrameStyle(),
		PrefixLen:    req.GetPrefixLen(),
		LittleEndian: req.GetLittleEndian(),
		Plugin:       req.GetPlugin(),
		IncludeHosts: req.GetIncludeHosts(),
		IncludePorts: req.GetIncludePorts(),
	})
	if err != nil {
		return &pb.UpdateProxyConfigResponse{
			Ok:      false,
			Message: err.Error(),
		}, nil
	}
	return &pb.UpdateProxyConfigResponse{
		Ok:      true,
		Message: "proxy server config applied",
		State:   stateToProto(st),
	}, nil
}

func stateToProto(st ProxyConfigState) *pb.ProxyConfigState {
	return &pb.ProxyConfigState{
		ListenAddr:     st.ListenAddr,
		ServerAddr:     st.ServerAddr,
		FrameStyle:     st.FrameStyle,
		PrefixLen:      st.PrefixLen,
		LittleEndian:   st.LittleEndian,
		AgentRunning:   st.AgentRunning,
		AgentPid:       st.AgentPID,
		SessionRunning: st.SessionRunning,
		SessionId:      st.SessionID,
		ConfigPath:     st.ConfigPath,
		Plugin:         st.Plugin,
		IncludeHosts:   st.IncludeHosts,
		IncludePorts:   st.IncludePorts,
	}
}
