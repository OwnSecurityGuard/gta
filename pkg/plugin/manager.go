package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdkcontract "github.com/OwnSecurityGuard/gta-plugin-sdk/contract"
	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
	"gta/pkg/schema"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gopkg.in/yaml.v3"
)

// RegisteredPlugin 表示一个已注册的插件实例。
type RegisteredPlugin struct {
	InstanceID    string
	SocketPath    string
	Manifest      *Manifest
	Client        pb.DecoderClient
	Conn          *grpc.ClientConn
	LastHeartbeat time.Time
	Online        atomic.Bool // true 表示最近有心跳，false 表示超时未心跳
}

// SchemaRegistry returns the schema registry for this plugin's manifest.
func (rp *RegisteredPlugin) SchemaRegistry() *schema.Registry {
	if rp.Manifest == nil {
		return schema.NewRegistry()
	}
	return ToSchemaRegistry(rp.Manifest)
}

// PluginSummary 是插件注册信息的摘要，用于对外暴露。
type PluginSummary struct {
	InstanceID    string
	Name          string
	Protocol      string
	Type          string
	APIVersion    string
	SocketPath    string
	Online        bool
	LastHeartbeat time.Time
}

// PluginEventType 表示插件注册表状态变化的类型。
type PluginEventType string

const (
	// PluginEventRegister 新插件注册（进程启动并成功注册）。
	PluginEventRegister PluginEventType = "register"
	// PluginEventDeregister 插件主动注销（进程退出时调用 Deregister）。
	PluginEventDeregister PluginEventType = "deregister"
	// PluginEventOnline 插件由离线恢复在线（心跳恢复，离线→在线翻转）。
	PluginEventOnline PluginEventType = "online"
	// PluginEventOffline 插件心跳超时被判离线（在线→离线翻转）。
	PluginEventOffline PluginEventType = "offline"
)

// PluginEvent 是插件注册表状态变化的通知，用于即时推送（避免轮询）。
type PluginEvent struct {
	Type       PluginEventType
	InstanceID string
	Name       string
	Online     bool
	Timestamp  time.Time
}

// RegistryServer 实现 PluginRegistry gRPC 服务，被动接受插件注册。
// 插件进程由外部编排（systemd/脚本）独立启动，RegistryServer 不再 spawn 子进程，
// 也不再维护 watcher/restart 逻辑。
type RegistryServer struct {
	pb.UnimplementedPluginRegistryServer
	mu           sync.RWMutex
	plugins      map[string]*RegisteredPlugin // key: instance_id
	byName       map[string]string            // key: manifest name → instance_id
	nextID       atomic.Int64
	heartbeatSec int32

	// 事件总线：插件注册/注销/上下线时向订阅者推送 PluginEvent。
	// 订阅者通道带缓冲，emit 非阻塞（订阅者处理不过来则丢弃，避免阻塞注册表主路径）。
	listenerMu  sync.Mutex
	listenerSeq int64
	listeners   map[int64]chan PluginEvent
}

// NewRegistryServer 创建注册服务器。heartbeatSec 是要求插件的心跳间隔，
// 非正数时默认 10 秒。
func NewRegistryServer(heartbeatSec int32) *RegistryServer {
	if heartbeatSec <= 0 {
		heartbeatSec = 10
	}
	return &RegistryServer{
		plugins:      map[string]*RegisteredPlugin{},
		byName:       map[string]string{},
		heartbeatSec: heartbeatSec,
		listeners:    map[int64]chan PluginEvent{},
	}
}

// Subscribe 订阅插件注册表状态变化事件。
// 返回只读事件通道与退订函数；退订后通道会被关闭。
func (s *RegistryServer) Subscribe() (<-chan PluginEvent, func()) {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	id := s.listenerSeq
	s.listenerSeq++
	ch := make(chan PluginEvent, 16)
	s.listeners[id] = ch
	unsub := func() {
		s.listenerMu.Lock()
		defer s.listenerMu.Unlock()
		if c, ok := s.listeners[id]; ok {
			delete(s.listeners, id)
			close(c)
		}
	}
	return ch, unsub
}

// emit 向所有订阅者非阻塞推送事件。
func (s *RegistryServer) emit(event PluginEvent) {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	for _, ch := range s.listeners {
		select {
		case ch <- event:
		default:
			// 订阅者消费不及时则丢弃，避免阻塞注册表主路径
		}
	}
}

