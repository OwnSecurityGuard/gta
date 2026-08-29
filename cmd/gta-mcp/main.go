package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/OwnSecurityGuard/gta-plugin-sdk/contract"
	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"google.golang.org/grpc"
	"gopkg.in/yaml.v3"

	"gta/docs"
	"gta/pkg/auth"
	"gta/pkg/config"
	"gta/pkg/event"
	"gta/pkg/internalipc"
	pb "gta/pkg/internalipc/proto"
	"gta/pkg/logging"
	"gta/pkg/plugin"
	plugindevclient "gta/pkg/plugindev/client"
	plugindevserver "gta/pkg/plugindev/server"
	"gta/pkg/schema"
	"gta/pkg/store"

	_ "modernc.org/sqlite"
)

type sessionMetadata struct {
	// Owner 是会话归属者（pkg/auth 的 Principal.Owner）。
	// 空串表示匿名（本地单机用法），落到 current.json；非空落到 current.<owner>.json。
	Owner        string                 `json:"owner,omitempty"`
	SessionID    string                 `json:"session_id"`
	StartedAt    string                 `json:"started_at"`
	StoppedAt    string                 `json:"stopped_at,omitempty"`
	Status       string                 `json:"status"`
	Port         int                    `json:"port"`
	Plugin       string                 `json:"plugin"`
	Interface    string                 `json:"interface"`
	PCAPFile     string                 `json:"pcap_file,omitempty"`
	Source       string                 `json:"source,omitempty"` // nic | proxy
	ListenAddr   string                 `json:"listen_addr,omitempty"`
	FrameStyle   string                 `json:"frame_style,omitempty"`
	RawPackets   int64                  `json:"raw_packets,omitempty"`
	Events       int64                  `json:"events,omitempty"`
	Metrics      int64                  `json:"metrics,omitempty"`
	DecodeErrors int64                  `json:"decode_errors,omitempty"`
	DurationSec  float64                `json:"duration_sec,omitempty"`
	DBPath       string                 `json:"db_path"`
	Extra        map[string]interface{} `json:"extra,omitempty"`

	// ManifestSnapshot 是会话创建时的插件 manifest 快照（plugin.yaml 原文）。
	// 从 controlStore.SessionMeta.ManifestSnapshot 同步，用于 MCP get_session_status 输出。
	ManifestSnapshot string `json:"manifest_snapshot,omitempty"`
}

// ownerFilterFromCtx 从 ctx 提取 owner 可见性过滤器。
// 无身份（T12 之前的 HTTP 直连 / 本地用法）视为匿名（owner=""，非 admin）。
func ownerFilterFromCtx(ctx context.Context) store.SessionOwnerFilter {
	if p, ok := auth.PrincipalFrom(ctx); ok {
		return store.SessionOwnerFilter{Owner: p.Owner, AllOwners: p.IsAdmin}
	}
	return store.SessionOwnerFilter{}
}

// pluginEventJSON 是插件事件 SSE 推送的 JSON 负载，与 proto PluginEvent 对应。
type pluginEventJSON struct {
	Type       string `json:"type"` // register | deregister | online | offline
	InstanceID string `json:"instance_id"`
	Name       string `json:"name"`
	Online     bool   `json:"online"`
	Timestamp  int64  `json:"timestamp_unix"`
}

// captureReader 组合 EventReader + ProjectionReader，供 gta-mcp 查询事件和投影数据。
// gta-mcp 只读，通过此接口访问 capture.sqlite，便于未来替换存储后端。
type captureReader interface {
	store.EventReader
	store.ProjectionReader
}

type mcpCapture struct {
	mu          sync.Mutex
	iface       string
	pluginsDir  string
	workDir     string
	mcpServer   *server.MCPServer
	sessionMgr  *sessionManager
	runRegistry *RunRegistry

	// gRPC client 连接 gta-pipeline
	pipelineClient pb.CaptureControlClient
	grpcConn       *grpc.ClientConn

	// Developer Plane client (PluginDev gRPC). gta-mcp forwards scaffold/list/
	// build/activate to it and never touches the filesystem or subprocesses
	// directly. Nil means the Developer Plane is not configured for this
	// capture instance.
	pdClient plugindevclient.PluginDev
	pdConn   *grpc.ClientConn

	// ControlStore 读取会话元数据（db_path 等）
	controlStore *store.ControlStore

	// readerOpener 打开指定路径的 capture.sqlite 返回 captureReader。
	// 生产用 store.NewSQLiteStore；测试可注入共享实例避免 Windows 文件锁。
	readerOpener func(dbPath string) (captureReader, error)

	// enableRawDebug 控制原始包工具是否注册到 MCP surface。
	// 原始包能力仅限插件调试场景，默认不暴露。
	enableRawDebug bool

	// httpAddr 是本进程 HTTP 服务监听地址（如 ":8781"），
	// 用于构造手机 sing-box 客户端可导入的远程 profile 二维码 URI。
	httpAddr string

	// 事件总线：插件注册/注销/上下线事件经 WatchPlugins 流汇聚后广播给 SSE 订阅者。
	eventMu   sync.Mutex
	eventSubs map[chan pluginEventJSON]struct{}
}

type sessionManager struct {
	workDir string
	mu      sync.Mutex
}

func newSessionManager(workDir string) *sessionManager {
	return &sessionManager{workDir: workDir}
}

func (sm *sessionManager) sessionsDir() string {
	return filepath.Join(sm.workDir, "sessions")
}

func (sm *sessionManager) absDBPath(sessionID string) string {
	path := sm.dbPath(sessionID)
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// currentShardName 返回 owner 对应的 current 分片文件名。
// 匿名 owner（""）保持使用 current.json（本地单机回归底线）；
// 非 anon owner 落到 current.<owner>.json，多客户端共享 workDir 时互不覆盖。
// owner 中的不安全字符（文件系统/转义风险）统一替换为 '_'。
// 注意：替换会引入分片碰撞（如 "team/prod" 与 "team:prod" 都落 current.team_prod.json；
// Windows 文件系统大小写不敏感，"Alice" 与 "alice" 共用分片）。可接受：owner 来自
// 受信的 token 解析器（pkg/auth），而非任意用户输入；碰撞只影响 current 指针共享，
// 不影响 control.sqlite 里的会话归属过滤。
func currentShardName(owner string) string {
	if owner == "" {
		return "current.json"
	}
	var b strings.Builder
	for _, r := range owner {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return "current." + b.String() + ".json"
}

func (sm *sessionManager) currentPathFor(owner string) string {
	return filepath.Join(sm.workDir, currentShardName(owner))
}

func (sm *sessionManager) generateSessionID() string {
	return time.Now().Format("20060102_150405.000")
}

func (sm *sessionManager) sessionDir(sessionID string) string {
	return filepath.Join(sm.sessionsDir(), sessionID)
}

func (sm *sessionManager) dbPath(sessionID string) string {
	return filepath.Join(sm.sessionDir(sessionID), "capture.sqlite")
}

func (sm *sessionManager) createSession(metadata sessionMetadata) (string, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sessionDir := sm.sessionDir(metadata.SessionID)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return "", err
	}

	pluginsDir := filepath.Join(sessionDir, "plugins")
	if err := os.MkdirAll(pluginsDir, 0755); err != nil {
		return "", err
	}

	if err := sm.writeCurrent(metadata); err != nil {
		return "", err
	}

	return sessionDir, nil
}

func (sm *sessionManager) writeCurrent(metadata sessionMetadata) error {
	tmpPath := sm.currentPathFor(metadata.Owner) + ".tmp"
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, sm.currentPathFor(metadata.Owner))
}

// readCurrent 读取指定 owner 的 current 分片。
// owner 为空串读 current.json（匿名 / 本地单机用法）。
func (sm *sessionManager) readCurrent(owner string) (*sessionMetadata, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.readCurrentLocked(owner)
}

func (sm *sessionManager) readCurrentLocked(owner string) (*sessionMetadata, error) {
	data, err := os.ReadFile(sm.currentPathFor(owner))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var metadata sessionMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, err
	}
	return &metadata, nil
}

// listSessions 列出 workDir 下的会话元数据（filesystem 层），按 started_at 降序。
// f 控制 owner 可见性：metadata.json 无 owner 字段的历史会话视为匿名（""），
// 因此匿名过滤器对既有本地数据行为不变；AllOwners=true（admin）不过滤。
func (sm *sessionManager) listSessions(f store.SessionOwnerFilter) ([]sessionMetadata, error) {
	sessionsDir := sm.sessionsDir()
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []sessionMetadata{}, nil
		}
		return nil, err
	}

	var sessions []sessionMetadata
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sessionID := entry.Name()
		meta, err := sm.readSessionMetadata(sessionID, f.Owner)
		if err != nil {
			slog.Warn("read session metadata failed", "session_id", sessionID, "error", err)
			continue
		}
		if meta != nil {
			if f.Matches(store.SessionMeta{Owner: meta.Owner}) {
				sessions = append(sessions, *meta)
			}
		}
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartedAt > sessions[j].StartedAt
	})

	return sessions, nil
}

func (sm *sessionManager) sessionMetadataPath(sessionID string) string {
	return filepath.Join(sm.sessionDir(sessionID), "metadata.json")
}

func (sm *sessionManager) writeSessionMetadata(sessionID string, metadata sessionMetadata) error {
	path := sm.sessionMetadataPath(sessionID)
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func (sm *sessionManager) readSessionMetadata(sessionID, owner string) (*sessionMetadata, error) {
	current, err := sm.readCurrent(owner)
	if err != nil {
		return nil, err
	}
	if current != nil && current.SessionID == sessionID {
		return current, nil
	}

	path := sm.sessionMetadataPath(sessionID)
	data, err := os.ReadFile(path)
	if err == nil {
		var metadata sessionMetadata
		if err := json.Unmarshal(data, &metadata); err == nil {
			return &metadata, nil
		}
	}

	dbPath := sm.absDBPath(sessionID)
	info, err := os.Stat(dbPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	startedAt := info.ModTime().Format(time.RFC3339)
	return &sessionMetadata{
		SessionID: sessionID,
		StartedAt: startedAt,
		Status:    "stopped",
		DBPath:    dbPath,
	}, nil
}

func (sm *sessionManager) deleteSession(sessionID, owner string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	current, err := sm.readCurrentLocked(owner)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if current != nil && current.SessionID == sessionID {
		tmpPath := sm.currentPathFor(owner) + ".tmp"
		if err := os.WriteFile(tmpPath, []byte("{}"), 0644); err != nil {
			return err
		}
		if err := os.Rename(tmpPath, sm.currentPathFor(owner)); err != nil {
			return err
		}
	}

	sessionDir := sm.sessionDir(sessionID)
	return os.RemoveAll(sessionDir)
}

func newMCPCapture(iface, pluginsDir, workDir, pipelineAddr, httpAddr string, mcpServer *server.MCPServer, enableRawDebug bool) (*mcpCapture, error) {
	runRegistry, err := NewRunRegistry(workDir)
	if err != nil {
		slog.Warn("init run registry failed", "error", err)
		// 不阻断启动，run 窗口功能降级
	}

	// gRPC client 连接 gta-pipeline。
	// 默认拨号 :8088（TCP），可通过 -pipeline-addr 覆盖。
	var conn *grpc.ClientConn
	conn, err = internalipc.DialGRPCAddr(pipelineAddr)
	if err != nil {
		return nil, fmt.Errorf("dial pipeline: %w", err)
	}
	client := pb.NewCaptureControlClient(conn)

	// Developer Plane client. By default an embedded PluginDev gRPC server is
	// started (rooted at pluginsDir) so gta-mcp works standalone for local
	// development. In production, set GTA_PLUGINDEV_ADDR to point at the
	// standalone gta-plugin-dev binary for physical isolation.
	pdClient, pdConn, err := dialPluginDev(pluginsDir, os.Getenv("GTA_PLUGINDEV_ADDR"))
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("dial plugin dev: %w", err)
	}

	// ControlStore
	controlPath := filepath.Join(workDir, "control.sqlite")
	controlStore, err := store.NewControlStore(controlPath)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("open control store: %w", err)
	}

	m := &mcpCapture{
		iface:          iface,
		pluginsDir:     pluginsDir,
		workDir:        workDir,
		mcpServer:      mcpServer,
		sessionMgr:     newSessionManager(workDir),
		runRegistry:    runRegistry,
		pipelineClient: client,
		grpcConn:       conn,
		pdClient:       pdClient,
		pdConn:         pdConn,
		controlStore:   controlStore,
		readerOpener: func(path string) (captureReader, error) {
			return store.NewSQLiteStore(path, nil)
		},
		enableRawDebug: enableRawDebug,
		httpAddr:       httpAddr,
		eventSubs:      map[chan pluginEventJSON]struct{}{},
	}
	// 订阅 gta-pipeline 的插件事件流并广播给 SSE 客户端（断线自动重连）。
	m.startPluginEventWatcher()
	return m, nil
}

// dialPluginDev resolves the Developer Plane client. When addr is non-empty it
// dials the standalone gta-plugin-dev server at that address. Otherwise it
// starts an embedded PluginDev gRPC server rooted at pluginsDir and dials it
// over loopback, so local development needs no separate process. The returned
// conn must be closed by the caller (it is the client-side connection; the
// embedded server runs for the process lifetime).
func dialPluginDev(pluginsDir, addr string) (plugindevclient.PluginDev, *grpc.ClientConn, error) {
	if addr != "" {
		conn, err := internalipc.DialGRPCAddr(addr)
		if err != nil {
			return nil, nil, fmt.Errorf("dial plugindev %q: %w", addr, err)
		}
		return plugindevclient.NewGRPCClient(conn), conn, nil
	}

	lis, err := internalipc.ListenAddr("127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("listen embedded plugindev: %w", err)
	}
	srv := plugindevserver.New(pluginsDir)
	go func() {
		// Serve blocks; on listener close it returns. Errors are logged, not fatal.
		if serveErr := srv.Serve(lis); serveErr != nil {
			slog.Warn("embedded plugindev server stopped", "error", serveErr)
		}
	}()
	conn, err := internalipc.DialGRPCAddr(lis.Addr().String())
	if err != nil {
		return nil, nil, fmt.Errorf("dial embedded plugindev: %w", err)
	}
	return plugindevclient.NewGRPCClient(conn), conn, nil
}

