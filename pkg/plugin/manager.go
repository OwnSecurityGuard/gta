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
	"gta/pkg/auth"
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
	// Owner 是注册方的属主标识（来自 gRPC auth 上下文，auth.OwnerFrom）。
	// 空串表示无主/匿名（本地单机用法），注册键退化为裸 manifest name，
	// 与改造前的行为完全一致。
	Owner string
	// Tunnel 表示该插件经 Connect 反向隧道接入：没有可回拨的 socket，
	// 存活与否跟随 Connect 流（不参与 CheckOffline 心跳超时判定）。
	Tunnel bool
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
	// Owner 是注册方属主（空串 = 匿名/本地）。T13 的 MCP listing 需要。
	Owner string
	// Tunnel 表示该插件经反向隧道接入（存活跟随 Connect 流）。
	Tunnel bool
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
	mu     sync.RWMutex
	plugins map[string]*RegisteredPlugin // key: instance_id
	byName map[string]string            // key: pluginKey(owner, name) → instance_id
	nextID atomic.Int64
	heartbeatSec int32

	// tunnelHub 处理 PluginRegistry.Connect 反向隧道流（见 tunnel.go）。
	// NewRegistryServer 时随注册表一起创建，Connect RPC 委托给它；
	// 隧道建流/断开钩子回调 bindTunnelClient / tunnelDisconnected 完成绑定与下线。
	tunnelHub *TunnelHub
	// tunnelPending 记录「Connect 已建立但尚未绑定到任何 tunnel 注册」的
	// 隧道客户端，按 owner 分桶、FIFO。见 Register 处的绑定时序说明。
	tunnelPending map[string][]pb.DecoderClient

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
	s := &RegistryServer{
		plugins:       map[string]*RegisteredPlugin{},
		byName:        map[string]string{},
		heartbeatSec:  heartbeatSec,
		listeners:     map[int64]chan PluginEvent{},
		tunnelPending: map[string][]pb.DecoderClient{},
	}
	// Connect 反向隧道：owner 从流上下文解析（gRPC auth 拦截器注入 Principal；
	// 未接入认证时 OwnerFrom 返回 ""，即匿名/本地语义）。
	s.tunnelHub = NewTunnelHub(
		WithTunnelOwnerResolver(auth.OwnerFrom),
		WithTunnelConnectHook(s.bindTunnelClient),
		WithTunnelDisconnectHook(s.tunnelDisconnected),
	)
	return s
}

// pluginKey 生成注册表 byName 的内部键。
// owner 为空（匿名/本地单机）时键就是裸 manifest name，行为与改造前一致；
// 非空 owner 的键为 "owner/name"，使不同属主的同名插件互不覆盖、各自路由。
func pluginKey(owner, name string) string {
	if owner == "" {
		return name
	}
	return owner + "/" + name
}