// Register 处理插件注册请求。
// 流程：解析 manifest → 校验 manifest → 版本协商 → 拨号插件 Decode socket 验证可达 → 分配 instance_id。
func (s *RegistryServer) Register(ctx context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	// 1. 解析 manifest
	m, err := ParseManifest(req.Manifest)
	if err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	// 2. 校验 manifest
	if err := ValidateManifest(m); err != nil {
		return nil, fmt.Errorf("validate manifest: %w", err)
	}
	// 3. 版本协商
	if err := CheckManifestVersion(m); err != nil {
		return nil, fmt.Errorf("version check: %w", err)
	}
	// 3.5 语义契约声明期校验（Semantic Contract v1 两层：schema/state）。
	//     error 级违规拒绝注册；warn 级放行但记日志，让插件作者能在 plugin.verify 看到全量报告。
	if report := sdkcontract.NewPluginChecker().Check(m); report != nil {
		if report.HasErrors() {
			return nil, fmt.Errorf("semantic contract check failed: %s", formatReport(report))
		}
		for _, v := range report.Violations {
			slog.Warn("plugin manifest semantic warn", "name", m.Name, "rule", v.RuleID, "detail", v.Message)
		}
	}
	// 4. 拨号到插件的 Decode socket 验证可达
	//    SocketPath 可能是 host:port（跨机器部署）、unix:/path 或 npipe:\\.\pipe\name。
	conn, err := dialDecoder(ctx, req.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("dial plugin socket: %w", err)
	}
	client := pb.NewDecoderClient(conn)

	// 5. 分配 instance_id
	instanceID := fmt.Sprintf("%s-%d", m.Name, s.nextID.Add(1))

	rp := &RegisteredPlugin{
		InstanceID:    instanceID,
		SocketPath:    req.SocketPath,
		Manifest:      m,
		Client:        client,
		Conn:          conn,
		LastHeartbeat: time.Now(),
	}
	rp.Online.Store(true)

	s.mu.Lock()
	// 若同名插件已注册，关闭旧连接（崩溃后重启场景）
	if oldID, ok := s.byName[m.Name]; ok {
		if old, ok := s.plugins[oldID]; ok {
			_ = old.Conn.Close()
			delete(s.plugins, oldID)
		}
	}
	s.plugins[instanceID] = rp
	s.byName[m.Name] = instanceID
	s.mu.Unlock()

	slog.Info("plugin registered", "name", m.Name, "instance_id", instanceID, "protocol", m.Protocol)

	s.emit(PluginEvent{
		Type:       PluginEventRegister,
		InstanceID: instanceID,
		Name:       m.Name,
		Online:     true,
		Timestamp:  time.Now(),
	})

	return &pb.RegisterResponse{
		InstanceId:           instanceID,
		HeartbeatIntervalSec: s.heartbeatSec,
	}, nil
}

// formatReport 把语义契约 Report 压缩为适合注册错误消息的单行摘要，
// 完整 JSON 形态由 plugin.verify 输出。
func formatReport(r *sdkcontract.Report) string {
	var sb strings.Builder
	for i, v := range r.Violations {
		if i >= 5 {
			fmt.Fprintf(&sb, " (+%d more)", len(r.Violations)-i)
			break
		}
		if i > 0 {
			sb.WriteString("; ")
		}
		sb.WriteString(v.Error())
	}
	return sb.String()
}

// Heartbeat 处理插件心跳，更新 LastHeartbeat 并标记为在线。
func (s *RegistryServer) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	s.mu.Lock()
	rp, ok := s.plugins[req.InstanceId]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("unknown instance_id: %s", req.InstanceId)
	}
	wasOnline := rp.Online.Load()
	rp.LastHeartbeat = time.Now()
	rp.Online.Store(true)
	name := rp.Manifest.Name
	instanceID := req.InstanceId
	s.mu.Unlock()

	if !wasOnline {
		// 离线 → 在线翻转（心跳恢复），推送 online 事件。
		s.emit(PluginEvent{
			Type:       PluginEventOnline,
			InstanceID: instanceID,
			Name:       name,
			Online:     true,
			Timestamp:  time.Now(),
		})
	}
	return &pb.HeartbeatResponse{}, nil
}

// Deregister 处理插件主动下线，关闭连接并从注册表移除。
func (s *RegistryServer) Deregister(ctx context.Context, req *pb.DeregisterRequest) (*pb.DeregisterResponse, error) {
	s.mu.Lock()
	rp, ok := s.plugins[req.InstanceId]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("unknown instance_id: %s", req.InstanceId)
	}
	name := rp.Manifest.Name
	instanceID := req.InstanceId
	_ = rp.Conn.Close()
	delete(s.plugins, instanceID)
	delete(s.byName, name)
	s.mu.Unlock()

	slog.Info("plugin deregistered", "instance_id", instanceID, "name", name)

	s.emit(PluginEvent{
		Type:       PluginEventDeregister,
		InstanceID: instanceID,
		Name:       name,
		Online:     false,
		Timestamp:  time.Now(),
	})

	return &pb.DeregisterResponse{}, nil
}