func (m *mcpCapture) handleStartCapture(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	port, _ := req.RequireInt("port") // proxy 源下端口可选
	pluginName := req.GetString("plugin", "")
	pcapFile := req.GetString("pcap_file", "")
	if pcapFile != "" && !filepath.IsAbs(pcapFile) {
		pcapFile, _ = filepath.Abs(pcapFile)
	}

	// 抓包来源：nic（网卡，默认）| proxy（移动代理 gta-singbox-agent 推送）| agent（gta-agent 推流）
	// "mobile" 是 proxy 的历史别名。
	source := req.GetString("source", "nic")
	if source == "mobile" {
		source = "proxy"
	}
	agentSource := false
	switch source {
	case "", "nic", "proxy", "agent":
	default:
		return errorResult(fmt.Errorf("unsupported source %q (allowed: nic|proxy|agent)", source)), nil
	}
	if source == "agent" {
		agentSource = true
	}
	listenAddr := req.GetString("listen_addr", "")
	frameStyle := req.GetString("frame_style", "")
	prefixLen, _ := req.RequireInt("prefix_len")
	littleEndian := strings.EqualFold(req.GetString("little_endian", "false"), "true")
	slog.Info("start_capture requested", "port", port, "plugin", pluginName, "pcap_file", pcapFile, "source", source, "listen_addr", listenAddr, "frame_style", frameStyle)

	// 构造 gRPC request
	grpcReq := &pb.StartCaptureRequest{
		Plugin: pluginName,
		Port:   int32(port),
		Agent:  agentSource,
	}
	// 透传调用方身份：pipeline 记录会话归属（SessionMeta.Owner）并做 owner 作用域插件路由。
	if p, ok := auth.PrincipalFrom(ctx); ok {
		grpcReq.Owner = p.Owner
		grpcReq.AllOwners = p.IsAdmin
	}
	switch {
	case source == "proxy":
		if strings.TrimSpace(listenAddr) == "" {
			listenAddr = "127.0.0.1:9090"
		}
		grpcReq.Source = &pb.StartCaptureRequest_Mobile{
			Mobile: &pb.MobileSourceConfig{
				ListenAddr:   listenAddr,
				FrameStyle:   frameStyle,
				PrefixLen:    int32(prefixLen),
				LittleEndian: littleEndian,
			},
		}
	case pcapFile != "":
		grpcReq.Source = &pb.StartCaptureRequest_File{
			File: &pb.PcapFileConfig{Path: pcapFile},
		}
	case agentSource:
		// 纯 agent source：不设置基础 source，pipeline 侧仅订阅 agent hub
	default:
		// Live capture：使用配置的网卡，若 Device 为空则由 pipeline 自动探测所有网卡
		if port <= 0 {
			return errorResult(fmt.Errorf("port is required for source=nic")), nil
		}
		grpcReq.Source = &pb.StartCaptureRequest_Live{
			Live: &pb.PcapLiveConfig{Device: m.iface},
		}
	}

	resp, err := m.pipelineClient.StartCapture(ctx, grpcReq)
	if err != nil {
		return errorResult(fmt.Errorf("start capture: %w", err)), nil
	}

	// 记录当前 session（current.json + 每会话 metadata.json）
	// 写 metadata.json 使 getDBPath 即使 gta-mcp 与 gta-pipeline 的 workDir 不一致，
	// 也能通过 pipeline 返回的绝对 db_path 定位到正确的会话库。
	meta := sessionMetadata{
		Owner:      auth.OwnerFrom(ctx),
		SessionID:  resp.GetSessionId(),
		StartedAt:  time.Now().Format(time.RFC3339),
		Status:     "running",
		Port:       port,
		Plugin:     pluginName,
		Interface:  m.iface,
		PCAPFile:   pcapFile,
		Source:     source,
		ListenAddr: listenAddr,
		FrameStyle: frameStyle,
		DBPath:     resp.GetDbPath(),
	}
	if err := m.sessionMgr.writeSessionMetadata(resp.GetSessionId(), meta); err != nil {
		slog.Warn("write session metadata failed", "session_id", resp.GetSessionId(), "error", err)
	}
	m.sessionMgr.writeCurrent(meta)

	slog.Info("start_capture succeeded", "session_id", resp.GetSessionId(), "port", port, "plugin", pluginName, "source", source, "db_path", resp.GetDbPath())
	return successResult(map[string]any{
		"status":      "started",
		"session_id":  resp.GetSessionId(),
		"port":        port,
		"plugin":      pluginName,
		"source":      source,
		"db_path":     resp.GetDbPath(),
		"interface":   m.iface,
		"listen_addr": listenAddr,
	}), nil
}

func (m *mcpCapture) handleStopCapture(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	owner := auth.OwnerFrom(ctx)
	sessionID := req.GetString("session_id", "")
	slog.Info("stop_capture requested", "session_id", sessionID)

	if sessionID == "" {
		// 回退到当前 session（向后兼容）
		sess, err := m.sessionMgr.readCurrent(owner)
		if err != nil {
			return errorResult(fmt.Errorf("read current session: %w", err)), nil
		}
		if sess == nil || sess.Status == "stopped" {
			slog.Warn("stop_capture rejected: no active capture session")
			return errorResult(fmt.Errorf("no active capture session")), nil
		}
		sessionID = sess.SessionID
	} else if err := m.authorizeSession(ctx, sessionID); err != nil {
		// 显式指定 session_id 时校验归属（admin 全通过）
		return errorResult(err), nil
	}

	resp, err := m.pipelineClient.StopCapture(ctx, &pb.StopCaptureRequest{SessionId: sessionID})
	if err != nil {
		return errorResult(fmt.Errorf("stop capture: %w", err)), nil
	}

	// 更新 session 元数据（如果 sessionMgr 中有记录）
	if sess, err := m.sessionMgr.readCurrent(owner); err == nil && sess != nil && sess.SessionID == sessionID {
		sess.Status = "stopped"
		sess.StoppedAt = time.Now().Format(time.RFC3339)
		sess.RawPackets = resp.GetRawPackets()
		sess.Events = resp.GetEvents()
		sess.Metrics = resp.GetMetrics()
		sess.DecodeErrors = resp.GetDecodeErrors()
		sess.DurationSec = resp.GetDurationSec()
		m.sessionMgr.writeCurrent(*sess)
		m.sessionMgr.writeSessionMetadata(sess.SessionID, *sess)
	}

	slog.Info("stop_capture completed", "session_id", sessionID, "raw_packets", resp.GetRawPackets(), "events", resp.GetEvents(), "metrics", resp.GetMetrics(), "decode_errors", resp.GetDecodeErrors(), "duration_sec", resp.GetDurationSec())
	return successResult(map[string]any{
		"status":        "stopped",
		"session_id":    sessionID,
		"raw_packets":   resp.GetRawPackets(),
		"events":        resp.GetEvents(),
		"metrics":       resp.GetMetrics(),
		"decode_errors": resp.GetDecodeErrors(),
		"duration_sec":  resp.GetDurationSec(),
	}), nil
}

func (m *mcpCapture) handleGetSessionStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	owner := auth.OwnerFrom(ctx)
	sessionID := req.GetString("session_id", "")

	// 如果未指定 session_id，回退到当前 session
	if sessionID == "" {
		sess, err := m.sessionMgr.readCurrent(owner)
		if err != nil {
			slog.Warn("read current session failed", "error", err)
			return successResult(map[string]any{"state": "idle"}), nil
		}
		if sess == nil {
			return successResult(map[string]any{"state": "idle"}), nil
		}
		sessionID = sess.SessionID
	}

	slog.Debug("get_session_status requested", "session_id", sessionID)

	// 显式指定 session_id 时校验归属（admin 全通过）
	if req.GetString("session_id", "") != "" {
		if err := m.authorizeSession(ctx, sessionID); err != nil {
			return errorResult(err), nil
		}
	}

	// 通过 gRPC 查询实时状态
	if m.pipelineClient != nil {
		resp, err := m.pipelineClient.GetCaptureStatus(ctx, &pb.GetCaptureStatusRequest{SessionId: sessionID})
		if err == nil {
			return successResult(map[string]any{
				"session_id":    sessionID,
				"state":         resp.GetState(),
				"source_name":   resp.GetSourceName(),
				"packets_in":    resp.GetPacketsIn(),
				"raw_count":     resp.GetRawCount(),
				"event_count":   resp.GetEventCount(),
				"metric_count":  resp.GetMetricCount(),
				"decode_errors": resp.GetDecodeErrors(),
				"drops":         resp.GetDrops(),
				"errors":        resp.GetErrors(),
				"err":           resp.GetErr(),
			}), nil
		}
		// gRPC 查询失败，降级返回 sessionMgr 中的元数据
		slog.Warn("get_session_status gRPC failed, falling back to metadata", "error", err, "session_id", sessionID)
	}

	// 返回 sessionMgr 中的元数据
	sess, err := m.sessionMgr.readSessionMetadata(sessionID, owner)
	if err != nil || sess == nil {
		return successResult(map[string]any{"state": "closed", "session_id": sessionID}), nil
	}
	result := map[string]any{
		"session_id":    sess.SessionID,
		"state":         sess.Status,
		"port":          sess.Port,
		"plugin":        sess.Plugin,
		"interface":     sess.Interface,
		"pcap_file":     sess.PCAPFile,
		"raw_packets":   sess.RawPackets,
		"events":        sess.Events,
		"metrics":       sess.Metrics,
		"decode_errors": sess.DecodeErrors,
		"duration_sec":  sess.DurationSec,
		"db_path":       sess.DBPath,
	}
	if sess.ManifestSnapshot != "" {
		result["manifest_snapshot"] = sess.ManifestSnapshot
	}
	return successResult(result), nil
}

func (m *mcpCapture) handleListPlugins(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Discovery lives in the Developer Plane (pkg/plugindev). gta-mcp is a pure
	// forwarder — it never touches the filesystem here.
	if m.pdClient == nil {
		return errorResult(fmt.Errorf("plugin dev not available (Developer Plane not configured)")), nil
	}
	resp, err := m.pdClient.ListPlugins(ctx)
	if err != nil {
		slog.Error("list_plugins failed", "error", err)
		return errorResult(err), nil
	}
	plugins := make([]map[string]string, 0, len(resp.Plugins))
	for _, p := range resp.Plugins {
		plugins = append(plugins, map[string]string{
			"name":   p.Name,
			"binary": p.Binary,
			"dir":    p.Dir,
		})
	}
	slog.Info("list_plugins completed", "count", len(plugins))
	return successResult(map[string]any{"plugins": plugins, "count": len(plugins)}), nil
}

// exeExt returns the executable suffix for the current platform (".exe" on
// Windows, empty elsewhere).
func exeExt() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func (m *mcpCapture) handleGetPluginContract(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return successResult(map[string]any{"contract_yaml": string(contract.RawYAML())}), nil
}

func (m *mcpCapture) handleGetPluginDevGuide(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return successResult(map[string]any{"guide": string(docs.DevGuide())}), nil
}

func (m *mcpCapture) handleListRegisteredPlugins(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	// owner 作用域：非 admin 只见自己的 + 匿名（系统）插件；admin 见全部。
	// 身份经 RPC 透传给 pipeline（capturecontrol.Server 注入 ctx）。
	grpcReq := &pb.ListPluginsRequest{}
	if p, ok := auth.PrincipalFrom(ctx); ok {
		grpcReq.Owner = p.Owner
		grpcReq.AllOwners = p.IsAdmin
	}
	resp, err := m.pipelineClient.ListPlugins(ctx, grpcReq)
	if err != nil {
		slog.Error("list_registered_plugins failed", "error", err)
		return errorResult(fmt.Errorf("list registered plugins: %w", err)), nil
	}
	var plugins []map[string]any
	for _, p := range resp.GetPlugins() {
		plugins = append(plugins, map[string]any{
			"instance_id":    p.GetInstanceId(),
			"name":           p.GetName(),
			"protocol":       p.GetProtocol(),
			"type":           p.GetType(),
			"api_version":    p.GetApiVersion(),
			"socket_path":    p.GetSocketPath(),
			"online":         p.GetOnline(),
			"last_heartbeat": p.GetLastHeartbeatUnix(),
			"owner":          p.GetOwner(),
		})
	}
	return successResult(map[string]any{"plugins": plugins}), nil
}

func (m *mcpCapture) handleGetPluginManifest(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	name := req.GetString("name", "")
	// owner 作用域查找：只能查到自己 + 匿名（系统）插件的 manifest；admin 不限。
	grpcReq := &pb.GetPluginManifestRequest{Name: name}
	if p, ok := auth.PrincipalFrom(ctx); ok {
		grpcReq.Owner = p.Owner
		grpcReq.AllOwners = p.IsAdmin
	}
	resp, err := m.pipelineClient.GetPluginManifest(ctx, grpcReq)
	if err != nil {
		slog.Error("get_plugin_manifest failed", "error", err)
		return errorResult(fmt.Errorf("get plugin manifest: %w", err)), nil
	}
	return successResult(map[string]any{
		"name":     resp.GetName(),
		"manifest": string(resp.GetManifest()),
	}), nil
}

func (m *mcpCapture) handleDeregisterPlugin(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	instanceID := req.GetString("instance_id", "")
	name := req.GetString("name", "")
	if instanceID == "" && name == "" {
		return errorResult(fmt.Errorf("instance_id or name is required")), nil
	}
	resp, err := m.pipelineClient.DeregisterPlugin(ctx, &pb.DeregisterPluginRequest{
		InstanceId: instanceID,
		Name:       name,
	})
	if err != nil {
		slog.Error("deregister_plugin failed", "error", err)
		return errorResult(fmt.Errorf("deregister plugin: %w", err)), nil
	}
	return successResult(map[string]any{
		"ok":          resp.GetOk(),
		"instance_id": resp.GetInstanceId(),
		"name":        resp.GetName(),
	}), nil
}

func (m *mcpCapture) handleSetSessionPlugin(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := req.GetString("session_id", "")
	pluginName := req.GetString("plugin", "")
	if sessionID == "" || pluginName == "" {
		return errorResult(fmt.Errorf("session_id and plugin are required")), nil
	}
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	slog.Info("set_session_plugin requested", "session_id", sessionID, "plugin", pluginName)
	// 归属校验：只能给自家会话换插件（admin 全通过）
	if err := m.authorizeSession(ctx, sessionID); err != nil {
		return errorResult(err), nil
	}
	resp, err := m.pipelineClient.SetSessionPlugin(ctx, &pb.SetSessionPluginRequest{
		SessionId: sessionID,
		Plugin:    pluginName,
	})
	if err != nil {
		return errorResult(fmt.Errorf("set session plugin: %w", err)), nil
	}
	if !resp.GetOk() {
		return errorResult(fmt.Errorf("set session plugin failed: %s", resp.GetMessage())), nil
	}
	slog.Info("set_session_plugin succeeded", "session_id", sessionID, "plugin", pluginName)
	return successResult(map[string]any{
		"session_id": resp.GetSessionId(),
		"plugin":     resp.GetPlugin(),
	}), nil
}

func (m *mcpCapture) handleListInterfaces(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	resp, err := m.pipelineClient.ListInterfaces(ctx, &pb.ListInterfacesRequest{})
	if err != nil {
		slog.Error("list_interfaces failed", "error", err)
		return errorResult(fmt.Errorf("list interfaces: %w", err)), nil
	}
	var out []map[string]any
	for _, name := range resp.GetNames() {
		out = append(out, map[string]any{"name": name})
	}
	slog.Info("list_interfaces completed", "count", len(out))
	return successResult(map[string]any{"interfaces": out}), nil
}