// resolvePluginKey 把调用方给的 name 解析成 byName 键。
// name 已含 "/" 时按完整键查找；否则先尝试 owner 作用域键，再退化裸 name
// （使有主调用方仍能看到匿名注册的本地/系统插件）。
// 单 owner（或匿名）语境下解析结果与改造前的 FindByName 完全一致。
func (s *RegistryServer) resolvePluginKey(owner, name string) string {
	if strings.Contains(name, "/") || owner == "" {
		return name
	}
	key := pluginKey(owner, name)
	if _, ok := s.byName[key]; ok {
		return key
	}
	return name
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
// 流程：解析 manifest → 校验 manifest → 版本协商 → （非隧道时）拨号插件 Decode
// socket 验证可达 → 分配 instance_id。
//
// 属主：owner 取自 gRPC auth 上下文（auth.OwnerFrom），注册键为 pluginKey(owner, name)；
// 同一 owner 的同名重复注册替换旧实例（崩溃重启场景），不同 owner 的同名插件共存。
//
// 隧道分支：req.Tunnel == true 时插件没有可回拨的 Decode socket，跳过拨号验证；
// 解码用 pb.DecoderClient 由 Connect 反向隧道提供（见 tunnel.go）。绑定时序：
// SDK 侧 RunRegisterLoop 是「先 Register 再 Connect」，但两者是独立 RPC，先后到达
// 顺序不保证，因此做双向 pending 匹配——
//   - Register 先到：实例以 Online=false（等待隧道）入表；
//   - Connect 先到：bindTunnelClient 把隧道 client 放进 tunnelPending[owner]；
//   - 两边齐了即绑定：取该 owner 的 FIFO pending client（同一插件进程一次
//     Connect 对应一次 tunnel 注册；同 owner 多插件时按到达顺序一一配对）。
//
// 绑定前实例不参与 Find/FindByName（Client 为 nil 不可用）；存活完全跟随
// Connect 流，不参与 CheckOffline 心跳超时（Tunnel=true 跳过）。
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

	owner := auth.OwnerFrom(ctx)
	tunnel := req.GetTunnel()

	// 4. 拨号到插件的 Decode socket 验证可达（隧道模式跳过：存活跟随 Connect 流）
	//    SocketPath 可能是 host:port（跨机器部署）、unix:/path 或 npipe:\\.\pipe\name。
	var conn *grpc.ClientConn
	var client pb.DecoderClient
	if !tunnel {
		conn, err = dialDecoder(ctx, req.SocketPath)
		if err != nil {
			return nil, fmt.Errorf("dial plugin socket: %w", err)
		}
		client = pb.NewDecoderClient(conn)
	}

	// 5. 分配 instance_id
	instanceID := fmt.Sprintf("%s-%d", m.Name, s.nextID.Add(1))

	rp := &RegisteredPlugin{
		InstanceID:    instanceID,
		SocketPath:    req.SocketPath,
		Manifest:      m,
		Client:        client,
		Conn:          conn,
		LastHeartbeat: time.Now(),
		Owner:         owner,
		Tunnel:        tunnel,
	}
	rp.Online.Store(!tunnel) // 隧道实例等 Connect 绑定后才算在线

	nameKey := pluginKey(owner, m.Name)
	bound := false
	s.mu.Lock()
	// 若同名插件已注册（同 owner 作用域内），关闭旧连接（崩溃后重启场景）
	if oldID, ok := s.byName[nameKey]; ok {
		if old, ok := s.plugins[oldID]; ok {
			if old.Conn != nil {
				_ = old.Conn.Close()
			}
			delete(s.plugins, oldID)
		}
	}
	s.plugins[instanceID] = rp
	s.byName[nameKey] = instanceID
	if tunnel {
		// 尝试立即绑定该 owner 的 pending 隧道（Connect 先到的场景）
		bound = s.bindPendingTunnelLocked(owner, rp)
	}
	s.mu.Unlock()

	slog.Info("plugin registered", "name", m.Name, "instance_id", instanceID, "protocol", m.Protocol,
		"owner", owner, "tunnel", tunnel, "tunnel_bound", bound)

	s.emit(PluginEvent{
		Type:       PluginEventRegister,
		InstanceID: instanceID,
		Name:       m.Name,
		Online:     rp.Online.Load(),
		Timestamp:  time.Now(),
	})
	if bound {
		s.emit(PluginEvent{
			Type:       PluginEventOnline,
			InstanceID: instanceID,
			Name:       m.Name,
			Online:     true,
			Timestamp:  time.Now(),
		})
	}

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
	if rp.Conn != nil {
		_ = rp.Conn.Close()
	}
	delete(s.plugins, instanceID)
	delete(s.byName, pluginKey(rp.Owner, name))
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
		// 隧道插件的存活跟随 Connect 流（tunnelDisconnected 处理下线），
		// 不参与心跳超时判定——隧道模式没有心跳，按超时判会误杀。
		if rp.Tunnel {
			continue
		}
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

// Find 根据 protocol hint 查找第一个匹配的解码插件（匿名/本地语境）。
// 匹配规则：manifest 的 protocol 字段或 hints 列表包含 protocolHint。
// 返回 DecoderClient、schema registry 和是否找到。
// 等价于 FindFor("", protocolHint)。
func (s *RegistryServer) Find(protocolHint string) (pb.DecoderClient, *schema.Registry, bool) {
	return s.FindFor("", protocolHint)
}

// FindFor 是 owner 作用域版的 Find：owner != "" 时优先（且限定）该 owner
// 注册的插件，同时兼容匿名注册的本地/系统插件；其他 owner 的插件不可见，
// 使多成员同名插件各自路由互不干扰。owner == "" 时与改造前的 Find 一致
// （只见匿名插件）。
// 隧道插件在 Connect 绑定前（Client 为 nil）不可见。
func (s *RegistryServer) FindFor(owner, protocolHint string) (pb.DecoderClient, *schema.Registry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, rp := range s.plugins {
		// 只返回在线插件
		if !rp.Online.Load() || rp.Client == nil {
			continue
		}
		// owner 作用域：非匿名调用方看得到自己的插件 + 匿名（系统）插件；
		// 匿名调用方只看匿名插件。其他 owner 的插件不可见。
		if owner != "" {
			if rp.Owner != "" && rp.Owner != owner {
				continue
			}
		} else if rp.Owner != "" {
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
// 等价于 FindByNameFor("", name)：键为裸 name，仅匹配匿名注册——与改造前行为一致。
func (s *RegistryServer) FindByName(name string) (pb.DecoderClient, *schema.Registry, bool) {
	return s.findByKey("", name)
}

// FindByNameFor 是 owner 作用域版的 FindByName：
//   - name 已含 "/"：按 "owner/name" 完整键精确查找；
//   - 否则先试 "<owner>/name"，查不到退化裸 "name"（兼容匿名注册的系统插件）；
//
// owner 为空时只按裸 name 查（匿名/本地语境，行为与改造前完全一致）。
// 解析顺序：先名精确、后退化——调用方（capture_task）的顺序由调用方保持。
func (s *RegistryServer) FindByNameFor(owner, name string) (pb.DecoderClient, *schema.Registry, bool) {
	return s.findByKey(owner, s.resolvePluginKey(owner, name))
}

// findByKey 按 byName 键查找在线插件；隧道插件绑定前（Client nil）不可见。
// callerOwner 用于隔离校验：即使按完整键（owner/name）寻址，也只能访问
// 自己的或匿名（系统）的插件。
func (s *RegistryServer) findByKey(callerOwner, key string) (pb.DecoderClient, *schema.Registry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byName[key]
	if !ok {
		return nil, nil, false
	}
	rp, ok := s.plugins[id]
	if !ok || !rp.Online.Load() || rp.Client == nil {
		return nil, nil, false
	}
	if rp.Owner != "" && rp.Owner != callerOwner {
		return nil, nil, false
	}
	return rp.Client, rp.SchemaRegistry(), true
}

func (s *RegistryServer) GetPluginManifest(name string) ([]byte, error) {
	return s.GetPluginManifestFor("", name)
}

// GetPluginManifestFor 是 owner 作用域版的 GetPluginManifest，
// 键解析规则同 FindByNameFor。
func (s *RegistryServer) GetPluginManifestFor(owner, name string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := s.resolvePluginKey(owner, name)
	ids, ok := s.byName[key]
	if !ok {
		return nil, fmt.Errorf("plugin %q not found", name)
	}
	rp, ok := s.plugins[ids]
	if !ok {
		return nil, fmt.Errorf("plugin %q instance not found", name)
	}
	if rp.Owner != "" && rp.Owner != owner {
		return nil, fmt.Errorf("plugin %q not found", name)
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
			Owner:         rp.Owner,
			Tunnel:        rp.Tunnel,
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
			Owner:         rp.Owner,
			Tunnel:        rp.Tunnel,
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
		if rp.Conn != nil {
			_ = rp.Conn.Close()
		}
		delete(s.plugins, id)
	}
	s.byName = map[string]string{}
	s.tunnelPending = map[string][]pb.DecoderClient{}
	return nil
}

// Connect 实现 PluginRegistry 的 Connect 双向流：委托给随注册表创建的
// TunnelHub（见 tunnel.go）。隧道建流/断开钩子回调 RegistryServer 的
// bindTunnelClient / tunnelDisconnected 完成「隧道 ↔ tunnel 注册」的绑定与下线。
func (s *RegistryServer) Connect(stream pb.PluginRegistry_ConnectServer) error {
	return s.tunnelHub.Connect(stream)
}

// bindPendingTunnelLocked 在持有 s.mu 时把 owner 的 FIFO pending 隧道 client
// 绑定到等待中的实例 rp；无可绑定 client 时返回 false。
func (s *RegistryServer) bindPendingTunnelLocked(owner string, rp *RegisteredPlugin) bool {
	pending := s.tunnelPending[owner]
	for i, c := range pending {
		if tunnelClientClosed(c) {
			continue
		}
		rp.Client = c
		rp.LastHeartbeat = time.Now()
		rp.Online.Store(true)
		s.tunnelPending[owner] = append(pending[:i], pending[i+1:]...)
		return true
	}
	return false
}

// bindTunnelClient 是 TunnelHub 的建流钩子：owner 从流上下文解析（auth.OwnerFrom）。
// 优先绑定该 owner 下「已 Register(tunnel=true) 但等待隧道」的实例（Register 先到）；
// 否则把 client 挂进 tunnelPending[owner] FIFO，等 Register 到达时认领（Connect 先到）。
// 绑定成功时实例置在线并推送 online 事件。
func (s *RegistryServer) bindTunnelClient(owner string, client pb.DecoderClient) {
	var bound *RegisteredPlugin
	var boundID string
	s.mu.Lock()
	// 先清理该 owner 下已死亡的 pending client（对应 Connect 已断但未绑定）
	pending := s.tunnelPending[owner][:0]
	for _, c := range s.tunnelPending[owner] {
		if !tunnelClientClosed(c) {
			pending = append(pending, c)
		}
	}
	s.tunnelPending[owner] = pending

	for id, rp := range s.plugins {
		if rp.Tunnel && rp.Owner == owner && rp.Client == nil {
			rp.Client = client
			rp.LastHeartbeat = time.Now()
			rp.Online.Store(true)
			bound = rp
			boundID = id
			break
		}
	}
	if bound == nil {
		s.tunnelPending[owner] = append(s.tunnelPending[owner], client)
	}
	s.mu.Unlock()

	if bound != nil {
		slog.Info("tunnel bound to registered plugin", "owner", owner, "instance_id", boundID,
			"name", bound.Manifest.Name)
		s.emit(PluginEvent{
			Type:       PluginEventOnline,
			InstanceID: boundID,
			Name:       bound.Manifest.Name,
			Online:     true,
			Timestamp:  time.Now(),
		})
	} else {
		slog.Info("tunnel connect pending registration", "owner", owner)
	}
}

// tunnelDisconnected 是 TunnelHub 的断开钩子：把该 owner 下绑定到此条
// （已死）Connect 会话的隧道插件标记离线并推送 offline 事件，同时清理
// pending 中已死的 client。插件重连后会重新 Register（替换旧实例）并绑定新隧道。
// 说明：TunnelHub 的断开钩子只带 owner 不带会话句柄，这里用「绑定 client 的
// 会话是否已关闭」来区分同 owner 多条隧道里真正断开的那条。
func (s *RegistryServer) tunnelDisconnected(owner string) {
	type offline struct {
		id, name string
	}
	var offs []offline
	s.mu.Lock()
	for id, rp := range s.plugins {
		if rp.Tunnel && rp.Owner == owner && rp.Client != nil && tunnelClientClosed(rp.Client) && rp.Online.Load() {
			rp.Online.Store(false)
			offs = append(offs, offline{id: id, name: rp.Manifest.Name})
		}
	}
	pending := s.tunnelPending[owner][:0]
	for _, c := range s.tunnelPending[owner] {
		if !tunnelClientClosed(c) {
			pending = append(pending, c)
		}
	}
	s.tunnelPending[owner] = pending
	s.mu.Unlock()

	for _, o := range offs {
		slog.Warn("tunnel disconnected, marking plugin offline", "owner", owner, "instance_id", o.id, "name", o.name)
		s.emit(PluginEvent{
			Type:       PluginEventOffline,
			InstanceID: o.id,
			Name:       o.name,
			Online:     false,
			Timestamp:  time.Now(),
		})
	}
}

// tunnelClientClosed 判断隧道 client 背后的 Connect 会话是否已关闭。
// manager.go 与 tunnel.go 同包，这里直接检查 tunnelSession 的 closed 通道。
func tunnelClientClosed(c pb.DecoderClient) bool {
	tc, ok := c.(*tunnelClient)
	if !ok {
		return false
	}
	select {
	case <-tc.sess.closed:
		return true
	default:
		return false
	}
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

// FindFor 是 owner 作用域版的 Find。
func (m *Manager) FindFor(owner, protocolHint string) (pb.DecoderClient, *schema.Registry, bool) {
	return m.registry.FindFor(owner, protocolHint)
}

// FindByNameFor 是 owner 作用域版的 FindByName。
func (m *Manager) FindByNameFor(owner, name string) (pb.DecoderClient, *schema.Registry, bool) {
	return m.registry.FindByNameFor(owner, name)
}

// GetPluginManifestFor 是 owner 作用域版的 GetPluginManifest。
func (m *Manager) GetPluginManifestFor(owner, name string) ([]byte, error) {
	return m.registry.GetPluginManifestFor(owner, name)
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