// CheckOffline 扫描注册表，将心跳超时的插件标记下线。
// 应由外部定时调用（如每秒）。
func (s *RegistryServer) CheckOffline(timeout time.Duration) {
	s.mu.Lock()
	now := time.Now()
	var transitions []PluginEvent
	for id, rp := range s.plugins {
		if now.Sub(rp.LastHeartbeat) > timeout && rp.Online.Load() {
			rp.Online.Store(false)
			transitions = append(transitions, PluginEvent{
				Type:       PluginEventOffline,
				InstanceID: id,
				Name:       rp.Manifest.Name,
				Online:     false,
				Timestamp:  now,
			})
		}
	}
	s.mu.Unlock()

	for _, ev := range transitions {
		slog.Warn("plugin heartbeat timeout, marking offline", "instance_id", ev.InstanceID, "name", ev.Name)
		s.emit(ev)
	}
}

// Find 根据 protocol hint 查找第一个匹配的解码插件。
// 匹配规则：manifest 的 protocol 字段或 hints 列表包含 protocolHint。
// 返回 DecoderClient、schema registry 和是否找到。
func (s *RegistryServer) Find(protocolHint string) (pb.DecoderClient, *schema.Registry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, rp := range s.plugins {
		// 只返回在线插件
		if !rp.Online.Load() {
			continue
		}
		// 匹配 protocol 或 hints
		if rp.Manifest.Protocol == protocolHint {
			return rp.Client, rp.SchemaRegistry(), true
		}
		for _, h := range rp.Manifest.Hints {
			if h == protocolHint {
				return rp.Client, rp.SchemaRegistry(), true
			}
		}
	}
	return nil, nil, false
}

// GetPlugin 按 name 查找已注册插件，返回 Manifest YAML bytes。
// FindByName 按插件名（manifest.name）精确查找已注册的解码插件。
// 用于一次抓包会话绑定特定插件（如 A 项目→插件 A、B 项目→插件 B），
// 使多项目并行抓包、各用各插件、均不重启主服务成为现实。
// 返回 DecoderClient、schema registry 和是否找到。插件离线或不存在时返回 nil, nil, false。
func (s *RegistryServer) FindByName(name string) (pb.DecoderClient, *schema.Registry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byName[name]
	if !ok {
		return nil, nil, false
	}
	rp, ok := s.plugins[id]
	if !ok || !rp.Online.Load() {
		return nil, nil, false
	}
	return rp.Client, rp.SchemaRegistry(), true
}

func (s *RegistryServer) GetPluginManifest(name string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids, ok := s.byName[name]
	if !ok {
		return nil, fmt.Errorf("plugin %q not found", name)
	}
	rp, ok := s.plugins[ids]
	if !ok {
		return nil, fmt.Errorf("plugin %q instance not found", name)
	}
	return yaml.Marshal(rp.Manifest)
}

// NameByClient 返回给定 DecoderClient 对应的插件 manifest name。
// 用于 capture 侧在解码器挂载时反查插件身份（Find/FindByName 只返回 client），
// 进而取 manifest 做规则契约对齐。client 不在任何在线插件下时返回 false。
func (s *RegistryServer) NameByClient(c pb.DecoderClient) (string, bool) {
	if c == nil {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, rp := range s.plugins {
		if rp.Client == c {
			return rp.Manifest.Name, true
		}
	}
	return "", false
}

// List 返回当前已注册插件的快照（全量，含下线状态）。
// 返回 PluginSummary 以避免拷贝含 atomic.Bool 的 RegisteredPlugin。
func (s *RegistryServer) List() []PluginSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]PluginSummary, 0, len(s.plugins))
	for _, rp := range s.plugins {
		out = append(out, PluginSummary{
			InstanceID:    rp.InstanceID,
			Name:          rp.Manifest.Name,
			Protocol:      rp.Manifest.Protocol,
			Type:          rp.Manifest.Type,
			APIVersion:    rp.Manifest.APIVersion,
			SocketPath:    rp.SocketPath,
			Online:        rp.Online.Load(),
			LastHeartbeat: rp.LastHeartbeat,
		})
	}
	return out
}

// ListSummaries 返回当前已注册插件的摘要列表。
func (s *RegistryServer) ListSummaries() []PluginSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]PluginSummary, 0, len(s.plugins))
	for _, rp := range s.plugins {
		out = append(out, PluginSummary{
			InstanceID:    rp.InstanceID,
			Name:          rp.Manifest.Name,
			Protocol:      rp.Manifest.Protocol,
			Type:          rp.Manifest.Type,
			APIVersion:    rp.Manifest.APIVersion,
			SocketPath:    rp.SocketPath,
			Online:        rp.Online.Load(),
			LastHeartbeat: rp.LastHeartbeat,
		})
	}
	return out
}