func (m *mcpCapture) handleListLiveSessions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	resp, err := m.pipelineClient.ListCaptureSessions(ctx, &pb.ListCaptureSessionsRequest{})
	if err != nil {
		slog.Error("list_live_sessions failed", "error", err)
		return errorResult(fmt.Errorf("list capture sessions: %w", err)), nil
	}
	var sessions []map[string]any
	for _, s := range resp.GetSessions() {
		sessions = append(sessions, map[string]any{
			"session_id":      s.GetSessionId(),
			"state":           s.GetState(),
			"source_name":     s.GetSourceName(),
			"port":            s.GetPort(),
			"plugin":          s.GetPlugin(),
			"interface":       s.GetInterface(),
			"pcap_file":       s.GetPcapFile(),
			"started_at_unix": s.GetStartedAtUnix(),
		})
	}
	slog.Info("list_live_sessions completed", "count", len(sessions))
	return successResult(map[string]any{"count": len(sessions), "sessions": sessions}), nil
}

func (m *mcpCapture) handleAggregateQuery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	expression, err := req.RequireString("expression")
	if err != nil {
		return errorResult(err), nil
	}
	sessionID := req.GetString("session_id", "")
	dbPath, err := m.getDBPath(ctx, sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	slog.Info("aggregate_query requested", "expression", expression, "db_path", dbPath, "session_id", sessionID)
	if dbPath == "" {
		slog.Warn("aggregate_query rejected: no capture database available")
		return errorResult(fmt.Errorf("no capture database available; start a capture first")), nil
	}

	reader, err := m.openReader(ctx, sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	defer reader.Close()

	metrics, err := reader.QueryMetrics(ctx, store.MetricQuery{})
	if err != nil {
		return errorResult(fmt.Errorf("query metrics: %w", err)), nil
	}

	program, err := expr.Compile(expression, expr.Env(map[string]any{
		"name":   "",
		"window": "",
		"value":  0.0,
		"group":  map[string]string{},
	}))
	if err != nil {
		return errorResult(fmt.Errorf("compile expression: %w", err)), nil
	}

	var matched []map[string]any
	for _, metric := range metrics {
		env := map[string]any{
			"name":   metric.Name,
			"window": metric.Window.Format(time.RFC3339),
			"value":  metric.Value,
			"group":  metric.Group,
		}
		out, err := expr.Run(program, env)
		if err != nil {
			continue
		}
		if v, ok := out.(bool); ok && v {
			matched = append(matched, map[string]any{
				"name":   metric.Name,
				"window": metric.Window.Format(time.RFC3339),
				"group":  metric.Group,
				"value":  metric.Value,
			})
		}
	}
	slog.Info("aggregate_query completed", "expression", expression, "matched", len(matched))
	out := map[string]any{"count": len(matched), "metrics": matched}
	// Semantic Contract v1 §13：从 manifest 快照派生可聚合字段（contract.CanAggregate），
	// 供 Agent 编写 rules.yaml 的 value/group_by 时对齐声明。
	if fields := aggregatableContractFields(m.getManifestSnapshot(sessionID)); len(fields) > 0 {
		out["aggregatable_fields"] = fields
	}
	return successResult(out), nil
}

// aggregatableFieldView 是 aggregate_query 返回的 manifest 可聚合字段视图。
type aggregatableFieldView struct {
	Schema string `json:"schema"`
	Field  string `json:"field"`
	Alias  string `json:"alias,omitempty"`
}

// aggregatableContractFields 用 SDK 契约入口 contract.CanAggregate 列出 manifest 中
// 声明为 aggregatable 的字段。快照缺失或解析失败时返回 nil（契约信息为可选补充）。
func aggregatableContractFields(snapshot string) []aggregatableFieldView {
	if snapshot == "" {
		return nil
	}
	m, err := plugin.ParseManifest([]byte(snapshot))
	if err != nil {
		return nil
	}
	idx := contract.ManifestSchemaIndex(m)
	var out []aggregatableFieldView
	for wire, s := range idx {
		for name, f := range s.Fields {
			if !contract.CanAggregate(m, wire, name) {
				continue
			}
			v := aggregatableFieldView{Schema: wire, Field: name}
			if f != nil {
				v.Alias = f.Alias
			}
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Schema != out[j].Schema {
			return out[i].Schema < out[j].Schema
		}
		return out[i].Field < out[j].Field
	})
	return out
}

func (m *mcpCapture) handleGetCaptureSchema(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := req.GetString("session_id", "")
	dbPath, err := m.getDBPath(ctx, sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	slog.Info("get_capture_schema requested", "db_path", dbPath, "session_id", sessionID)
	if dbPath == "" {
		slog.Warn("get_capture_schema rejected: no capture database available")
		return errorResult(fmt.Errorf("no capture database available; start a capture first")), nil
	}

	sessionDir := filepath.Dir(dbPath)

	// 打开 reader 用于 loadDataFields 采样推断（manifest 缺失时的兜底路径）
	reader, err := m.openReader(ctx, sessionID)
	if err != nil {
		return errorResult(fmt.Errorf("open reader: %w", err)), nil
	}
	defer reader.Close()

	// 1. events 表的列（包含 data.* 展开后的字段）
	// 这些字段同时也是 list_decoded_data filter 表达式的 env 变量。
	queryFields := []map[string]any{
		{"name": "id", "type": "string", "description": "decoded event uuid"},
		{"name": "timestamp", "type": "string", "description": "event timestamp (RFC3339)"},
		{"name": "session_id", "type": "string", "description": "capture session id or tcp flow key"},
		{"name": "protocol", "type": "string", "description": "transport protocol, e.g. tcp"},
		{"name": "raw_len", "type": "number", "description": "original packet length"},
		{"name": "flow_id", "type": "number", "description": "direction-agnostic flow hash (5-tuple)"},
		{"name": "direction", "type": "string", "description": "client_to_server | server_to_client | unknown"},
		{"name": "msg_name", "type": "string", "description": "business message name"},
		{"name": "msg_id", "type": "number", "description": "per-flow auto-increment message id"},
		{"name": "is_push", "type": "number", "description": "1 if server push, 0 otherwise"},
		{"name": "src", "type": "string", "description": "source addr (ip:port)"},
		{"name": "dst", "type": "string", "description": "destination addr (ip:port)"},
		{"name": "tcp_flags", "type": "string", "description": "TCP control flags (FIN|RST|...), non-empty means tcp_close event"},
	}
	decodedColumns := make([]map[string]any, len(queryFields))
	copy(decodedColumns, queryFields)

	// data.* 字段三优先级：manifest 声明（Semantic Contract 真源）> schema.json > event_index 采样。
	var dataFields []dataField
	fieldSource := "event_index_sampling"
	if snapshot := m.getManifestSnapshot(sessionID); snapshot != "" {
		if mf, ok := manifestDataFields(snapshot); ok {
			dataFields = mf
			fieldSource = "manifest"
		}
	}
	if dataFields == nil {
		mf, err := loadDataFields(ctx, sessionDir, reader)
		if err != nil {
			slog.Warn("load data fields failed", "error", err)
		}
		if mf != nil {
			dataFields = mf
			if _, err := os.Stat(filepath.Join(sessionDir, "schema.json")); err == nil {
				fieldSource = "schema.json"
			}
		}
	}
	for _, f := range dataFields {
		decodedColumns = append(decodedColumns, map[string]any{
			"name":        "data." + f.name,
			"type":        f.typ,
			"description": "plugin decoded field",
		})
	}

	// 2. state_changes 投影表的列
	stateChangeColumns := []map[string]any{
		{"name": "id", "type": "string", "description": "state change uuid"},
		{"name": "session_id", "type": "string", "description": "capture session id"},
		{"name": "flow_id", "type": "string", "description": "flow id"},
		{"name": "timestamp", "type": "string", "description": "change timestamp (RFC3339)"},
		{"name": "subject_type", "type": "string", "description": "e.g. Building, Hero"},
		{"name": "subject_id", "type": "string", "description": "subject identifier"},
		{"name": "op", "type": "string", "description": "set | delete | merge"},
		{"name": "path", "type": "string", "description": "changed path/field"},
		{"name": "before", "type": "any", "description": "previous value (JSON)"},
		{"name": "after", "type": "any", "description": "new value (JSON)"},
		{"name": "version", "type": "number", "description": "optional version"},
	}

	// 3. aggregated_metrics 表的列
	metricColumns := []map[string]any{
		{"name": "name", "type": "string", "description": "metric output name, e.g. http_req_count"},
		{"name": "window", "type": "string", "description": "metric window start (RFC3339)"},
		{"name": "value", "type": "number", "description": "metric value"},
		{"name": "group", "type": "map[string]string", "description": "group tags, access by group['data.method']"},
	}

	// 3. 读取 rules.yaml，返回扁平化规则信息
	var rules []map[string]any
	rulesPath := filepath.Join(sessionDir, "rules.yaml")
	if rulesData, err := os.ReadFile(rulesPath); err == nil {
		var f config.File
		if err := yaml.Unmarshal(rulesData, &f); err == nil {
			for _, r := range f.Rules {
				rules = append(rules, map[string]any{
					"name":     r.Name,
					"filter":   r.Filter,
					"type":     r.Aggregate.Type,
					"window":   r.Aggregate.Window,
					"group_by": r.Aggregate.GroupBy,
					"value":    r.Aggregate.Value,
					"output":   r.Aggregate.Output,
				})
			}
		} else {
			slog.Warn("parse rules.yaml failed", "path", rulesPath, "error", err)
		}
	}

	// 4. 生成示例表达式
	examples := buildExamples(dataFields, rules)

	// 5. 契约声明视图（schema/state 层，从 manifest 快照派生）
	var manifestView map[string]any
	if snapshot := m.getManifestSnapshot(sessionID); snapshot != "" {
		manifestView = manifestDeclarationView(snapshot)
	}
	result := map[string]any{
		"sources": []map[string]any{
			{
				"name":        "events",
				"description": "解码后的事件表，list_decoded_data 的数据来源",
				"columns":     decodedColumns,
			},
			{
				"name":        "state_changes",
				"description": "状态变更投影表，list_state_changes 的数据来源",
				"columns":     stateChangeColumns,
			},
			{
				"name":        "aggregated_metrics",
				"description": "聚合指标表，aggregate_query 的数据来源",
				"columns":     metricColumns,
			},
		},
		"query_fields": queryFields,
		"rules":        rules,
		"examples":     examples,
		"field_source": fieldSource,
	}
	if manifestView != nil {
		result["manifest"] = manifestView
	}

	slog.Info("get_capture_schema completed", "session_dir", sessionDir, "field_source", fieldSource, "decoded_columns", len(decodedColumns), "rules", len(rules))
	return successResult(result), nil
}

type dataField struct {
	name string
	typ  string
}

// getManifestSnapshot 返回会话创建时保存的 plugin manifest 快照（plugin.yaml 原文）。
// 优先 ControlStore（持久层），无则返回空串。
func (m *mcpCapture) getManifestSnapshot(sessionID string) string {
	if m.controlStore != nil && sessionID != "" {
		if meta, err := m.controlStore.GetSession(context.Background(), sessionID); err == nil && meta != nil {
			return meta.ManifestSnapshot
		}
	}
	return ""
}

// manifestDataFields 从 manifest 快照派生 data.* 字段清单（Semantic Contract 真源）。
// 返回 ok=false 表示快照缺失或未声明任何 schema 字段，调用方回退到 schema.json / 采样。
func manifestDataFields(snapshot string) ([]dataField, bool) {
	m, err := plugin.ParseManifest([]byte(snapshot))
	if err != nil {
		return nil, false
	}
	idx := contract.ManifestSchemaIndex(m)
	if len(idx) == 0 {
		return nil, false
	}
	var fields []dataField
	for _, s := range idx {
		for name, f := range s.Fields {
			if f == nil {
				continue
			}
			fields = append(fields, dataField{name: name, typ: string(f.Type)})
		}
	}
	if len(fields) == 0 {
		return nil, false
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].name < fields[j].name })
	return fields, true
}

// manifestDeclarationView 把 manifest 的契约声明（schema/state）压成
// MCP 可返回的紧凑视图，让 Agent 无需连接插件即可了解会话的契约能力。
func manifestDeclarationView(snapshot string) map[string]any {
	m, err := plugin.ParseManifest([]byte(snapshot))
	if err != nil {
		return nil
	}
	// capabilities：转成字符串列表。
	var caps []string
	for cap, on := range m.Capabilities {
		if on {
			caps = append(caps, string(cap))
		}
	}
	sort.Strings(caps)
	out := map[string]any{
		"name":         m.Name,
		"protocol":     m.Protocol,
		"capabilities": caps,
	}

	// schema 层：字段 + semantic / 查询能力位。
	type fieldView struct {
		Name         string `json:"name"`
		Type         string `json:"type"`
		Semantic     string `json:"semantic,omitempty"`
		Queryable    bool   `json:"queryable,omitempty"`
		Aggregatable bool   `json:"aggregatable,omitempty"`
		Groupable    bool   `json:"groupable,omitempty"`
		Alias        string `json:"alias,omitempty"`
	}
	var schemas []map[string]any
	idx := contract.ManifestSchemaIndex(m)
	for wire, s := range idx {
		var fvs []fieldView
		for name, f := range s.Fields {
			if f == nil {
				continue
			}
			fvs = append(fvs, fieldView{
				Name: name, Type: string(f.Type), Semantic: string(f.Semantic),
				Queryable: f.Queryable, Aggregatable: f.Aggregatable,
				Groupable: f.Groupable, Alias: f.Alias,
			})
		}
		sort.Slice(fvs, func(i, j int) bool { return fvs[i].Name < fvs[j].Name })
		schemas = append(schemas, map[string]any{"id": wire, "fields": fvs})
	}
	if len(schemas) > 0 {
		out["schemas"] = schemas
	}

	// state 层：subject 类型 + id 字段 + 路径白名单。
	if len(m.States) > 0 {
		var states []map[string]any
		for _, s := range m.States {
			states = append(states, map[string]any{
				"subject_type": s.Type, "id_field": s.IDField, "paths": s.Paths,
			})
		}
		out["states"] = states
	}

	return out
}

func loadDataFields(ctx context.Context, sessionDir string, reader captureReader) ([]dataField, error) {
	// 优先读取 schema.json
	schemaPath := filepath.Join(sessionDir, "schema.json")
	if schemaData, err := os.ReadFile(schemaPath); err == nil {
		var s schema.Schema
		if err := json.Unmarshal(schemaData, &s); err == nil && s.Fields != nil {
			fields := make([]dataField, 0, len(s.Fields))
			for name, f := range s.Fields {
				fields = append(fields, dataField{name: name, typ: string(f.Type)})
			}
			sort.Slice(fields, func(i, j int) bool { return fields[i].name < fields[j].name })
			return fields, nil
		}
	}

	// 回退：从 event_index 采样推断 projection_json
	rows, err := reader.RawQuery(ctx, "SELECT projection_json FROM event_index LIMIT 50")
	if err != nil {
		return nil, err
	}

	seen := map[string]string{}
	for _, row := range rows {
		// projection_json 列可能是 []byte 或 string，统一处理
		var jsonStr string
		switch v := row["projection_json"].(type) {
		case []byte:
			jsonStr = string(v)
		case string:
			jsonStr = v
		default:
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
			continue
		}
		for k, v := range data {
			if _, exists := seen[k]; exists {
				continue
			}
			seen[k] = inferType(v)
		}
	}

	fields := make([]dataField, 0, len(seen))
	for name, typ := range seen {
		fields = append(fields, dataField{name: name, typ: typ})
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].name < fields[j].name })
	return fields, nil
}