// WatchOffline 启动心跳超时检测 goroutine。
// timeout 超过此时间的插件标记为下线。
// caller 调用返回的 cancel 函数停止检测。
func (s *RegistryServer) WatchOffline(timeout time.Duration) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.CheckOffline(timeout)
			}
		}
	}()
	return cancel
}

// Close 关闭所有插件连接。
func (s *RegistryServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, rp := range s.plugins {
		_ = rp.Conn.Close()
		delete(s.plugins, id)
	}
	s.byName = map[string]string{}
	return nil
}

// dialTarget 解析插件上报的 Decode 地址并建立 net.Conn。
// 支持 host:port（TCP，跨机器）、unix:/path、npipe:\\.\pipe\name、以及裸路径（视为 Unix socket）。
func dialTarget(ctx context.Context, target string) (net.Conn, error) {
	switch {
	case strings.HasPrefix(target, "unix:"):
		return (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(target, "unix:"))
	case strings.HasPrefix(target, "npipe:"):
		return dialNamedPipe(strings.TrimPrefix(target, "npipe:"))
	case strings.HasPrefix(target, `\\.\pipe\`):
		return dialNamedPipe(target)
	case strings.ContainsRune(target, '/'):
		// 裸路径视为 Unix socket
		return (&net.Dialer{}).DialContext(ctx, "unix", target)
	default:
		// host:port 走 TCP（跨机器部署）
		return (&net.Dialer{}).DialContext(ctx, "tcp", target)
	}
}

// dialDecoder 建立到插件 Decode 服务的 gRPC 连接。
func dialDecoder(ctx context.Context, target string) (*grpc.ClientConn, error) {
	return grpc.NewClient(
		"passthrough:///"+target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return dialTarget(ctx, target)
		}),
	)
}

// Manager 管理插件注册表（被动接受注册，不再 spawn 子进程）。
type Manager struct {
	registry       *RegistryServer
	workDir        string
	mu             sync.RWMutex
	autoRestart    bool
	restartCount   map[string]int
	registryCancel context.CancelFunc
}

// NewManager 创建插件管理器。
// workDir 是插件注册表 socket 的存放目录（默认 work/gta-registry.sock）。
func NewManager(workDir string) *Manager {
	registry := NewRegistryServer(10)
	return &Manager{
		registry:     registry,
		workDir:      workDir,
		restartCount: map[string]int{},
	}
}

// Start 启动注册表监听（调用方应在服务启动时调用）。
// 返回 registry socket 地址，供插件进程通过环境变量知晓。
func (m *Manager) Start(ctx context.Context) (string, error) {
	sockPath := fmt.Sprintf("%s/gta-registry.sock", m.workDir)
	_ = os.Remove(sockPath)
	srv, lis, err := m.registry.StartListen(sockPath)
	if err != nil {
		return "", err
	}
	m.registryCancel = m.registry.WatchOffline(30 * time.Second)
	go func() {
		if err := srv.Serve(lis); err != nil {
			slog.Error("registry server serve", "error", err)
		}
	}()
	slog.Info("registry server started", "socket", sockPath)
	return sockPath, nil
}

// Find 按 protocol_hint 查找第一个在线插件。
func (m *Manager) Find(protocolHint string) (pb.DecoderClient, *schema.Registry, bool) {
	return m.registry.Find(protocolHint)
}

// FindByName 按插件名精确查找已注册的解码插件。
func (m *Manager) FindByName(name string) (pb.DecoderClient, *schema.Registry, bool) {
	return m.registry.FindByName(name)
}

// Subscribe 订阅插件注册表状态变化事件（register/deregister/online/offline）。
// 返回只读事件通道与退订函数。
func (m *Manager) Subscribe() (<-chan PluginEvent, func()) {
	return m.registry.Subscribe()
}

// List 返回已注册插件摘要。
func (m *Manager) List() []PluginSummary {
	return m.registry.ListSummaries()
}

// Close 关闭注册表服务。
func (m *Manager) Close() error {
	if m.registryCancel != nil {
		m.registryCancel()
	}
	_ = m.registry.Close()
	slog.Info("manager closed")
	return nil
}

// SetAutoRestart 保留占位（Phase 3+ 再实现）。
func (m *Manager) SetAutoRestart(enable bool) {
	m.mu.Lock()
	m.autoRestart = enable
	m.mu.Unlock()
}

// RestartCount 保留占位（Phase 3+ 再实现）。
func (m *Manager) RestartCount(path string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.restartCount[path]
}

// Restart 保留占位（Phase 3+ 再实现）。
func (m *Manager) Restart(path string) error {
	return fmt.Errorf("manual restart not yet implemented in registration mode")
}