func inferType(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}

func buildExamples(fields []dataField, rules []map[string]any) map[string][]string {
	examples := map[string][]string{
		"aggregate_query": {},
	}

	// 为每个规则生成一个示例
	for _, r := range rules {
		name, _ := r["name"].(string)
		if name == "" {
			continue
		}
		examples["aggregate_query"] = append(examples["aggregate_query"], fmt.Sprintf("name == %q", name))
	}

	// 如果有 method 字段，给出按方法过滤的示例
	hasMethod := false
	for _, f := range fields {
		if f.name == "method" {
			hasMethod = true
			break
		}
	}
	if hasMethod {
		examples["aggregate_query"] = append(examples["aggregate_query"], `group["data.method"] == "GET"`)
	}

	// list_decoded_data filter 示例 - 基于实际字段动态生成
	filterExamples := []string{
		`protocol == "tcp"`,
		`raw_len > 100`,
	}

	// 根据实际 data 字段生成示例
	for _, f := range fields {
		switch f.name {
		case "type":
			filterExamples = append(filterExamples, `data.type == "request"`)
		case "method":
			filterExamples = append(filterExamples, `data.method == "GET"`)
		case "path":
			filterExamples = append(filterExamples, `data.path contains "/api"`)
		case "body_len":
			filterExamples = append(filterExamples, `data.body_len > 50`)
		case "status":
			filterExamples = append(filterExamples, `data.status == "200"`)
		}
		// 如果是 number 类型，生成比较示例
		if f.typ == "number" && f.name != "body_len" {
			filterExamples = append(filterExamples, fmt.Sprintf(`data.%s > 5`, f.name))
		}
	}

	// 添加组合条件示例
	if hasMethod {
		filterExamples = append(filterExamples, `data.method == "POST" && data.body_len > 100`)
	}

	examples["list_decoded_data_filter"] = filterExamples

	return examples
}

func (m *mcpCapture) handleListDecodedData(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	limit := req.GetInt("limit", 100)
	offset := req.GetInt("offset", 0)
	sessionID := req.GetString("session_id", "")
	filterExpr := req.GetString("filter", "")
	dbPath, err := m.getDBPath(ctx, sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	slog.Info("list_decoded_data requested", "limit", limit, "offset", offset, "filter", filterExpr, "db_path", dbPath, "session_id", sessionID)
	if dbPath == "" {
		slog.Warn("list_decoded_data rejected: no capture database available")
		return errorResult(fmt.Errorf("no capture database available; start a capture first")), nil
	}

	// Compile filter expression if provided.
	var program *vm.Program
	if filterExpr != "" {
		program, err = expr.Compile(filterExpr, expr.Env(queryEnv()))
		if err != nil {
			return errorResult(fmt.Errorf("compile filter: %w", err)), nil
		}
	}

	reader, err := m.openReader(ctx, sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	defer reader.Close()

	// 诊断：确认解析到的 db 文件确实存在且有内容
	if fi, statErr := os.Stat(dbPath); statErr == nil {
		slog.Info("list_decoded_data: db file present", "db_path", dbPath, "size_bytes", fi.Size())
	} else {
		slog.Warn("list_decoded_data: db file missing", "db_path", dbPath, "error", statErr)
	}

	// Query all events (no LIMIT) for application-level filtering.
	eventRows, err := reader.QueryEventsDesc(ctx, sessionID, 0, 0)
	if err != nil {
		return errorResult(fmt.Errorf("query events: %w", err)), nil
	}
	slog.Info("list_decoded_data: queried events from db", "session_id", sessionID, "db_path", dbPath, "raw_event_count", len(eventRows))

	// 捕获上下文索引：基于全量事件计算连接序号/流序号（与分页、筛选无关）。
	// 仅代理抓包（conn_id 非空）的事件会获得 capture 字段，供前端展示 Capture Context。
	captureIdx := buildCaptureContext(eventRows)

	// 批量查询原始包长度：收集所有 RawPacketID，一次 SELECT id,LENGTH(payload) 构建 map。
	rawLenMap := make(map[string]int, len(eventRows))
	if dbReader, ok := reader.(interface{ DB() *sql.DB }); ok {
		var ids []string
		for _, ev := range eventRows {
			if ev.Context.RawPacketID != "" {
				ids = append(ids, ev.Context.RawPacketID)
			}
		}
		if len(ids) > 0 {
			placeholder := strings.Repeat(",?", len(ids)-1)
			rows, qErr := dbReader.DB().QueryContext(ctx,
				"SELECT id, COALESCE(LENGTH(payload),0) FROM raw_packets WHERE id IN (?"+placeholder+")",
				toAnySlice(ids)...,
			)
			if qErr != nil {
				slog.Debug("batch raw_len lookup failed (non-fatal)", "error", qErr)
			} else {
				for rows.Next() {
					var id string
					var ln int
					if err := rows.Scan(&id, &ln); err == nil {
						rawLenMap[id] = ln
					}
				}
				rows.Close()
			}
		}
	}

	matched := make([]map[string]any, 0)
	for _, ev := range eventRows {
		// 从 Event 构建 eventMap
		dataContent := ev.Payload.Value.ToAny()
		if dataContent == nil {
			dataContent = map[string]any{}
		}

		rawLen := 0
		if ev.Context.RawPacketID != "" {
			rawLen = rawLenMap[ev.Context.RawPacketID]
		}

		eventMap := map[string]any{
			"id":         string(ev.Identity.ID),
			"timestamp":  ev.Identity.Timestamp.Format(time.RFC3339),
			"session_id": ev.Identity.SessionID,
			"protocol":   string(ev.Identity.Type),
			"raw_len":    rawLen,
			"data":       dataContent,
		}
		if cc, ok := captureIdx[string(ev.Identity.ID)]; ok {
			eventMap["capture"] = cc
		}

		if program != nil {
			out, err := expr.Run(program, eventMap)
			if err != nil {
				slog.Debug("filter eval error", "event_id", ev.Identity.ID, "error", err)
				continue
			}
			if v, ok := out.(bool); !ok || !v {
				continue
			}
		}
		matched = append(matched, eventMap)
	}

	totalMatched := len(matched)

	// Apply offset/limit to filtered results.
	start := offset
	if start > totalMatched {
		start = totalMatched
	}
	end := start + limit
	if end > totalMatched {
		end = totalMatched
	}
	events := matched[start:end]

	slog.Info("list_decoded_data completed", "filter", filterExpr, "total_matched", totalMatched, "returned", len(events))
	return successResult(map[string]any{
		"total_matched": totalMatched,
		"count":         len(events),
		"events":        events,
	}), nil
}

// captureContextJSON 是单个事件的捕获上下文（Capture Context），
// 前端据此展示 "Captured By / Connection / Stream / Source"（代理抓包特有）。
type captureContextJSON struct {
	CapturedBy string `json:"captured_by"`
	ConnID     string `json:"conn_id"`
	ConnSeq    int    `json:"conn_seq"`   // 连接序号（1-based，按连接最新事件时间倒序）
	StreamID   string `json:"stream_id"`  // 流分组键（correlation_id 或事件 ID）
	StreamSeq  int    `json:"stream_seq"` // 连接内流序号（1-based，按流首事件时间正序）
	Source     string `json:"source"`
}

// captureDisplayName 把抓包来源映射为展示名（如 mobile → Mobile Proxy）。
func captureDisplayName(source string) string {
	switch source {
	case "mobile":
		return "Mobile Proxy"
	case "":
		return "Proxy"
	default:
		return source
	}
}

// buildCaptureContext 基于全量事件计算每个事件的连接/流序号。
//
// 连接序号：按连接最新事件时间倒序（与 Connections 页面一致），最新连接为 1。
// 流序号：连接内按流首事件时间正序（Stream View 语义），每流从 1 递增。
// 流分组键：correlation_id 非空的事件同组；未关联事件各自成流。
// 仅 conn_id 非空（代理抓包）的事件会被编入索引。
func buildCaptureContext(events []*event.Event) map[string]captureContextJSON {
	out := make(map[string]captureContextJSON, len(events))
	connSeqByID := make(map[string]int)
	streamSeqByConn := make(map[string]int)
	connEvents := make(map[string][]*event.Event)

	for _, ev := range events {
		connID := ev.Context.ConnID
		if connID == "" {
			continue
		}
		if _, ok := connSeqByID[connID]; !ok {
			connSeqByID[connID] = len(connSeqByID) + 1
			streamSeqByConn[connID] = 0
		}
		connEvents[connID] = append(connEvents[connID], ev)
	}

	for connID, evs := range connEvents {
		// evs 来自 QueryEventsDesc（时间倒序），反转为正序以符合流首事件时间正序。
		asc := make([]*event.Event, len(evs))
		for i, ev := range evs {
			asc[len(evs)-1-i] = ev
		}
		seenStream := make(map[string]bool)
		for _, ev := range asc {
			key := ev.Trace.CorrelationID
			if key == "" {
				key = string(ev.Identity.ID)
			}
			if !seenStream[key] {
				seenStream[key] = true
				streamSeqByConn[connID]++
			}
			out[string(ev.Identity.ID)] = captureContextJSON{
				CapturedBy: captureDisplayName(ev.Context.Source),
				ConnID:     connID,
				ConnSeq:    connSeqByID[connID],
				StreamID:   key,
				StreamSeq:  streamSeqByConn[connID],
				Source:     ev.Context.Source,
			}
		}
	}
	return out
}

// connectionReader 连接聚合查询能力（由 *store.SQLiteStore 实现）。
// 与 captureReader 分离：Connections 页面是代理抓包专有能力，非通用事件查询。
type connectionReader interface {
	QueryConnections(ctx context.Context, sessionID string, limit, offset int) ([]store.ConnectionSummary, error)
	QueryConnectionDetail(ctx context.Context, sessionID, connID string) (*store.ConnectionDetail, error)
	QueryConnectionStreams(ctx context.Context, sessionID, connID string, limit, offset int) ([]store.ConnectionStream, error)
	QueryConnectionFrames(ctx context.Context, connID string, limit, offset int) ([]store.ConnectionFrame, error)
}

// asConnectionReader 把 captureReader 断言为 connectionReader；后端不支持时返回错误。
func asConnectionReader(r captureReader) (connectionReader, error) {
	cr, ok := r.(connectionReader)
	if !ok {
		return nil, fmt.Errorf("store backend does not support connection queries")
	}
	return cr, nil
}

// handleListConnections 返回代理抓包的连接列表（按 conn_id 聚合，最新在前）。
func (m *mcpCapture) handleListConnections(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := req.GetString("session_id", "")
	limit := req.GetInt("limit", 100)
	offset := req.GetInt("offset", 0)

	dbPath, err := m.getDBPath(ctx, sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	slog.Info("list_connections requested", "session_id", sessionID, "limit", limit, "offset", offset, "db_path", dbPath)
	if dbPath == "" {
		return errorResult(fmt.Errorf("no capture database available; start a capture first")), nil
	}

	reader, err := m.openReader(ctx, sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	defer reader.Close()

	cr, err := asConnectionReader(reader)
	if err != nil {
		return errorResult(err), nil
	}

	conns, err := cr.QueryConnections(ctx, sessionID, limit, offset)
	if err != nil {
		return errorResult(fmt.Errorf("query connections: %w", err)), nil
	}

	slog.Info("list_connections completed", "session_id", sessionID, "count", len(conns))
	return successResult(map[string]any{
		"count":       len(conns),
		"connections": conns,
	}), nil
}

// handleGetConnectionDetail 返回单个连接的详情（头部信息 + 统计）。
func (m *mcpCapture) handleGetConnectionDetail(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := req.GetString("session_id", "")
	connID := req.GetString("conn_id", "")

	dbPath, err := m.getDBPath(ctx, sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	slog.Info("get_connection_detail requested", "session_id", sessionID, "conn_id", connID, "db_path", dbPath)
	if dbPath == "" {
		return errorResult(fmt.Errorf("no capture database available; start a capture first")), nil
	}
	if connID == "" {
		return errorResult(fmt.Errorf("conn_id is required")), nil
	}

	reader, err := m.openReader(ctx, sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	defer reader.Close()

	cr, err := asConnectionReader(reader)
	if err != nil {
		return errorResult(err), nil
	}

	detail, err := cr.QueryConnectionDetail(ctx, sessionID, connID)
	if err != nil {
		return errorResult(fmt.Errorf("query connection detail: %w", err)), nil
	}
	if detail == nil {
		return errorResult(fmt.Errorf("connection not found: %s", connID)), nil
	}

	slog.Info("get_connection_detail completed", "session_id", sessionID, "conn_id", connID)
	return successResult(map[string]any{
		"connection": detail,
	}), nil
}

// handleListConnectionStreams 返回连接内按关联键分组的流（Stream View）。
func (m *mcpCapture) handleListConnectionStreams(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := req.GetString("session_id", "")
	connID := req.GetString("conn_id", "")
	limit := req.GetInt("limit", 200)
	offset := req.GetInt("offset", 0)

	dbPath, err := m.getDBPath(ctx, sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	slog.Info("list_connection_streams requested", "session_id", sessionID, "conn_id", connID, "db_path", dbPath)
	if dbPath == "" {
		return errorResult(fmt.Errorf("no capture database available; start a capture first")), nil
	}
	if connID == "" {
		return errorResult(fmt.Errorf("conn_id is required")), nil
	}

	reader, err := m.openReader(ctx, sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	defer reader.Close()

	cr, err := asConnectionReader(reader)
	if err != nil {
		return errorResult(err), nil
	}

	streams, err := cr.QueryConnectionStreams(ctx, sessionID, connID, limit, offset)
	if err != nil {
		return errorResult(fmt.Errorf("query connection streams: %w", err)), nil
	}

	slog.Info("list_connection_streams completed", "session_id", sessionID, "conn_id", connID, "count", len(streams))
	return successResult(map[string]any{
		"count":   len(streams),
		"streams": streams,
	}), nil
}

// handleListConnectionFrames 返回连接内的原始帧（Frames / Raw 子页）。
func (m *mcpCapture) handleListConnectionFrames(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := req.GetString("session_id", "")
	connID := req.GetString("conn_id", "")
	limit := req.GetInt("limit", 100)
	offset := req.GetInt("offset", 0)

	dbPath, err := m.getDBPath(ctx, sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	slog.Info("list_connection_frames requested", "session_id", sessionID, "conn_id", connID, "db_path", dbPath)
	if dbPath == "" {
		return errorResult(fmt.Errorf("no capture database available; start a capture first")), nil
	}
	if connID == "" {
		return errorResult(fmt.Errorf("conn_id is required")), nil
	}

	reader, err := m.openReader(ctx, sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	defer reader.Close()

	cr, err := asConnectionReader(reader)
	if err != nil {
		return errorResult(err), nil
	}

	frames, err := cr.QueryConnectionFrames(ctx, connID, limit, offset)
	if err != nil {
		return errorResult(fmt.Errorf("query connection frames: %w", err)), nil
	}

	slog.Info("list_connection_frames completed", "session_id", sessionID, "conn_id", connID, "count", len(frames))
	return successResult(map[string]any{
		"count":  len(frames),
		"frames": frames,
	}), nil
}

// handleListRawPackets 查询指定 session 的 raw_packets 表。
// 支持 protocol/src/dst 过滤和分页，payload 以 base64 返回。
// 该工具属于受限调试能力，仅在 --enable-raw-debug 开启时注册。
func (m *mcpCapture) handleListRawPackets(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	limit := req.GetInt("limit", 100)
	offset := req.GetInt("offset", 0)
	sessionID := req.GetString("session_id", "")
	protocol := req.GetString("protocol", "")
	src := req.GetString("src", "")
	dst := req.GetString("dst", "")

	dbPath, err := m.getDBPath(ctx, sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	slog.Info("list_raw_packets requested", "limit", limit, "offset", offset, "protocol", protocol, "src", src, "dst", dst, "db_path", dbPath, "session_id", sessionID)
	if dbPath == "" {
		return errorResult(fmt.Errorf("no capture database available; start a capture first")), nil
	}

	reader, err := m.openReader(ctx, sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	defer reader.Close()

	rows, err := reader.QueryRawPackets(ctx, store.RawPacketQuery{
		Protocol: protocol,
		Src:      src,
		Dst:      dst,
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		return errorResult(fmt.Errorf("query raw packets: %w", err)), nil
	}

	packets := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		packets = append(packets, map[string]any{
			"id":          r.ID,
			"timestamp":   r.Timestamp.Format(time.RFC3339),
			"src":         r.Src,
			"dst":         r.Dst,
			"protocol":    r.Protocol,
			"payload":     base64.StdEncoding.EncodeToString(r.Payload),
			"payload_len": len(r.Payload),
			"link_type":   r.LinkType,
		})
	}

	slog.Info("list_raw_packets completed", "count", len(packets))
	return successResult(map[string]any{
		"count":   len(packets),
		"packets": packets,
	}), nil
}

// handleListStateChanges 查询指定 session 的 state_changes 投影表。
// 支持按 subject_type、subject_id、op、path、flow_id 过滤和分页。
func (m *mcpCapture) handleListStateChanges(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	limit := req.GetInt("limit", 100)
	offset := req.GetInt("offset", 0)
	sessionID := req.GetString("session_id", "")
	subjectType := req.GetString("subject_type", "")
	subjectID := req.GetString("subject_id", "")
	op := req.GetString("op", "")
	path := req.GetString("path", "")
	flowID := req.GetString("flow_id", "")

	dbPath, err := m.getDBPath(ctx, sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	slog.Info("list_state_changes requested", "limit", limit, "offset", offset, "subject_type", subjectType, "subject_id", subjectID, "op", op, "path", path, "flow_id", flowID, "db_path", dbPath, "session_id", sessionID)
	if dbPath == "" {
		return errorResult(fmt.Errorf("no capture database available; start a capture first")), nil
	}

	reader, err := m.openReader(ctx, sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	defer reader.Close()

	q := store.StateChangeQuery{
		SessionID:   sessionID,
		SubjectType: subjectType,
		SubjectID:   subjectID,
		Op:          op,
		Path:        path,
		FlowID:      flowID,
		Limit:       limit,
		Offset:      offset,
	}
	rows, err := reader.QueryStateChanges(ctx, q)
	if err != nil {
		return errorResult(fmt.Errorf("query state changes: %w", err)), nil
	}

	changes := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		changes = append(changes, map[string]any{
			"id":           r.ID,
			"event_id":     r.EventID,
			"session_id":   r.SessionID,
			"flow_id":      r.FlowID,
			"timestamp":    r.Timestamp.Format(time.RFC3339),
			"subject_type": r.SubjectType,
			"subject_id":   r.SubjectID,
			"op":           r.Op,
			"path":         r.Path,
			"before":       json.RawMessage(r.Before),
			"after":        json.RawMessage(r.After),
			"version":      r.Version,
			"metadata":     json.RawMessage(r.Metadata),
		})
	}

	slog.Info("list_state_changes completed", "count", len(changes))
	return successResult(map[string]any{
		"count":   len(changes),
		"changes": changes,
	}), nil
}

// handleQueryCaptureTable 提供对内部投影/审计表的只读出口。
// event_index（schema indexable_fields 投影）与 plugin_debug_access（采样审计留痕）
// 此前没有任何专用 MCP 工具暴露，AI 无法验证其是否生效；本工具以白名单方式安全开放
// SELECT（表名来自固定允许列表，杜绝注入），limit/offset 走参数化。
func (m *mcpCapture) handleQueryCaptureTable(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	allowed := map[string]bool{
		"event_index":         true,
		"plugin_debug_access": true,
		"raw_packets":         true,
		"events":              true,
		"state_changes":       true,
		"aggregated_metrics":  true,
	}
	sessionID := req.GetString("session_id", "")
	table := req.GetString("table", "")
	limit := req.GetInt("limit", 100)
	offset := req.GetInt("offset", 0)
	if sessionID == "" {
		return errorResult(fmt.Errorf("session_id is required")), nil
	}
	if !allowed[table] {
		return errorResult(fmt.Errorf("table %q is not in the allowlist; use one of: event_index, plugin_debug_access, raw_packets, events, state_changes, aggregated_metrics", table)), nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	reader, err := m.openReader(ctx, sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	defer reader.Close()

	query := fmt.Sprintf("SELECT * FROM %s WHERE session_id=? ORDER BY rowid LIMIT ? OFFSET ?", table)
	rows, err := reader.RawQuery(ctx, query, sessionID, limit, offset)
	if err != nil {
		return errorResult(fmt.Errorf("query %s: %w", table, err)), nil
	}
	return successResult(map[string]any{
		"table": table,
		"count": len(rows),
		"rows":  rows,
	}), nil
}

// handleDecodeRawPackets 用指定插件对离线会话的 raw_packets 批量解码，
// 结果写入该 session 的 events 表（随后可用 list_decoded_data 查询）。
// 仅允许解码已停止的 session；插件必须指定。
// 该工具属于受限调试能力，仅在 --enable-raw-debug 开启时注册。
func (m *mcpCapture) handleDecodeRawPackets(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := req.GetString("session_id", "")
	pluginName := req.GetString("plugin", "")
	protocol := req.GetString("protocol", "")
	src := req.GetString("src", "")
	dst := req.GetString("dst", "")
	limit := req.GetInt("limit", 0)
	clearExisting := req.GetBool("clear_existing", true)

	if sessionID == "" {
		return errorResult(fmt.Errorf("session_id is required")), nil
	}
	if pluginName == "" {
		return errorResult(fmt.Errorf("plugin is required")), nil
	}

	slog.Info("decode_raw_packets requested",
		"session_id", sessionID, "plugin", pluginName, "protocol", protocol,
		"src", src, "dst", dst, "limit", limit, "clear_existing", clearExisting)

	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}

	resp, err := m.pipelineClient.DecodeRawPackets(ctx, &pb.DecodeRawPacketsRequest{
		SessionId:     sessionID,
		Plugin:        pluginName,
		Protocol:      protocol,
		Src:           src,
		Dst:           dst,
		Limit:         int64(limit),
		ClearExisting: clearExisting,
	})
	if err != nil {
		return errorResult(fmt.Errorf("decode raw packets: %w", err)), nil
	}

	slog.Info("decode_raw_packets completed",
		"session_id", sessionID, "plugin", pluginName,
		"total_raw", resp.GetTotalRaw(), "decoded", resp.GetDecoded(), "decode_errors", resp.GetDecodeErrors())
	return successResult(map[string]any{
		"status":         "decoded",
		"session_id":     sessionID,
		"plugin":         pluginName,
		"total_raw":      resp.GetTotalRaw(),
		"decoded":        resp.GetDecoded(),
		"decode_errors":  resp.GetDecodeErrors(),
		"clear_existing": clearExisting,
	}), nil
}

// handleTestPlugin 用指定插件对离线会话的 raw_packets 解码并采样返回，用于验证插件解码质量。
// 原始包字节仅 gta-pipeline 进程内使用，绝不回传前端；结果不落库（隔离测试）。
// 本工具不暴露原始包，因此不依赖 --enable-raw-debug，常驻可用。
func (m *mcpCapture) handleTestPlugin(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := req.GetString("session_id", "")
	pluginName := req.GetString("plugin", "")
	protocol := req.GetString("protocol", "")
	src := req.GetString("src", "")
	dst := req.GetString("dst", "")
	limit := req.GetInt("limit", 0)
	sampleLimit := req.GetInt("sample_limit", 0)

	if sessionID == "" {
		return errorResult(fmt.Errorf("session_id is required")), nil
	}
	if pluginName == "" {
		return errorResult(fmt.Errorf("plugin is required")), nil
	}
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}

	slog.Info("test_plugin requested",
		"session_id", sessionID, "plugin", pluginName, "protocol", protocol,
		"src", src, "dst", dst, "limit", limit, "sample_limit", sampleLimit)

	resp, err := m.pipelineClient.TestPlugin(ctx, &pb.TestPluginRequest{
		SessionId:   sessionID,
		Plugin:      pluginName,
		Protocol:    protocol,
		Src:         src,
		Dst:         dst,
		Limit:       int64(limit),
		SampleLimit: int64(sampleLimit),
	})
	if err != nil {
		return errorResult(fmt.Errorf("test plugin: %w", err)), nil
	}

	slog.Info("test_plugin completed",
		"session_id", sessionID, "plugin", pluginName,
		"total_raw", resp.GetTotalRaw(), "decoded", resp.GetDecoded(), "decode_errors", resp.GetDecodeErrors())
	return successResult(map[string]any{
		"status":         "tested",
		"session_id":     sessionID,
		"plugin":         pluginName,
		"total_raw":      resp.GetTotalRaw(),
		"decoded":        resp.GetDecoded(),
		"decode_errors":  resp.GetDecodeErrors(),
		"type_histogram": resp.GetTypeHistogram(),
		"sample_events":  resp.GetSampleEvents(),
		"error_samples":  resp.GetErrorSamples(),
	}), nil
}

// queryEnv returns the expr environment for decoded event queries.
func queryEnv() map[string]any {
	return map[string]any{
		"id":         "",
		"timestamp":  "",
		"session_id": "",
		"protocol":   "",
		"raw_len":    0,
		"data":       map[string]any{},
	}
}

// openReader 打开指定 session 的 capture.sqlite 返回 captureReader 用于查询。
func (m *mcpCapture) openReader(ctx context.Context, sessionID string) (captureReader, error) {
	dbPath, err := m.getDBPath(ctx, sessionID)
	if err != nil || dbPath == "" {
		return nil, fmt.Errorf("no db path for session %s: %w", sessionID, err)
	}
	return m.readerOpener(dbPath)
}

// authorizeSession 校验调用方对指定会话的可见性（controlStore + metadata.json 双源）。
// 规则与 store.SessionOwnerFilter.Matches 一致：admin 全可见；否则仅 owner 匹配的会话。
// 防泄露规则：controlStore 中存在该会话但调用方不可见时直接拒绝，绝不回退到
// 文件系统路径（metadata.json 缺失时 readSessionMetadata 会按 os.Stat 合成
// Owner="" 的元数据，若回退将把他人会话的 db_path 泄露给匿名调用方）。
// 仅当 controlStore 完全查不到该会话（workDir 漂移等）时才走 metadata.json 兜底。
func (m *mcpCapture) authorizeSession(ctx context.Context, sessionID string) error {
	f := ownerFilterFromCtx(ctx)
	if f.AllOwners {
		return nil
	}
	// 1. controlStore：按 owner 过滤，命中即通过；
	//    会话存在但不可见 → 拒绝（不回退，见函数注释）。
	if m.controlStore != nil && sessionID != "" {
		meta, err := m.controlStore.GetSessionFor(ctx, sessionID, f)
		if err == nil && meta != nil {
			return nil
		}
		if _, errAll := m.controlStore.GetSession(ctx, sessionID); errAll == nil {
			return fmt.Errorf("session %s not found or not owned by you", sessionID)
		}
		// controlStore 无此会话记录 → 文件系统兜底
	}
	// 2. sessionMgr metadata.json（本地文件，需自行比对 Owner）
	if meta, err := m.sessionMgr.readSessionMetadata(sessionID, f.Owner); err == nil && meta != nil {
		if f.Matches(store.SessionMeta{Owner: meta.Owner}) {
			return nil
		}
		return fmt.Errorf("session %s not found or not owned by you", sessionID)
	}
	return nil
}

// getDBPath 获取指定 session 的 db_path。
// 优先从 ControlStore 查询（owner 过滤），回退到 sessionMgr（owner 校验）。
func (m *mcpCapture) getDBPath(ctx context.Context, sessionID string) (string, error) {
	if err := m.authorizeSession(ctx, sessionID); err != nil {
		slog.Warn("getDBPath: session access denied", "session_id", sessionID, "error", err)
		return "", err
	}
	owner := auth.OwnerFrom(ctx)
	// 1. 尝试 ControlStore
	if m.controlStore != nil && sessionID != "" {
		meta, err := m.controlStore.GetSessionFor(ctx, sessionID, ownerFilterFromCtx(ctx))
		if err == nil && meta != nil {
			slog.Info("getDBPath: resolved via controlStore", "session_id", sessionID, "db_path", meta.DBPath)
			return meta.DBPath, nil
		}
		slog.Debug("getDBPath: controlStore miss", "session_id", sessionID, "err", err)
	}
	// 2. 回退到 sessionMgr（metadata.json，含 pipeline 返回的绝对 db_path）
	if sessionID != "" {
		meta, err := m.sessionMgr.readSessionMetadata(sessionID, owner)
		if err == nil && meta != nil {
			slog.Info("getDBPath: resolved via sessionMgr metadata", "session_id", sessionID, "db_path", meta.DBPath)
			return meta.DBPath, nil
		}
	}
	// 3. 尝试当前 session
	current, err := m.sessionMgr.readCurrent(owner)
	if err == nil && current != nil {
		slog.Info("getDBPath: resolved via current session", "session_id", sessionID, "current_session_id", current.SessionID, "db_path", current.DBPath)
		return current.DBPath, nil
	}
	slog.Warn("getDBPath: no db path resolved", "session_id", sessionID)
	return "", nil
}

func (m *mcpCapture) handleListAllSessions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessions, err := m.sessionMgr.listSessions(ownerFilterFromCtx(ctx))
	if err != nil {
		slog.Error("list_all_sessions failed", "error", err)
		return errorResult(err), nil
	}

	// 可选 status 过滤（failed/success）。"failed" 映射到内部 status="error"。
	statusFilter := req.GetString("status", "")
	matchStatus := func(s string) bool {
		if statusFilter == "" {
			return true
		}
		if statusFilter == "failed" {
			return s == "error"
		}
		return s == statusFilter
	}

	// 用 pipeline 的 live sessions（gRPC）补全 running 会话的实时状态，
	// 避免 gta-mcp 与 gta-pipeline 的 workDir 漂移、或 metadata.json 缺失时，
	// running 会话被降级逻辑误标为 stopped（端口/插件/网卡也随之丢失）。
	liveByID := map[string]map[string]any{}
	if m.pipelineClient != nil {
		if resp, lerr := m.pipelineClient.ListCaptureSessions(ctx, &pb.ListCaptureSessionsRequest{}); lerr == nil {
			for _, s := range resp.GetSessions() {
				liveByID[s.GetSessionId()] = map[string]any{
					"state":  s.GetState(),
					"port":   s.GetPort(),
					"plugin": s.GetPlugin(),
					"iface":  s.GetInterface(),
				}
			}
		} else {
			slog.Warn("list_all_sessions: live sessions unavailable, falling back to local metadata", "error", lerr)
		}
	}

	// 返回所有会话（包括已停止的离线会话），running 的用 live 信息覆盖
	out := []map[string]any{}
	seen := map[string]bool{}
	for _, sess := range sessions {
		status := sess.Status
		port := sess.Port
		plugin := sess.Plugin
		iface := sess.Interface
		if live, ok := liveByID[sess.SessionID]; ok {
			if state, _ := live["state"].(string); state != "" {
				status = state
			}
			if p, _ := live["port"].(int32); p != 0 {
				port = int(p)
			}
			if p, _ := live["plugin"].(string); p != "" {
				plugin = p
			}
			if iv, _ := live["iface"].(string); iv != "" {
				iface = iv
			}
		}
		if !matchStatus(status) {
			continue
		}
		seen[sess.SessionID] = true
		out = append(out, map[string]any{
			"session_id":    sess.SessionID,
			"started_at":    sess.StartedAt,
			"stopped_at":    sess.StoppedAt,
			"status":        status,
			"port":          port,
			"plugin":        plugin,
			"interface":     iface,
			"pcap_file":     sess.PCAPFile,
			"source":        sess.Source,
			"listen_addr":   sess.ListenAddr,
			"frame_style":   sess.FrameStyle,
			"raw_packets":   sess.RawPackets,
			"events":        sess.Events,
			"metrics":       sess.Metrics,
			"decode_errors": sess.DecodeErrors,
			"duration_sec":  sess.DurationSec,
			"db_path":       sess.DBPath,
		})
	}

	// 补上仅存在于 pipeline live、但 sessionMgr 未枚举到的会话（workDir 漂移兜底）
	for id, live := range liveByID {
		if seen[id] {
			continue
		}
		status, _ := live["state"].(string)
		port, _ := live["port"].(int32)
		plugin, _ := live["plugin"].(string)
		iface, _ := live["iface"].(string)
		if !matchStatus(status) {
			continue
		}
		out = append(out, map[string]any{
			"session_id": id,
			"status":     status,
			"port":       int(port),
			"plugin":     plugin,
			"interface":  iface,
			"db_path":    m.sessionMgr.dbPath(id),
		})
	}

	slog.Info("list_all_sessions completed", "count", len(out), "local", len(sessions), "live", len(liveByID))
	return successResult(map[string]any{"count": len(out), "sessions": out}), nil
}

func (m *mcpCapture) handleDeleteSession(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID, err := req.RequireString("session_id")
	if err != nil {
		return errorResult(err), nil
	}

	// 检查 session 是否正在运行
	owner := auth.OwnerFrom(ctx)
	current, err := m.sessionMgr.readCurrent(owner)
	running := err == nil && current != nil && current.Status == "running" && current.SessionID == sessionID

	if running {
		slog.Warn("delete_session rejected: session is running", "session_id", sessionID)
		return errorResult(fmt.Errorf("cannot delete running session %s; stop it first", sessionID)), nil
	}

	// 归属校验：只能删除自己的会话（admin 全通过）
	if err := m.authorizeSession(ctx, sessionID); err != nil {
		return errorResult(err), nil
	}

	if err := m.sessionMgr.deleteSession(sessionID, owner); err != nil {
		slog.Error("delete_session failed", "session_id", sessionID, "error", err)
		return errorResult(err), nil
	}

	slog.Info("delete_session completed", "session_id", sessionID)
	return successResult(map[string]any{"status": "deleted", "session_id": sessionID}), nil
}

func successResult(v any) *mcp.CallToolResult {
	vMap, ok := v.(map[string]any)
	if !ok {
		// 尝试将结构体通过 JSON 序列化/反序列化转为 map[string]any
		b, err := json.Marshal(v)
		if err == nil {
			var m map[string]any
			if err := json.Unmarshal(b, &m); err == nil {
				vMap = m
			}
		}
		if vMap == nil {
			vMap = map[string]any{"result": v}
		}
	}
	vMap["ok"] = true
	b, _ := json.Marshal(vMap)
	return mcp.NewToolResultText(string(b))
}

func errorResult(err error) *mcp.CallToolResult {
	b, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
	return mcp.NewToolResultText(string(b))
}

// toAnySlice 将 []string 转为 []any，用于 SQL IN 查询的变参展开。
func toAnySlice(ss []string) []any {
	r := make([]any, len(ss))
	for i := range ss {
		r[i] = ss[i]
	}
	return r
}

// subscribeEvents 注册一个 SSE 订阅者，返回事件通道与退订函数。
func (m *mcpCapture) subscribeEvents() (<-chan pluginEventJSON, func()) {
	ch := make(chan pluginEventJSON, 16)
	m.eventMu.Lock()
	m.eventSubs[ch] = struct{}{}
	m.eventMu.Unlock()
	unsub := func() {
		m.eventMu.Lock()
		delete(m.eventSubs, ch)
		m.eventMu.Unlock()
		close(ch)
	}
	return ch, unsub
}

// broadcastPluginEvent 把事件推送给所有 SSE 订阅者。慢订阅者丢弃，避免阻塞广播。
func (m *mcpCapture) broadcastPluginEvent(ev pluginEventJSON) {
	m.eventMu.Lock()
	defer m.eventMu.Unlock()
	for ch := range m.eventSubs {
		select {
		case ch <- ev:
		default:
			// 慢订阅者丢弃，避免阻塞广播主路径
		}
	}
}

// startPluginEventWatcher 订阅 gta-pipeline 的 WatchPlugins gRPC 流，
// 逐条广播给 SSE 订阅者。断线后带指数退避自动重连，保证事件不丢。
func (m *mcpCapture) startPluginEventWatcher() {
	if m.pipelineClient == nil {
		return
	}
	go func() {
		backoff := time.Second
		for {
			stream, err := m.pipelineClient.WatchPlugins(context.Background(), &pb.WatchPluginsRequest{})
			if err != nil {
				slog.Warn("watch plugins stream failed, retrying", "error", err, "backoff", backoff.String())
				time.Sleep(backoff)
				if backoff < 15*time.Second {
					backoff *= 2
				}
				continue
			}
			backoff = time.Second
			for {
				ev, err := stream.Recv()
				if err != nil {
					slog.Warn("watch plugins recv failed, reconnecting", "error", err)
					break
				}
				m.broadcastPluginEvent(pluginEventJSON{
					Type:       ev.GetType(),
					InstanceID: ev.GetInstanceId(),
					Name:       ev.GetName(),
					Online:     ev.GetOnline(),
					Timestamp:  ev.GetTimestampUnix(),
				})
			}
			time.Sleep(time.Second)
		}
	}()
}

// handleEventsSSE 以 text/event-stream 向浏览器推送插件事件（SSE）。
// 事件名 "plugin"，data 为 pluginEventJSON 的 JSON；浏览器用 EventSource 订阅。
func (m *mcpCapture) handleEventsSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ch, unsub := m.subscribeEvents()
	defer unsub()

	// 15s 心跳注释，保持连接活跃并探测断线。
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			_, _ = fmt.Fprintf(w, ": keep-alive\n\n")
			flusher.Flush()
		case ev := <-ch:
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "event: plugin\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// resolvePluginsDir resolves the plugins directory to an absolute path.
// When the default relative value "plugins" is used, it is resolved relative
// to the running executable so that built binaries find plugins next to them.
// If that executable-relative path does not exist, it falls back to resolving
// relative to the current working directory to keep `go run` usable.
func resolvePluginsDir(input string) (string, error) {
	if filepath.IsAbs(input) {
		return filepath.Clean(input), nil
	}

	// For the default value, prefer a directory next to the executable.
	if input == "plugins" {
		exePath, err := os.Executable()
		if err == nil {
			exeDir := filepath.Dir(exePath)
			candidate := filepath.Join(exeDir, input)
			if _, err := os.Stat(candidate); err == nil {
				return filepath.Abs(candidate)
			}
		}
	}

	abs, err := filepath.Abs(input)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func main() {
	// 统一配置（T10）：-config 指向 gta.yaml（可选）。优先级 flag > 环境变量 GTA_* > 配置文件 > 默认值。
	cfgPath := flag.String("config", "", "统一配置文件 gta.yaml 路径（可选；优先级 flag > 环境变量 GTA_* > 配置文件 > 默认值）")
	addr := flag.String("addr", ":8781", "SSE server address（支持 :0 动态分配，实际地址回写 <workdir>/addr.mcp.json）")
	iface := flag.String("iface", "", "capture interface; empty means all available interfaces")
	pluginsDir := flag.String("plugins-dir", "plugins", "plugins directory")
	// 工作目录解析规则（T10）：显式 -work-dir > GTA_HOME > gta.yaml workdir >
	// CWD 既有数据探测（存在 control.sqlite/sessions/runs 时沿用 CWD）> ~/.gta。
	workDir := flag.String("work-dir", ".", "working directory for session databases（显式传参优先；否则 GTA_HOME > gta.yaml workdir > CWD 既有数据沿用 > ~/.gta）")
	pipelineAddr := flag.String("pipeline-addr", ":9888", "gta-pipeline gRPC 地址（默认 :9888）")
	debug := flag.Bool("debug", false, "enable debug logging")
	enableRawDebug := flag.Bool("enable-raw-debug", os.Getenv("GTA_MCP_ENABLE_RAW_DEBUG") == "1", "暴露原始包调试工具（list_raw_packets / decode_raw_packets），仅限插件开发调试；默认关闭")
	logFormat := flag.String("log-format", "json", "log format: json | text")
	logFile := flag.String("log-file", "", "log file path (default: <workdir>/logs/gta-mcp.log)")
	allowedOrigins := flag.String("allowed-origins", os.Getenv("GTA_MCP_ALLOWED_ORIGINS"), "CORS 允许的跨域 Origin（逗号分隔，如 http://localhost:5173,https://gta.example.com）；留空不返回 CORS 头（同源用法不受影响）")
	flag.Parse()

	// 加载统一配置并按优先级合并（flag 显式 > 环境变量 > 文件 > 默认值）。
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("load config", "path", *cfgPath, "error", err)
		os.Exit(1)
	}
	flagSet := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { flagSet[f.Name] = true })
	if !flagSet["addr"] && cfg.MCP.Addr != "" {
		*addr = cfg.MCP.Addr
	}
	// pipeline-addr 是要连接的 pipeline CaptureControl gRPC 地址，与 pipeline 的
	// control_addr 是同一个配置点（gta.yaml pipeline.control_addr / GTA_CONTROL_ADDR）。
	if !flagSet["pipeline-addr"] && cfg.Pipeline.ControlAddr != "" {
		*pipelineAddr = cfg.Pipeline.ControlAddr
	}
	// allowed-origins 的 flag 默认值本身就读 GTA_MCP_ALLOWED_ORIGINS（环境变量已兜底），
	// 这里只在 flag 未显式传入且环境变量为空时用配置文件值补齐。
	if !flagSet["allowed-origins"] && *allowedOrigins == "" && cfg.MCP.AllowedOrigins != "" {
		*allowedOrigins = cfg.MCP.AllowedOrigins
	}

	// 工作目录：显式 flag > GTA_HOME > gta.yaml workdir > CWD 既有数据沿用 > ~/.gta。
	absWorkDir, err := config.ResolveWorkDir(*workDir, flagSet["work-dir"], cfg.WorkDir)
	if err != nil {
		slog.Error("resolve workdir", "error", err)
		os.Exit(1)
	}

	// 统一日志初始化：文件落盘 + stderr 双写 + 按大小轮转
	logCfg := logging.DefaultConfig()
	if *debug {
		logCfg.Level = slog.LevelDebug
	}
	logCfg.Format = logging.Format(*logFormat)
	if *logFile == "" {
		*logFile = filepath.Join(absWorkDir, "logs", "gta-mcp.log")
	}
	logCfg.FilePath = *logFile
	logging.MustInit(logCfg)

	resolvedPluginsDir, err := resolvePluginsDir(*pluginsDir)
	if err != nil {
		slog.Error("resolve plugins directory failed", "error", err)
		os.Exit(1)
	}
	slog.Info("using plugins directory", "plugins_dir", resolvedPluginsDir)

	s := server.NewMCPServer("game-traffic-analysis", "1.0.0",
		server.WithToolCapabilities(true),
	)

	capture, err := newMCPCapture(*iface, resolvedPluginsDir, *workDir, *pipelineAddr, *addr, s, *enableRawDebug)
	if err != nil {
		slog.Error("init mcp capture", "error", err)
		os.Exit(1)
	}
	defer capture.grpcConn.Close()
	defer capture.controlStore.Close()
	if capture.pdConn != nil {
		defer capture.pdConn.Close()
	}

	s.AddTool(mcp.NewTool("start_capture",
		mcp.WithDescription("Start capturing traffic. Capture sources: source=nic (default) captures on a network interface filtered by port; source=proxy starts the mobile proxy gRPC listener (gta-singbox-agent connects and pushes connection-level frames); source=agent subscribes to the agent hub (a running gta-agent pushes raw frames for this session_id). Sources can be combined where supported (e.g. agent with pcap_file). Packets are always captured and stored; an optional plugin enables protocol decoding."),
		mcp.WithNumber("port", mcp.DefaultNumber(0), mcp.Description("Server port to capture or filter, e.g. 8080. Required for source=nic; ignored for source=proxy")),
		mcp.WithString("plugin", mcp.Description("Optional plugin name for protocol decoding, e.g. http. If omitted or no matching plugin is found, only raw packets are stored.")),
		mcp.WithString("pcap_file", mcp.Description("Optional pcap file to replay instead of live capture")),
		mcp.WithString("source", mcp.DefaultString("nic"), mcp.Description("Capture source: nic (network interface, default), proxy (mobile proxy via gta-singbox-agent) or agent (raw frames pushed by gta-agent via the agent hub)")),
		mcp.WithString("listen_addr", mcp.DefaultString("127.0.0.1:9090"), mcp.Description("For source=proxy: gRPC listen address that gta-singbox-agent connects to, e.g. 127.0.0.1:9090 or unix:///tmp/gta-mobile.sock")),
		mcp.WithString("frame_style", mcp.DefaultString("raw"), mcp.Description("For source=proxy: frame reassembly style, raw (each data chunk as one frame) or length_prefix (N-byte length header)")),
		mcp.WithNumber("prefix_len", mcp.DefaultNumber(4), mcp.Description("For source=proxy + frame_style=length_prefix: length prefix byte count 1|2|4")),
		mcp.WithString("little_endian", mcp.DefaultString("false"), mcp.Description("For source=proxy + frame_style=length_prefix: length prefix byte order, 'true' for little-endian, default big-endian")),
	), capture.handleStartCapture)

	s.AddTool(mcp.NewTool("stop_capture",
		mcp.WithDescription("Stop a running capture session and flush all data"),
		mcp.WithString("session_id", mcp.Description("Session ID to stop; defaults to current session")),
	), capture.handleStopCapture)

	s.AddTool(mcp.NewTool("get_session_status",
		mcp.WithDescription("Get capture status for a specific or current session"),
		mcp.WithString("session_id", mcp.Description("Session ID to query; defaults to current session")),
	), capture.handleGetSessionStatus)

	s.AddTool(mcp.NewTool("list_plugins",
		mcp.WithDescription("List available decoder plugins"),
	), capture.handleListPlugins)

	s.AddTool(mcp.NewTool("create_plugin",
		mcp.WithDescription("Scaffold a new decoder plugin project (plugin.yaml + main.go + go.mod) from templates. The skeleton registers itself via github.com/OwnSecurityGuard/gta-plugin-sdk. IMPORTANT: the generated decoder receives a COMPLETE link-layer frame (not L7) for pcap sources — when the pinned SDK ships the framing package, the scaffold uses framing.ExtractL7 + framing.Reassembler; otherwise it is explicitly marked framing-unavailable. Returns the actual output_dir (absolute), the exact sdk_version pinned, and whether framing is available. Ready to compile after adjusting the replace path (point it at the local gta-plugin-sdk repo or the published remote module)."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Plugin name, kebab-case, e.g. my-game-decoder")),
		mcp.WithString("protocol", mcp.Required(), mcp.Description("Protocol the plugin decodes, e.g. my_game")),
		mcp.WithString("protocol_version", mcp.Description("Optional protocol version, e.g. game/v3")),
		mcp.WithString("hints", mcp.Description("Optional match hints as JSON array of strings or comma-separated, e.g. [\"tcp\",\"port:7000\"]")),
		mcp.WithString("output_dir", mcp.Description("Strict target directory for the generated project; files are written directly there. Defaults to <plugins_dir>/<name> when omitted.")),
	), capture.handleCreatePlugin)

	s.AddTool(mcp.NewTool("build_plugin",
		mcp.WithDescription("Compile a scaffolded plugin project via the Developer Plane. Returns structured file:line:col diagnostics on failure so the AI can fix the exact location without reading SDK files. On success the artifact state advances scaffolded → compiled and any prior validated proof is invalidated."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Plugin name (kebab-case), e.g. my-game-decoder")),
		mcp.WithNumber("timeout_sec", mcp.Description("Optional build timeout in seconds (default 120)")),
	), capture.handleBuildPlugin)

	s.AddTool(mcp.NewTool("activate_plugin",
		mcp.WithDescription("Launch the local plugin binary and inject GTA_REGISTRY_ADDR so it registers with the runtime. The Developer Plane owns only the process it launches; deactivate_plugin tears it down. registry_addr resolves in order: explicit arg → GTA_REGISTRY_ADDR env → the pipeline's actual registry address (read via get_registry_addr), so you usually don't need to pass it. After launch, activation is NOT considered complete on a mere pid/activate-ok: it jointly verifies list_registered_plugins (registered), status_plugin.online (online), and get_plugin_manifest (manifest_present) — all three must hold before integrated=true."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Plugin name (kebab-case) to launch")),
		mcp.WithString("registry_addr", mcp.Description("Runtime registry address, e.g. :9091. Defaults to env GTA_REGISTRY_ADDR, then to the pipeline's address (via get_registry_addr)")),
	), capture.handleActivatePlugin)

	s.AddTool(mcp.NewTool("deactivate_plugin",
		mcp.WithDescription("Stop the plugin process the Developer Plane launched for name, and best-effort force-deregister it from the runtime registry. Safe to call when the plugin is already offline."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Plugin name (kebab-case) to stop")),
	), capture.handleDeactivatePlugin)

	s.AddTool(mcp.NewTool("status_plugin",
		mcp.WithDescription("Return the dual-state view of a plugin (design §2): artifact (unknown→scaffolded→compiled→validated, from disk) merged with runtime (offline→registered→active, from the registry), plus the last build/activate attempt for failure attribution and a suggested next_action. Use this as the per-iteration entry point."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Plugin name (kebab-case), e.g. my-game-decoder")),
	), capture.handleStatusPlugin)

	s.AddTool(mcp.NewTool("explain_plugin",
		mcp.WithDescription("Attribute the most recent build or activate failure of a plugin (design §2.3 / P3a). Reads the Developer Plane's last attempt and returns structured findings (category + optional SDK contract rule_id + why + fix), plus a next_action. The returned ref is what status_plugin's last_attempt.explain_ref points back to. On a failed build/activate the Developer Plane already auto-runs this, so status surfaces the ref immediately."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Plugin name (kebab-case), e.g. my-game-decoder")),
		mcp.WithString("action", mcp.Description("Optional: which attempt to explain (build | activate | deactivate). Omit to explain the latest attempt")),
		mcp.WithObject("verify", mcp.Description("Optional verify result from plugin.verify, shape {violations, quality, verdict}. When provided, decode-class attribution is derived from it; when omitted, the Developer Plane attributes the most recent recorded verify result.")),
	), capture.handleExplainPlugin)

	s.AddTool(mcp.NewTool("get_plugin_contract",
		mcp.WithDescription("Return the full contract.yaml spec for the GTA decoder plugin API. Use this as the single source of truth when writing or reviewing plugin code."),
	), capture.handleGetPluginContract)

	s.AddTool(mcp.NewTool("get_plugin_dev_guide",
		mcp.WithDescription("Return the full plugin development guide (markdown). Covers architecture, plugin.yaml schema, Decode RPC contract, lifecycle, framing, and best practices. KEY TAKEAWAY: for pcap sources DecodeRequest.payload is a COMPLETE link-layer frame (link header + IP + TCP/UDP + app bytes), NOT pre-stripped L7 — strip it per link_type with framing.ExtractL7 and reassemble TCP with framing.Reassembler first. Only ProxyPayload(1001)/TLSPlaintext(1002) are already L7. Read this BEFORE writing any decoder."),
	), capture.handleGetPluginDevGuide)

	s.AddTool(mcp.NewTool("get_proxy_server_config",
		mcp.WithDescription("Get the current mobile proxy capture server config and runtime status (agent process + always-on session), plus the machine LAN IP and a connect_addr (HTTP CONNECT proxy address for the phone). Use this to render the server config page and generate a scan-to-connect QR code. Proxy capture no longer requires manually starting a session — the pipeline keeps it running."),
	), capture.handleGetProxyServerConfig)

	s.AddTool(mcp.NewTool("update_proxy_server_config",
		mcp.WithDescription("Apply a new mobile proxy capture server config: persists to proxy.json, hot-restarts gta-singbox-agent and restarts the always-on proxy session so the change takes effect immediately. Empty fields keep the current value (listen_addr is the proxy port the phone connects to, e.g. 0.0.0.0:12000; server_addr is the mobile source gRPC addr, e.g. 127.0.0.1:9090; frame_style raw|length_prefix with prefix_len 1|2|4)."),
		mcp.WithString("listen_addr", mcp.Description("HTTP CONNECT proxy listen address, e.g. 0.0.0.0:12000 (empty = keep current)")),
		mcp.WithString("server_addr", mcp.Description("Mobile source gRPC address the agent pushes to, e.g. 127.0.0.1:9090 (empty = keep current)")),
		mcp.WithString("frame_style", mcp.Description("Frame reassembly style: raw | length_prefix (empty = keep current)")),
		mcp.WithNumber("prefix_len", mcp.Description("Length prefix byte count for length_prefix: 1|2|4")),
		mcp.WithString("little_endian", mcp.Description("Length prefix byte order: 'true' for little-endian, default big-endian")),
	), capture.handleUpdateProxyServerConfig)

	s.AddTool(mcp.NewTool("get_registry_addr",
		mcp.WithDescription("Return the registry address the pipeline is currently listening on (its -registry-addr, e.g. :9091). Plugins MUST connect here by setting GTA_REGISTRY_ADDR at startup; this tool removes the guesswork of reading pipeline startup logs. Use it to learn where a freshly launched plugin should register, or to confirm activate_plugin's resolved address."),
	), capture.handleGetRegistryAddr)

	s.AddTool(mcp.NewTool("get_capabilities",
		mcp.WithDescription("Return a self-describing catalog of all MCP tools grouped by workflow (capture / query / behavior / plugin-dev / plugin-verify / plugin-runtime / raw-debug) plus recommended call chains. Call this FIRST when unsure which tool to use or how tools relate; it replaces reading the README."),
	), capture.handleGetCapabilities)

	s.AddTool(mcp.NewTool("list_registered_plugins",
		mcp.WithDescription("List all plugins currently registered with the pipeline (active via gRPC PluginRegistry). Different from list_plugins which scans the plugins directory for binary files."),
	), capture.handleListRegisteredPlugins)

	s.AddTool(mcp.NewTool("get_plugin_manifest",
		mcp.WithDescription("Get the plugin.yaml manifest of a registered plugin by name. Returns the raw YAML bytes."),
		mcp.WithString("name", mcp.Description("Plugin name (kebab-case), e.g. http or my-game-decoder")),
	), capture.handleGetPluginManifest)

	s.AddTool(mcp.NewTool("deregister_plugin",
		mcp.WithDescription("Manually deregister a plugin from the pipeline. Use when a plugin crashed or needs to be forced-offline without restarting the pipeline."),
		mcp.WithString("instance_id", mcp.Description("Preferred: the instance_id returned by Register")),
		mcp.WithString("name", mcp.Description("Fallback: deregister by plugin name (matches first online instance)")),
	), capture.handleDeregisterPlugin)

	s.AddTool(mcp.NewTool("set_session_plugin",
		mcp.WithDescription("Hot-swap the decoder plugin bound to a RUNNING capture session. Takes effect immediately (the next decoded packet uses the new plugin) without stopping capture. The target plugin must already be registered with the pipeline. Fails if the session is not running or the plugin is unknown."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Target capture session ID")),
		mcp.WithString("plugin", mcp.Required(), mcp.Description("New decoder plugin name to bind (must be registered)")),
	), capture.handleSetSessionPlugin)

	s.AddTool(mcp.NewTool("list_interfaces",
		mcp.WithDescription("List available pcap capture interfaces"),
	), capture.handleListInterfaces)

	s.AddTool(mcp.NewTool("list_live_sessions",
		mcp.WithDescription("List currently active capture sessions from the pipeline"),
	), capture.handleListLiveSessions)

	// 默认 MCP surface：事件（events）、StateChange、聚合统计（aggregate stats）。
	s.AddTool(mcp.NewTool("list_decoded_data",
		mcp.WithDescription("List decoded protocol events from a capture session. This is the primary event query surface; results are stored in the events table and can be filtered with expr expressions."),
		mcp.WithNumber("limit", mcp.DefaultNumber(100), mcp.Description("Max rows to return")),
		mcp.WithNumber("offset", mcp.DefaultNumber(0), mcp.Description("Offset")),
		mcp.WithString("session_id", mcp.Description("Optional session ID to query; defaults to current session")),
		mcp.WithString("filter", mcp.Description("Optional expr expression to filter events, e.g. data.entity == \"buff\" && data.hp > 5. Available fields: id, timestamp, session_id, protocol, raw_len, data.*")),
	), capture.handleListDecodedData)

	// 代理抓包专有：连接/流/帧查询（Connections 页面数据源）。
	// 与 list_decoded_data 分离：这些工具按 conn_id 聚合，是移动代理抓包的核心入口。
	s.AddTool(mcp.NewTool("list_connections",
		mcp.WithDescription("List proxy capture connections aggregated by conn_id (newest first). Each row has client/server endpoints, protocol, source, start/end time, duration, event count and frame count. Requires a mobile proxy capture session."),
		mcp.WithNumber("limit", mcp.DefaultNumber(100), mcp.Description("Max rows to return")),
		mcp.WithNumber("offset", mcp.DefaultNumber(0), mcp.Description("Offset")),
		mcp.WithString("session_id", mcp.Description("Optional session ID to query; defaults to current session")),
	), capture.handleListConnections)

	s.AddTool(mcp.NewTool("get_connection_detail",
		mcp.WithDescription("Get one proxy capture connection's detail: client/server endpoints, protocol, source, app/device, time range, duration and stream/frame/event counts. Use the conn_id from list_connections."),
		mcp.WithString("session_id", mcp.Description("Optional session ID to query; defaults to current session")),
		mcp.WithString("conn_id", mcp.Required(), mcp.Description("Connection ID from list_connections")),
	), capture.handleGetConnectionDetail)

	s.AddTool(mcp.NewTool("list_connection_streams",
		mcp.WithDescription("List the streams within one connection (Stream View). Events are grouped by correlation_id; unpaired events (e.g. pushes) each form their own stream. Each stream contains its ordered decoded events with direction and msg_name."),
		mcp.WithString("session_id", mcp.Description("Optional session ID to query; defaults to current session")),
		mcp.WithString("conn_id", mcp.Required(), mcp.Description("Connection ID from list_connections")),
		mcp.WithNumber("limit", mcp.DefaultNumber(200), mcp.Description("Max streams to return")),
		mcp.WithNumber("offset", mcp.DefaultNumber(0), mcp.Description("Offset")),
	), capture.handleListConnectionStreams)

	s.AddTool(mcp.NewTool("list_connection_frames",
		mcp.WithDescription("List the raw reassembled frames within one connection (Frames / Raw view). Each frame has timestamp, direction, src/dst, protocol, link_type and base64 payload."),
		mcp.WithString("session_id", mcp.Description("Optional session ID to query; defaults to current session")),
		mcp.WithString("conn_id", mcp.Required(), mcp.Description("Connection ID from list_connections")),
		mcp.WithNumber("limit", mcp.DefaultNumber(100), mcp.Description("Max rows to return")),
		mcp.WithNumber("offset", mcp.DefaultNumber(0), mcp.Description("Offset")),
	), capture.handleListConnectionFrames)

	s.AddTool(mcp.NewTool("list_state_changes",
		mcp.WithDescription("List state change projections from a capture session, with optional filtering by subject_type, subject_id, op, path, or flow_id."),
		mcp.WithNumber("limit", mcp.DefaultNumber(100), mcp.Description("Max rows to return")),
		mcp.WithNumber("offset", mcp.DefaultNumber(0), mcp.Description("Offset")),
		mcp.WithString("session_id", mcp.Description("Optional session ID to query; defaults to current session")),
		mcp.WithString("subject_type", mcp.Description("Filter by subject type, e.g. Building")),
		mcp.WithString("subject_id", mcp.Description("Filter by subject ID, e.g. 1001")),
		mcp.WithString("op", mcp.Description("Filter by operation, e.g. set | delete")),
		mcp.WithString("path", mcp.Description("Filter by changed path/field, e.g. level")),
		mcp.WithString("flow_id", mcp.Description("Filter by flow ID")),
	), capture.handleListStateChanges)

	s.AddTool(mcp.NewTool("query_capture_table",
		mcp.WithDescription("Read-only escape hatch for internal projection/audit tables that have no dedicated tool. Whitelisted tables: event_index (schema indexable_fields projection), plugin_debug_access (audit trail of sampled bytes), raw_packets, events, state_changes, aggregated_metrics. The table name is constrained to the allowlist (no SQL injection possible); limit/offset are parameterized. Use this to verify event_index projections were built, or to inspect plugin_debug_access audit rows."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session ID to query")),
		mcp.WithString("table", mcp.Required(), mcp.Description("Whitelisted table: event_index | plugin_debug_access | raw_packets | events | state_changes | aggregated_metrics")),
		mcp.WithNumber("limit", mcp.DefaultNumber(100), mcp.Description("Max rows (clamped to 1000)")),
		mcp.WithNumber("offset", mcp.DefaultNumber(0), mcp.Description("Offset")),
	), capture.handleQueryCaptureTable)

	s.AddTool(mcp.NewTool("aggregate_query",
		mcp.WithDescription("Query aggregated metrics/statistics using an expr expression over {name, window, value, group}. Metrics are precomputed by rules.yaml; the aggregatable source fields are declared per-schema in the plugin manifest (see get_capture_schema manifest.schemas[].fields[].aggregatable)."),
		mcp.WithString("expression", mcp.Required(), mcp.Description("expr expression, e.g. name == 'http_req_count' && value > 0")),
		mcp.WithString("session_id", mcp.Description("Optional session ID to query; defaults to current session")),
	), capture.handleAggregateQuery)

	// 行为（behavior）与因果链（causation chain）工具。
	s.AddTool(mcp.NewTool("begin_capture_run",
		mcp.WithDescription("Mark the start of a user operation or behavior WITHOUT starting capture. It records a run window and returns run_id for later correlation (end_capture_run / get_run_status / trace_protocol_flow). plugin_name/device/filter/port are DESCRIPTIVE HINTS only and do NOT auto-start capture. To actually capture, call start_capture separately; if no capture is running this tool only returns a time_window_only uncertainty telling you to call start_capture first."),
		mcp.WithString("feature_name", mcp.Required(), mcp.Description("Name of the feature/behavior being tested, e.g. 'upgrade_building'")),
		mcp.WithString("project_path", mcp.Required(), mcp.Description("Path to the load-test project where code will be generated")),
		mcp.WithString("plugin_name", mcp.Description("Optional descriptive hint recorded for the run window. NOT used to auto-start capture; call start_capture separately.")),
		mcp.WithString("device", mcp.Description("Optional device identifier")),
		mcp.WithString("filter", mcp.Description("Optional capture filter")),
		mcp.WithNumber("port", mcp.Description("Optional descriptive hint recorded for the run window. NOT used to auto-start capture.")),
	), capture.handleBeginCaptureRun)

	s.AddTool(mcp.NewTool("end_capture_run",
		mcp.WithDescription("Close the current behavior window. Returns summary statistics for the run. Idempotent: repeated calls return the same summary."),
		mcp.WithString("run_id", mcp.Required(), mcp.Description("Run ID from begin_capture_run")),
		mcp.WithString("time_to", mcp.Description("Optional upper bound of the time window (RFC3339Nano). Defaults to now. Mainly for testing.")),
	), capture.handleEndCaptureRun)

	s.AddTool(mcp.NewTool("get_run_status",
		mcp.WithDescription("Quickly check whether a behavior run has useful data. Returns flow/message counts for fail-fast decisions."),
		mcp.WithString("run_id", mcp.Required(), mcp.Description("Run ID to check")),
	), capture.handleGetRunStatus)

	s.AddTool(mcp.NewTool("trace_protocol_flow",
		mcp.WithDescription("Build the chronological execution trace (causation chain) for one behavior. Stitches request/response/push/state_diff across the run window. Returns steps or file_path for large results."),
		mcp.WithString("run_id", mcp.Required(), mcp.Description("Run ID from begin_capture_run")),
		mcp.WithString("flow_id", mcp.Required(), mcp.Description("Flow ID to trace")),
		mcp.WithString("feature_name", mcp.Required(), mcp.Description("Feature/behavior name for context")),
		mcp.WithObject("noise_filter", mcp.Description("Noise filtering options"), mcp.Properties(map[string]any{
			"drop_names":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"drop_heartbeats": map[string]any{"type": "boolean", "default": true},
		})),
		mcp.WithObject("entity_diff", mcp.Description("Entity diff options"), mcp.Properties(map[string]any{
			"enabled":   map[string]any{"type": "boolean", "default": true},
			"window_ms": map[string]any{"type": "number", "default": 500},
		})),
	), capture.handleTraceProtocolFlow)

	// 会话级时序树（Phase 1 MVP）：整 session 的 request/response 因果树。
	s.AddTool(mcp.NewTool("get_session_timeline",
		mcp.WithDescription("Build the full request/response timeline tree for one capture session from TraceContext (causation_id = parent, correlation_id = conversation group). This is the 'capture once, see the whole flow' MVP view. Returns a nested tree of roots plus per-conversation aggregation."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session ID to build timeline for")),
		mcp.WithNumber("limit", mcp.DefaultNumber(500), mcp.Description("Max events to load into the tree (clamped to 5000)")),
		mcp.WithNumber("offset", mcp.DefaultNumber(0), mcp.Description("Offset into the session's events")),
	), capture.handleGetSessionTimeline)

	s.AddTool(mcp.NewTool("get_capture_schema",
		mcp.WithDescription("Describe available fields for decoded events, state_changes projections, aggregation metrics and current rules."),
		mcp.WithString("session_id", mcp.Description("Optional session ID to query; defaults to current session")),
	), capture.handleGetCaptureSchema)

	// 受限调试能力：原始包工具仅在 --enable-raw-debug 或 GTA_MCP_ENABLE_RAW_DEBUG=1 时注册。
	if capture.enableRawDebug {
		s.AddTool(mcp.NewTool("list_raw_packets",
			mcp.WithDescription("[PLUGIN DEBUG ONLY] List raw packets from a capture session, with optional protocol/src/dst filtering. Payload is base64-encoded. Requires --enable-raw-debug."),
			mcp.WithNumber("limit", mcp.DefaultNumber(100), mcp.Description("Max rows to return")),
			mcp.WithNumber("offset", mcp.DefaultNumber(0), mcp.Description("Offset")),
			mcp.WithString("session_id", mcp.Description("Optional session ID to query; defaults to current session")),
			mcp.WithString("protocol", mcp.Description("Filter by protocol, e.g. tcp")),
			mcp.WithString("src", mcp.Description("Filter by source address (substring match)")),
			mcp.WithString("dst", mcp.Description("Filter by destination address (substring match)")),
		), capture.handleListRawPackets)

		s.AddTool(mcp.NewTool("decode_raw_packets",
			mcp.WithDescription("[PLUGIN DEBUG ONLY] Decode raw packets of an offline session using a specified plugin. Results are written into the session's events table; query them afterwards via list_decoded_data. Only stopped sessions can be decoded. Requires --enable-raw-debug."),
			mcp.WithString("session_id", mcp.Required(), mcp.Description("Session ID to decode (must be stopped)")),
			mcp.WithString("plugin", mcp.Required(), mcp.Description("Plugin name for decoding, e.g. http or tcp")),
			mcp.WithString("protocol", mcp.Description("Optional: only decode packets with this protocol, e.g. tcp")),
			mcp.WithString("src", mcp.Description("Optional: only decode packets whose source matches (substring)")),
			mcp.WithString("dst", mcp.Description("Optional: only decode packets whose destination matches (substring)")),
			mcp.WithNumber("limit", mcp.Description("Optional: max number of raw packets to decode, 0 means all")),
			mcp.WithBoolean("clear_existing", mcp.Description("Optional: clear events, state_changes and event_index before writing new results, default true")),
		), capture.handleDecodeRawPackets)
	}

	// test_plugin：隐私安全的插件测试通道。原始包仅在 gta-pipeline 进程内解码，不回传前端；
	// 结果不落库。因此不需要 --enable-raw-debug，常驻可用。
	s.AddTool(mcp.NewTool("test_plugin",
		mcp.WithDescription("Test a plugin by decoding an offline session's raw packets in-process and returning sampled decoded events. Raw packet bytes are NEVER returned to the client (used only server-side for decoding); results are NOT persisted. Safe to use without --enable-raw-debug. Only stopped sessions can be tested."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session ID whose raw packets to test against (must be stopped)")),
		mcp.WithString("plugin", mcp.Required(), mcp.Description("Plugin name to test, e.g. http or tcp")),
		mcp.WithString("protocol", mcp.Description("Optional: only test packets with this protocol, e.g. tcp")),
		mcp.WithString("src", mcp.Description("Optional: only test packets whose source matches (substring)")),
		mcp.WithString("dst", mcp.Description("Optional: only test packets whose destination matches (substring)")),
		mcp.WithNumber("limit", mcp.Description("Optional: max number of raw packets to test, 0 means all")),
		mcp.WithNumber("sample_limit", mcp.Description("Optional: max number of decoded events to return as samples, default 50")),
	), capture.handleTestPlugin)

	// verify_plugin：契约+质量校验，产出 verdict 并把 artifact.state 升到 validated。
	// 纯转发到 Runtime Plane（gta-pipeline）；MCP 零归因逻辑。
	s.AddTool(mcp.NewTool("verify_plugin",
		mcp.WithDescription("Verify a plugin by decoding an offline session's raw packets and checking contract violations (SDK checker, each tagged with a contract.yaml rule_id) plus gta-side quality stats (unknown ratio, entropy, correlation). Returns a verdict: pass | warn | fail, and on a non-fail verdict promotes the plugin's artifact.state to validated (with a proof). Pure forwarder to the Runtime Plane; MCP owns no attribution logic."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Stopped session whose raw packets to verify against")),
		mcp.WithString("plugin", mcp.Required(), mcp.Description("Plugin name to verify, e.g. http or tcp")),
		mcp.WithString("protocol", mcp.Description("Optional: only verify packets with this protocol, e.g. tcp")),
		mcp.WithString("src", mcp.Description("Optional: only verify packets whose source matches (substring)")),
		mcp.WithString("dst", mcp.Description("Optional: only verify packets whose destination matches (substring)")),
		mcp.WithNumber("limit", mcp.Description("Optional: max number of raw packets to verify, 0 means all")),
	), capture.handleVerifyPlugin)

	// sample_bytes_plugin：取证取样（事实），并在 plugin_debug_access 留审计。
	// 硬上限 20 包 / 64 字节不可突破；审计记真实返回量。纯转发到 Runtime Plane。
	s.AddTool(mcp.NewTool("sample_bytes_plugin",
		mcp.WithDescription("Sample the first bytes of a session's raw packets as FACTS only (hexdump, length histogram, first-byte distribution, entropy). No interpretation, no code. Every call is recorded in the plugin_debug_access audit table with the REAL returned packet/byte counts (not the requested ones). Hard cap 20 packets / 64 bytes, not bypassable via parameters. Pure forwarder to the Runtime Plane; MCP reads nothing and writes nothing."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session to sample from")),
		mcp.WithString("plugin", mcp.Description("Optional: plugin name, recorded in the audit row only")),
		mcp.WithNumber("limit", mcp.Description("Optional: requested packet cap (server caps at 20)")),
		mcp.WithNumber("max_bytes", mcp.Description("Optional: requested bytes per packet (server caps at 64)")),
	), capture.handleSampleBytesPlugin)

	s.AddTool(mcp.NewTool("list_all_sessions",
		mcp.WithDescription("List all capture sessions with their metadata (including stopped/offline sessions). Supports optional status filter: running | stopped | error | success | failed (failed maps to error)."),
		mcp.WithString("status", mcp.Description("Optional filter: running | stopped | error | success | failed")),
	), capture.handleListAllSessions)

	s.AddTool(mcp.NewTool("delete_session",
		mcp.WithDescription("Delete a capture session and its data"),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session ID to delete")),
	), capture.handleDeleteSession)

	// Script management tools removed: the Python script sandbox (save_script /
	// list_scripts / run_script / delete_script) has been deleted. Arbitrary
	// Python execution no longer lives in the capture control plane.

	sseServer := server.NewSSEServer(s)
	httpServer := server.NewStreamableHTTPServer(s, server.WithStateLess(true))

	mux := http.NewServeMux()
	mux.Handle("/sse", sseServer.SSEHandler())
	mux.Handle("/message", sseServer.MessageHandler())
	mux.Handle("/mcp", httpServer)
	mux.HandleFunc("/events/plugins", capture.handleEventsSSE)

	// CORS：仅放行 -allowed-origins 中的 Origin（T12 之前是 *，任意站点都能
	// 跨域调用 MCP 工具）。未配置任何 origin 时不返回 CORS 头，同源用法不受影响。
	// 鉴权（B3）：GTA_AUTH_TOKENS 配置了 token 时强制 Bearer 校验（auth.Middleware）；
	// 未配置（匿名模式）时保持旧行为——直接放行、不注入身份。
	resolver, err := auth.LoadFromEnv()
	if err != nil {
		slog.Error("load auth tokens failed", "error", err)
		os.Exit(1)
	}
	authed := buildHTTPHandler(strings.Split(*allowedOrigins, ","), resolver, mux)

	// /singbox/profile 鉴权豁免：手机 sing-box 客户端扫码导入 profile 时无法携带
	// Bearer 头（SFA 不支持自定义请求头）。该端点只输出代理监听端口/地址配置
	// （与扫码页展示的信息一致），不含任何会话/抓包数据，故挂载在鉴权链之外。
	root := http.NewServeMux()
	root.HandleFunc("/singbox/profile", capture.handleSingboxProfile)
	root.Handle("/", authed)
	handler := http.Handler(root)

	customServer := &http.Server{
		Addr:    *addr,
		Handler: handler,
	}
	// 用 net.Listen 以便拿到实际监听地址（:0 动态分配时回写 <workdir>/addr.mcp.json，
	// 同机可跑多套实例）。
	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		slog.Error("listen", "addr", *addr, "error", err)
		os.Exit(1)
	}
	slog.Info("mcp server listening", "addr", lis.Addr().String(), "endpoints", []string{"/sse", "/message", "/mcp"}, "raw_debug_enabled", capture.enableRawDebug, "auth_enabled", resolver.Required(), "allowed_origins", *allowedOrigins)
	config.WriteAddrFile(absWorkDir, "mcp", lis.Addr().String())
	if err := customServer.Serve(lis); err != nil && err != http.ErrServerClosed {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}
