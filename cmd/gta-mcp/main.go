package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/OwnSecurityGuard/gta-plugin-sdk/contract"
	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"google.golang.org/grpc"
	"gopkg.in/yaml.v3"

	"gta/pkg/analyze/semantic"
	"gta/pkg/config"
	"gta/pkg/internalipc"
	pb "gta/pkg/internalipc/proto"
	"gta/pkg/logging"
	"gta/pkg/plugin/skills"
	plugindevclient "gta/pkg/plugindev/client"
	plugindevserver "gta/pkg/plugindev/server"
	"gta/pkg/schema"
	"gta/pkg/store"

	_ "modernc.org/sqlite"
)

type sessionMetadata struct {
	SessionID    string                 `json:"session_id"`
	StartedAt    string                 `json:"started_at"`
	StoppedAt    string                 `json:"stopped_at,omitempty"`
	Status       string                 `json:"status"`
	Port         int                    `json:"port"`
	Plugin       string                 `json:"plugin"`
	Interface    string                 `json:"interface"`
	PCAPFile     string                 `json:"pcap_file,omitempty"`
	RawPackets   int64                  `json:"raw_packets,omitempty"`
	Events       int64                  `json:"events,omitempty"`
	Metrics      int64                  `json:"metrics,omitempty"`
	DecodeErrors int64                  `json:"decode_errors,omitempty"`
	DurationSec  float64                `json:"duration_sec,omitempty"`
	DBPath       string                 `json:"db_path"`
	Extra        map[string]interface{} `json:"extra,omitempty"`
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

func (sm *sessionManager) currentPath() string {
	return filepath.Join(sm.workDir, "current.json")
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
	tmpPath := sm.currentPath() + ".tmp"
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, sm.currentPath())
}

func (sm *sessionManager) readCurrent() (*sessionMetadata, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.readCurrentLocked()
}

func (sm *sessionManager) readCurrentLocked() (*sessionMetadata, error) {
	data, err := os.ReadFile(sm.currentPath())
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

func (sm *sessionManager) listSessions() ([]sessionMetadata, error) {
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
		meta, err := sm.readSessionMetadata(sessionID)
		if err != nil {
			slog.Warn("read session metadata failed", "session_id", sessionID, "error", err)
			continue
		}
		if meta != nil {
			sessions = append(sessions, *meta)
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

func (sm *sessionManager) readSessionMetadata(sessionID string) (*sessionMetadata, error) {
	current, err := sm.readCurrent()
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

func (sm *sessionManager) deleteSession(sessionID string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	current, err := sm.readCurrentLocked()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if current != nil && current.SessionID == sessionID {
		tmpPath := sm.currentPath() + ".tmp"
		if err := os.WriteFile(tmpPath, []byte("{}"), 0644); err != nil {
			return err
		}
		if err := os.Rename(tmpPath, sm.currentPath()); err != nil {
			return err
		}
	}

	sessionDir := sm.sessionDir(sessionID)
	return os.RemoveAll(sessionDir)
}

func newMCPCapture(iface, pluginsDir, workDir, pipelineAddr string, mcpServer *server.MCPServer, enableRawDebug bool) (*mcpCapture, error) {
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
	port, err := req.RequireInt("port")
	if err != nil {
		return errorResult(err), nil
	}
	pluginName := req.GetString("plugin", "")
	pcapFile := req.GetString("pcap_file", "")
	if pcapFile != "" && !filepath.IsAbs(pcapFile) {
		pcapFile, _ = filepath.Abs(pcapFile)
	}
	slog.Info("start_capture requested", "port", port, "plugin", pluginName, "pcap_file", pcapFile)

	// 构造 gRPC request
	grpcReq := &pb.StartCaptureRequest{
		Plugin: pluginName,
		Port:   int32(port),
	}
	if pcapFile != "" {
		grpcReq.Source = &pb.StartCaptureRequest_File{
			File: &pb.PcapFileConfig{Path: pcapFile},
		}
	} else {
		// Live capture：使用配置的网卡，若 Device 为空则由 pipeline 自动探测所有网卡
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
		SessionID: resp.GetSessionId(),
		StartedAt: time.Now().Format(time.RFC3339),
		Status:    "running",
		Port:      port,
		Plugin:    pluginName,
		Interface: m.iface,
		PCAPFile:  pcapFile,
		DBPath:    resp.GetDbPath(),
	}
	if err := m.sessionMgr.writeSessionMetadata(resp.GetSessionId(), meta); err != nil {
		slog.Warn("write session metadata failed", "session_id", resp.GetSessionId(), "error", err)
	}
	m.sessionMgr.writeCurrent(meta)

	slog.Info("start_capture succeeded", "session_id", resp.GetSessionId(), "port", port, "plugin", pluginName, "db_path", resp.GetDbPath())
	return successResult(map[string]any{
		"status":     "started",
		"session_id": resp.GetSessionId(),
		"port":       port,
		"plugin":     pluginName,
		"db_path":    resp.GetDbPath(),
		"interface":  m.iface,
	}), nil
}

func (m *mcpCapture) handleStopCapture(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := req.GetString("session_id", "")
	slog.Info("stop_capture requested", "session_id", sessionID)

	if sessionID == "" {
		// 回退到当前 session（向后兼容）
		sess, err := m.sessionMgr.readCurrent()
		if err != nil {
			return errorResult(fmt.Errorf("read current session: %w", err)), nil
		}
		if sess == nil || sess.Status == "stopped" {
			slog.Warn("stop_capture rejected: no active capture session")
			return errorResult(fmt.Errorf("no active capture session")), nil
		}
		sessionID = sess.SessionID
	}

	resp, err := m.pipelineClient.StopCapture(ctx, &pb.StopCaptureRequest{SessionId: sessionID})
	if err != nil {
		return errorResult(fmt.Errorf("stop capture: %w", err)), nil
	}

	// 更新 session 元数据（如果 sessionMgr 中有记录）
	if sess, err := m.sessionMgr.readCurrent(); err == nil && sess != nil && sess.SessionID == sessionID {
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
	sessionID := req.GetString("session_id", "")

	// 如果未指定 session_id，回退到当前 session
	if sessionID == "" {
		sess, err := m.sessionMgr.readCurrent()
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
	sess, err := m.sessionMgr.readSessionMetadata(sessionID)
	if err != nil || sess == nil {
		return successResult(map[string]any{"state": "closed", "session_id": sessionID}), nil
	}
	return successResult(map[string]any{
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
	}), nil
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
	return successResult(map[string]any{"guide": string(skills.DevGuide())}), nil
}

func (m *mcpCapture) handleListRegisteredPlugins(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	resp, err := m.pipelineClient.ListPlugins(ctx, &pb.ListPluginsRequest{})
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
		})
	}
	return successResult(map[string]any{"plugins": plugins}), nil
}

func (m *mcpCapture) handleGetPluginManifest(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.pipelineClient == nil {
		return errorResult(fmt.Errorf("pipeline client not available")), nil
	}
	name := req.GetString("name", "")
	resp, err := m.pipelineClient.GetPluginManifest(ctx, &pb.GetPluginManifestRequest{Name: name})
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
	dbPath, err := m.getDBPath(sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	slog.Info("aggregate_query requested", "expression", expression, "db_path", dbPath, "session_id", sessionID)
	if dbPath == "" {
		slog.Warn("aggregate_query rejected: no capture database available")
		return errorResult(fmt.Errorf("no capture database available; start a capture first")), nil
	}

	reader, err := m.openReader(sessionID)
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
	return successResult(map[string]any{"count": len(matched), "metrics": matched}), nil
}

func (m *mcpCapture) handleGetCaptureSchema(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := req.GetString("session_id", "")
	dbPath, err := m.getDBPath(sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	slog.Info("get_capture_schema requested", "db_path", dbPath, "session_id", sessionID)
	if dbPath == "" {
		slog.Warn("get_capture_schema rejected: no capture database available")
		return errorResult(fmt.Errorf("no capture database available; start a capture first")), nil
	}

	sessionDir := filepath.Dir(dbPath)

	// 打开 reader 用于 loadDataFields 采样推断
	reader, err := m.openReader(sessionID)
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

	// 读取 schema.json 或采样推断 data.* 字段
	dataFields, err := loadDataFields(ctx, sessionDir, reader)
	if err != nil {
		slog.Warn("load data fields failed", "error", err)
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

	slog.Info("get_capture_schema completed", "session_dir", sessionDir, "decoded_columns", len(decodedColumns), "rules", len(rules))
	return successResult(map[string]any{
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
	}), nil
}

type dataField struct {
	name string
	typ  string
}

func loadDataFields(ctx context.Context, sessionDir string, reader captureReader) ([]dataField, error) {
	// 优先读取 schema.json
	schemaPath := filepath.Join(sessionDir, "schema.json")
	if schemaData, err := os.ReadFile(schemaPath); err == nil {
		var s schema.Schema
		if err := json.Unmarshal(schemaData, &s); err == nil && s.Fields != nil {
			fields := make([]dataField, 0, len(s.Fields))
			for name, f := range s.Fields {
				fields = append(fields, dataField{name: name, typ: f.Type})
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
	dbPath, err := m.getDBPath(sessionID)
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

	reader, err := m.openReader(sessionID)
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
	eventRows, err := reader.QueryEvents(ctx, sessionID, 0, 0)
	if err != nil {
		return errorResult(fmt.Errorf("query events: %w", err)), nil
	}
	slog.Info("list_decoded_data: queried events from db", "session_id", sessionID, "db_path", dbPath, "raw_event_count", len(eventRows))

	matched := make([]map[string]any, 0)
	for _, ev := range eventRows {
		// 从 Event 构建 eventMap
		dataContent := ev.Payload.Value.ToAny()
		if dataContent == nil {
			dataContent = map[string]any{}
		}

		eventMap := map[string]any{
			"id":         string(ev.Identity.ID),
			"timestamp":  ev.Identity.Timestamp.Format(time.RFC3339),
			"session_id": ev.Identity.SessionID,
			"protocol":   string(ev.Identity.Type),
			"raw_len":    0, // Event 不再保留 raw_len
			"data":       dataContent,
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

	dbPath, err := m.getDBPath(sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	slog.Info("list_raw_packets requested", "limit", limit, "offset", offset, "protocol", protocol, "src", src, "dst", dst, "db_path", dbPath, "session_id", sessionID)
	if dbPath == "" {
		return errorResult(fmt.Errorf("no capture database available; start a capture first")), nil
	}

	reader, err := m.openReader(sessionID)
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

	dbPath, err := m.getDBPath(sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	slog.Info("list_state_changes requested", "limit", limit, "offset", offset, "subject_type", subjectType, "subject_id", subjectID, "op", op, "path", path, "flow_id", flowID, "db_path", dbPath, "session_id", sessionID)
	if dbPath == "" {
		return errorResult(fmt.Errorf("no capture database available; start a capture first")), nil
	}

	reader, err := m.openReader(sessionID)
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
		"evidence_nodes":      true,
		"evidence_edges":      true,
	}
	sessionID := req.GetString("session_id", "")
	table := req.GetString("table", "")
	limit := req.GetInt("limit", 100)
	offset := req.GetInt("offset", 0)
	if sessionID == "" {
		return errorResult(fmt.Errorf("session_id is required")), nil
	}
	if !allowed[table] {
		return errorResult(fmt.Errorf("table %q is not in the allowlist; use one of: event_index, plugin_debug_access, raw_packets, events, state_changes, aggregated_metrics, evidence_nodes, evidence_edges", table)), nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	reader, err := m.openReader(sessionID)
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

// handleQueryEvidenceGraph 查询证据图（节点 + 边）。
// 支持按 node_kind、flow_id、edge_type 过滤，以及从根节点出发扩展邻接子图。
func (m *mcpCapture) handleQueryEvidenceGraph(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	limit := req.GetInt("limit", 100)
	offset := req.GetInt("offset", 0)
	sessionID := req.GetString("session_id", "")
	nodeKind := req.GetString("node_kind", "")
	flowID := req.GetString("flow_id", "")
	edgeType := req.GetString("edge_type", "")
	minConfidence := req.GetFloat("min_confidence", 0)
	rootNodeID := req.GetString("root_node_id", "")
	maxDepth := req.GetInt("max_depth", 0)

	dbPath, err := m.getDBPath(sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	if dbPath == "" {
		return errorResult(fmt.Errorf("no capture database available; start a capture first")), nil
	}

	slog.Info("query_evidence_graph requested", "session_id", sessionID, "node_kind", nodeKind, "flow_id", flowID, "edge_type", edgeType, "root_node", rootNodeID, "max_depth", maxDepth)

	reader, err := m.openReader(sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	defer reader.Close()

	q := store.EvidenceGraphQuery{
		SessionID:     sessionID,
		NodeKind:      nodeKind,
		FlowID:        flowID,
		EdgeType:      edgeType,
		MinConfidence: minConfidence,
		RootNodeID:    rootNodeID,
		MaxDepth:      maxDepth,
		Limit:         limit,
		Offset:        offset,
	}
	result, err := reader.QueryEvidenceGraph(ctx, q)
	if err != nil {
		return errorResult(fmt.Errorf("query evidence graph: %w", err)), nil
	}

	// 将节点/边转换为统一的 v1 Contract 输出（含 Semantic 投影与 Strength/Method/RuleID/EvidenceIDs）。
	nodes := make([]map[string]any, 0, len(result.Nodes))
	for _, n := range result.Nodes {
		nodes = append(nodes, v1EvidenceNodeEntry(n))
	}

	edges := make([]map[string]any, 0, len(result.Edges))
	for _, e := range result.Edges {
		edges = append(edges, v1EvidenceEdgeEntry(e))
	}

	slog.Info("query_evidence_graph completed", "nodes", len(nodes), "edges", len(edges))
	return successResult(map[string]any{
		"count": len(nodes) + len(edges),
		"nodes": nodes,
		"edges": edges,
	}), nil
}

// handleTraceEventChain 追踪事件的完整上下游证据链。
// 从指定事件节点出发，沿证据图边 BFS 遍历上游（跟随 target→source）和下游（跟随 source→target），
// 返回按深度组织的节点链路和边关系。
func (m *mcpCapture) handleTraceEventChain(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := req.GetString("session_id", "")
	eventID := req.GetString("event_id", "")
	nodeID := req.GetString("node_id", "")
	maxDepth := req.GetInt("max_depth", 5)
	minConfidence := req.GetFloat("min_confidence", 0.5)

	if eventID == "" && nodeID == "" {
		return errorResult(fmt.Errorf("either event_id or node_id is required")), nil
	}

	dbPath, err := m.getDBPath(sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	if dbPath == "" {
		return errorResult(fmt.Errorf("no capture database available; start a capture first")), nil
	}

	reader, err := m.openReader(sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	defer reader.Close()

	// 解析起始节点 ID
	startNodeID := nodeID
	if startNodeID == "" {
		startNodeID, err = reader.QueryEventNodeID(ctx, sessionID, eventID)
		if err != nil {
			return errorResult(fmt.Errorf("resolve event node: %w", err)), nil
		}
	}

	slog.Info("trace_event_chain start", "session_id", sessionID, "start_node", startNodeID, "max_depth", maxDepth, "min_confidence", minConfidence)

	// BFS 双向追踪
	visited := map[string]bool{startNodeID: true}
	type chainElem struct {
		NodeID    string
		Depth     int
		Direction string // "upstream" 或 "downstream"
	}
	queue := []chainElem{{NodeID: startNodeID, Depth: 0, Direction: "root"}}
	upstream := []map[string]any{}
	downstream := []map[string]any{}
	allEdges := []store.EvidenceEdgeRow{}
	nodeIDSet := map[string]bool{startNodeID: true}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.Depth >= maxDepth {
			continue
		}

		if cur.Direction == "root" || cur.Direction == "downstream" {
			// 下游：source = cur.NodeID, 跟随 target
			edges, err := reader.QueryEvidenceEdges(ctx, store.EvidenceEdgeQuery{
				SessionID: sessionID,
				Source:    cur.NodeID,
				Limit:     100,
			})
			if err != nil {
				slog.Warn("query downstream edges", "node", cur.NodeID, "error", err)
			} else {
				for _, e := range edges {
					if e.Confidence < minConfidence {
						continue
					}
					if !visited[e.Target] {
						visited[e.Target] = true
						nodeIDSet[e.Target] = true
						queue = append(queue, chainElem{NodeID: e.Target, Depth: cur.Depth + 1, Direction: "downstream"})
						downstream = append(downstream, v1ChainHop(e, e.Target, cur.Depth+1))
					}
					allEdges = append(allEdges, e)
				}
			}
		}

		if cur.Direction == "root" || cur.Direction == "upstream" {
			// 上游：target = cur.NodeID, 跟随 source
			edges, err := reader.QueryEvidenceEdges(ctx, store.EvidenceEdgeQuery{
				SessionID: sessionID,
				Target:    cur.NodeID,
				Limit:     100,
			})
			if err != nil {
				slog.Warn("query upstream edges", "node", cur.NodeID, "error", err)
			} else {
				for _, e := range edges {
					if e.Confidence < minConfidence {
						continue
					}
					if !visited[e.Source] {
						visited[e.Source] = true
						nodeIDSet[e.Source] = true
						queue = append(queue, chainElem{NodeID: e.Source, Depth: cur.Depth + 1, Direction: "upstream"})
						upstream = append(upstream, v1ChainHop(e, e.Source, cur.Depth+1))
					}
					allEdges = append(allEdges, e)
				}
			}
		}
	}

	// 查询涉及的节点信息
	nodeIDs := make([]string, 0, len(nodeIDSet))
	for id := range nodeIDSet {
		nodeIDs = append(nodeIDs, id)
	}
	nodes, err := reader.QueryEvidenceNodesByIDs(ctx, sessionID, nodeIDs)
	if err != nil {
		slog.Warn("query nodes by ids", "error", err)
		nodes = nil
	}

	// 格式化节点（统一 v1 Contract 输出：含 Semantic 投影）
	nodeList := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		nodeList = append(nodeList, v1EvidenceNodeEntry(n))
	}

	// 格式化边（统一 v1 Contract 输出：含 Strength/Method/RuleID/EvidenceIDs）
	edgeList := make([]map[string]any, 0, len(allEdges))
	for _, e := range allEdges {
		edgeList = append(edgeList, v1EvidenceEdgeEntry(e))
	}

	slog.Info("trace_event_chain done", "nodes", len(nodeList), "edges", len(edgeList), "upstream", len(upstream), "downstream", len(downstream))

	return successResult(map[string]any{
		"start_node_id": startNodeID,
		"nodes":         nodeList,
		"edges":         edgeList,
		"upstream":      upstream,
		"downstream":    downstream,
	}), nil
}

// v1EvidenceNodeEntry 将 EvidenceNodeRow 转换为统一的 v1 Contract 节点输出。
//
// 关键：消费 Phase 2 确定性投影产出的 Semantic（json 形式复用 SemanticEvent 的
// json tag，保证与 SSOT 文档一致），并把 Strength/Method/RuleID/EvidenceIDs 等
// v1 结构化字段带上。
//
// ⚠️ v1 契约外字段（Labels / Properties）不再对外输出：二者已标记 deprecated，
// 仅是内部兼容读取旧数据的过渡字段，既非稳定契约、又易退化为"各自塞字段"的逃生通道，
// 故 MCP v1 输出严格排除。存储层仍照常读取（EvidenceNodeRow.Labels/Properties），
// 保留对历史数据的内部兼容，但不进入 Agent/UI 可见的契约。
func v1EvidenceNodeEntry(n store.EvidenceNodeRow) map[string]any {
	entry := map[string]any{
		"id":         n.ID,
		"session_id": n.SessionID,
		"kind":       n.Kind,
		"timestamp":  n.Timestamp,
	}
	if n.FlowID != "" {
		entry["flow_id"] = n.FlowID
	}
	// Semantic 是事件节点的语义投影（Phase 2 确定性投影器产出）。
	// 经 SemanticEvent 反序列化再序列化，保证输出严格符合 v1 Contract 的字段/枚举，
	// 不被存储层可能的历史格式漂移污染。
	if n.Semantic != "" {
		var sem semantic.SemanticEvent
		if err := json.Unmarshal([]byte(n.Semantic), &sem); err == nil {
			if raw, mErr := json.Marshal(sem); mErr == nil {
				entry["semantic"] = json.RawMessage(raw)
			}
		} else {
			slog.Warn("query_evidence_graph: skip invalid semantic blob", "node", n.ID, "error", err)
		}
	}
	return entry
}

// v1EvidenceEdgeEntry 将 EvidenceEdgeRow 转换为统一的 v1 Contract 边输出。
//
// Confidence（判定可信度）与 Strength（证据强度）并列输出，二者含义不同，
// 不可合并；Method/RuleID/EvidenceIDs 提供可解释性与溯源。
func v1EvidenceEdgeEntry(e store.EvidenceEdgeRow) map[string]any {
	entry := map[string]any{
		"id":         e.ID,
		"session_id": e.SessionID,
		"source":     e.Source,
		"target":     e.Target,
		"type":       e.Type,
		"confidence": e.Confidence,
	}
	if e.Reason != "" {
		entry["reason"] = e.Reason
	}
	if e.Strength != "" {
		entry["strength"] = e.Strength
	}
	if e.Method != "" {
		entry["method"] = e.Method
	}
	if e.RuleID != "" {
		entry["rule_id"] = e.RuleID
	}
	// EvidenceIDs 以 JSON 数组原样输出（如 ["pkt-1"] 或 ["reqID","respID"]）。
	if e.EvidenceIDs != "" {
		if raw := json.RawMessage(e.EvidenceIDs); json.Valid(raw) {
			entry["evidence_ids"] = raw
		}
	}
	return entry
}

// v1ChainHop 把一条边封装为链路追踪里 upstream/downstream 的轻量跳点。
//
// nodeID 是该跳点所到达的邻接节点（下游为 target，上游为 source）。除基础字段外，
// 一并带上 v1 结构化字段（strength/method/rule_id/evidence_ids），使 AI 无需回查边表即可判断关系性质。
func v1ChainHop(e store.EvidenceEdgeRow, nodeID string, depth int) map[string]any {
	hop := map[string]any{
		"node_id":    nodeID,
		"depth":      depth,
		"edge_type":  e.Type,
		"confidence": e.Confidence,
		"reason":     e.Reason,
	}
	if e.Strength != "" {
		hop["strength"] = e.Strength
	}
	if e.Method != "" {
		hop["method"] = e.Method
	}
	if e.RuleID != "" {
		hop["rule_id"] = e.RuleID
	}
	if e.EvidenceIDs != "" {
		if raw := json.RawMessage(e.EvidenceIDs); json.Valid(raw) {
			hop["evidence_ids"] = raw
		}
	}
	return hop
}

// handleAnalyzeProtocolPatterns 分析会话中的协议模式，为 AI Skill 链路规则发现提供数据。
// 返回：流统计、消息类型分布、实体/StateChange 模式、请求-响应对统计。
func (m *mcpCapture) handleAnalyzeProtocolPatterns(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := req.GetString("session_id", "")
	sampleLimit := req.GetInt("sample_limit", 200)

	dbPath, err := m.getDBPath(sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	if dbPath == "" {
		return errorResult(fmt.Errorf("no capture database available; start a capture first")), nil
	}

	reader, err := m.openReader(sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	defer reader.Close()

	result := make(map[string]any)

	// 1. 流统计：每个 flow 的事件数、方向分布
	flowRows, err := reader.RawQuery(ctx,
		`SELECT ei.flow_id, COUNT(*) as event_count,
			SUM(CASE WHEN ei.direction='client_to_server' THEN 1 ELSE 0 END) as c2s,
			SUM(CASE WHEN ei.direction='server_to_client' THEN 1 ELSE 0 END) as s2c
		FROM event_index ei WHERE ei.session_id=? AND ei.flow_id IS NOT NULL
		GROUP BY ei.flow_id ORDER BY event_count DESC LIMIT ?`,
		sessionID, sampleLimit)
	if err == nil {
		result["flows"] = flowRows
	}

	// 2. 消息类型分布
	typeRows, err := reader.RawQuery(ctx,
		`SELECT ei.type as event_type, COUNT(*) as count
		FROM event_index ei WHERE ei.session_id=?
		GROUP BY ei.type ORDER BY count DESC`,
		sessionID)
	if err == nil {
		result["event_types"] = typeRows
	}

	// 3. 有 correlation_id 的请求-响应对统计
	corrRows, err := reader.RawQuery(ctx,
		`SELECT flow_id, COUNT(*) as correlation_count
		FROM event_index WHERE session_id=? AND correlation_id != ''
		GROUP BY flow_id ORDER BY correlation_count DESC LIMIT ?`,
		sessionID, sampleLimit)
	if err == nil {
		result["correlated_flows"] = corrRows
	}

	// 4. 实体状态变更模式：subject 分布
	scRows, err := reader.RawQuery(ctx,
		`SELECT subject_type, COUNT(*) as change_count, COUNT(DISTINCT subject_id) as distinct_subjects
		FROM state_changes WHERE session_id=?
		GROUP BY subject_type ORDER BY change_count DESC LIMIT ?`,
		sessionID, sampleLimit)
	if err == nil {
		result["state_change_subjects"] = scRows
	}

	// 5. 状态变更操作分布
	opRows, err := reader.RawQuery(ctx,
		`SELECT subject_type, op, path, COUNT(*) as count
		FROM state_changes WHERE session_id=?
		GROUP BY subject_type, op, path ORDER BY count DESC LIMIT ?`,
		sessionID, sampleLimit)
	if err == nil {
		result["state_change_patterns"] = opRows
	}

	// 6. 证据图结构概览（若有）
	graphRows, err := reader.RawQuery(ctx,
		`SELECT kind, COUNT(*) as node_count
		FROM evidence_nodes WHERE session_id=?
		GROUP BY kind`,
		sessionID)
	if err == nil && len(graphRows) > 0 {
		result["evidence_graph_nodes"] = graphRows
	}

	edgeRows, err := reader.RawQuery(ctx,
		`SELECT type, COUNT(*) as edge_count, AVG(confidence) as avg_confidence
		FROM evidence_edges WHERE session_id=?
		GROUP BY type ORDER BY edge_count DESC`,
		sessionID)
	if err == nil && len(edgeRows) > 0 {
		result["evidence_graph_edges"] = edgeRows
	}

	// 7. 方向分布统计
	dirRows, err := reader.RawQuery(ctx,
		`SELECT direction, COUNT(*) as count
		FROM event_index WHERE session_id=? AND direction != ''
		GROUP BY direction`,
		sessionID)
	if err == nil && len(dirRows) > 0 {
		result["direction_distribution"] = dirRows
	}

	slog.Info("analyze_protocol_patterns completed", "session", sessionID, "flows", len(flowRows))
	return successResult(result), nil
}

// handleSuggestLinkRules 基于证据图和协议模式自动生成链路规则建议。
// 分析 response_to、updates、caused_by 等边的分组模式，产出结构化的规则建议，
// 供 AI Agent 或人工审查后导入为正式链路规则。
func (m *mcpCapture) handleSuggestLinkRules(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := req.GetString("session_id", "")
	minConfidence := req.GetFloat("min_confidence", 0.6)
	minOccurrences := req.GetInt("min_occurrences", 3)

	dbPath, err := m.getDBPath(sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	if dbPath == "" {
		return errorResult(fmt.Errorf("no capture database available; start a capture first")), nil
	}

	reader, err := m.openReader(sessionID)
	if err != nil {
		return errorResult(err), nil
	}
	defer reader.Close()

	// 查询证据图节点和边
	graphResult, err := reader.QueryEvidenceGraph(ctx, store.EvidenceGraphQuery{SessionID: sessionID})
	if err != nil {
		return errorResult(fmt.Errorf("query evidence graph: %w", err)), nil
	}

	// 构建节点 ID → 标签映射
	type nodeInfo struct {
		Kind   string
		Labels []string
		FlowID string
	}
	nodeMap := make(map[string]nodeInfo, len(graphResult.Nodes))
	for _, n := range graphResult.Nodes {
		var labels []string
		if n.Labels != "" {
			_ = json.Unmarshal([]byte(n.Labels), &labels)
		}
		nodeMap[n.ID] = nodeInfo{Kind: n.Kind, Labels: labels, FlowID: n.FlowID}
	}

	// 获取标签（优先第一个）
	getLabel := func(ni nodeInfo) string {
		if len(ni.Labels) > 0 {
			return ni.Labels[0]
		}
		return ni.Kind
	}

	type edgePair struct {
		SourceType string
		TargetType string
		EdgeType   string
	}
	type pairStats struct {
		Count     int
		TotalConf float64
	}

	pairs := make(map[edgePair]*pairStats)

	for _, e := range graphResult.Edges {
		if e.Confidence < minConfidence {
			continue
		}
		src := nodeMap[e.Source]
		tgt := nodeMap[e.Target]
		key := edgePair{
			SourceType: getLabel(src),
			TargetType: getLabel(tgt),
			EdgeType:   e.Type,
		}
		if ps, ok := pairs[key]; ok {
			ps.Count++
			ps.TotalConf += e.Confidence
		} else {
			pairs[key] = &pairStats{
				Count:     1,
				TotalConf: e.Confidence,
			}
		}
	}

	// 生成建议
	type suggestion struct {
		EdgeType     string   `json:"edge_type"`
		SourceType   string   `json:"source_type"`
		TargetType   string   `json:"target_type"`
		Occurrences  int      `json:"occurrences"`
		AvgConf      float64  `json:"avg_confidence"`
		RuleTemplate string   `json:"rule_template"`
		Notes        []string `json:"notes,omitempty"`
	}

	var suggestions []suggestion
	for pair, ps := range pairs {
		if ps.Count < minOccurrences {
			continue
		}
		s := suggestion{
			EdgeType:    pair.EdgeType,
			SourceType:  pair.SourceType,
			TargetType:  pair.TargetType,
			Occurrences: ps.Count,
			AvgConf:     ps.TotalConf / float64(ps.Count),
		}

		switch pair.EdgeType {
		case "response_to":
			s.RuleTemplate = fmt.Sprintf("link response_to: when %s appears, link it as response to the most recent %s in the same flow", pair.TargetType, pair.SourceType)
			s.Notes = append(s.Notes, "Match by correlation_key or naming pattern (e.g., Req→Resp)")
		case "updates":
			s.RuleTemplate = fmt.Sprintf("link updates: when %s produces state changes, link them as updates to %s entity", pair.SourceType, pair.TargetType)
			s.Notes = append(s.Notes, "Verify the subject_type matches the entity kind")
		case "caused_by":
			s.RuleTemplate = fmt.Sprintf("link caused_by: %s is caused by %s", pair.TargetType, pair.SourceType)
			s.Notes = append(s.Notes, "Check if this is a push/notification triggered by the prior request")
		case "decoded_from":
			s.RuleTemplate = fmt.Sprintf("link decoded_from: %s originates from %s raw packet data", pair.TargetType, pair.SourceType)
			s.Notes = append(s.Notes, "Typically auto-generated; verify the source type matches the decoder protocol")
		case "possible_followup":
			s.RuleTemplate = fmt.Sprintf("link possible_followup: %s may follow %s within %s", pair.TargetType, pair.SourceType, "5s")
			s.Notes = append(s.Notes, fmt.Sprintf("Low confidence (%.2f); consider adding explicit correlation_key or tightening time window", s.AvgConf))
		default:
			s.RuleTemplate = fmt.Sprintf("link %s: %s → %s", pair.EdgeType, pair.SourceType, pair.TargetType)
		}

		suggestions = append(suggestions, s)
	}

	// 按出现次数降序排序
	for i := 0; i < len(suggestions); i++ {
		for j := i + 1; j < len(suggestions); j++ {
			if suggestions[j].Occurrences > suggestions[i].Occurrences {
				suggestions[i], suggestions[j] = suggestions[j], suggestions[i]
			}
		}
	}

	slog.Info("suggest_link_rules completed", "session", sessionID, "pairs", len(pairs), "suggestions", len(suggestions))

	return successResult(map[string]any{
		"session_id":  sessionID,
		"suggestions": suggestions,
		"total_edges": len(graphResult.Edges),
		"total_nodes": len(graphResult.Nodes),
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
func (m *mcpCapture) openReader(sessionID string) (captureReader, error) {
	dbPath, err := m.getDBPath(sessionID)
	if err != nil || dbPath == "" {
		return nil, fmt.Errorf("no db path for session %s: %w", sessionID, err)
	}
	return m.readerOpener(dbPath)
}

// getDBPath 获取指定 session 的 db_path。
// 优先从 ControlStore 查询，回退到 sessionMgr。
func (m *mcpCapture) getDBPath(sessionID string) (string, error) {
	// 1. 尝试 ControlStore
	if m.controlStore != nil && sessionID != "" {
		meta, err := m.controlStore.GetSession(context.Background(), sessionID)
		if err == nil && meta != nil {
			slog.Info("getDBPath: resolved via controlStore", "session_id", sessionID, "db_path", meta.DBPath)
			return meta.DBPath, nil
		}
		slog.Debug("getDBPath: controlStore miss", "session_id", sessionID, "err", err)
	}
	// 2. 回退到 sessionMgr（metadata.json，含 pipeline 返回的绝对 db_path）
	if sessionID != "" {
		meta, err := m.sessionMgr.readSessionMetadata(sessionID)
		if err == nil && meta != nil {
			slog.Info("getDBPath: resolved via sessionMgr metadata", "session_id", sessionID, "db_path", meta.DBPath)
			return meta.DBPath, nil
		}
	}
	// 3. 尝试当前 session
	current, err := m.sessionMgr.readCurrent()
	if err == nil && current != nil {
		slog.Info("getDBPath: resolved via current session", "session_id", sessionID, "current_session_id", current.SessionID, "db_path", current.DBPath)
		return current.DBPath, nil
	}
	slog.Warn("getDBPath: no db path resolved", "session_id", sessionID)
	return "", nil
}

func (m *mcpCapture) handleListAllSessions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessions, err := m.sessionMgr.listSessions()
	if err != nil {
		slog.Error("list_all_sessions failed", "error", err)
		return errorResult(err), nil
	}

	// 返回所有会话（包括已停止的离线会话）
	var out []map[string]any
	for _, sess := range sessions {
		out = append(out, map[string]any{
			"session_id":    sess.SessionID,
			"started_at":    sess.StartedAt,
			"stopped_at":    sess.StoppedAt,
			"status":        sess.Status,
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
		})
	}

	slog.Info("list_all_sessions completed", "count", len(out), "total", len(sessions))
	return successResult(map[string]any{"count": len(out), "sessions": out}), nil
}

func (m *mcpCapture) handleDeleteSession(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID, err := req.RequireString("session_id")
	if err != nil {
		return errorResult(err), nil
	}

	// 检查 session 是否正在运行
	current, err := m.sessionMgr.readCurrent()
	running := err == nil && current != nil && current.Status == "running" && current.SessionID == sessionID

	if running {
		slog.Warn("delete_session rejected: session is running", "session_id", sessionID)
		return errorResult(fmt.Errorf("cannot delete running session %s; stop it first", sessionID)), nil
	}

	if err := m.sessionMgr.deleteSession(sessionID); err != nil {
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
	w.Header().Set("Access-Control-Allow-Origin", "*")

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
	addr := flag.String("addr", ":8781", "SSE server address")
	iface := flag.String("iface", "", "capture interface; empty means all available interfaces")
	pluginsDir := flag.String("plugins-dir", "plugins", "plugins directory")
	workDir := flag.String("work-dir", ".", "working directory for session databases")
	pipelineAddr := flag.String("pipeline-addr", ":9888", "gta-pipeline gRPC 地址（默认 :9888）")
	debug := flag.Bool("debug", false, "enable debug logging")
	enableRawDebug := flag.Bool("enable-raw-debug", os.Getenv("GTA_MCP_ENABLE_RAW_DEBUG") == "1", "暴露原始包调试工具（list_raw_packets / decode_raw_packets），仅限插件开发调试；默认关闭")
	logFormat := flag.String("log-format", "json", "log format: json | text")
	logFile := flag.String("log-file", "", "log file path (default: <workdir>/logs/gta-mcp.log)")
	flag.Parse()

	// 统一日志初始化：文件落盘 + stderr 双写 + 按大小轮转
	absWorkDir, _ := filepath.Abs(*workDir)
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

	capture, err := newMCPCapture(*iface, resolvedPluginsDir, *workDir, *pipelineAddr, s, *enableRawDebug)
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
		mcp.WithDescription("Start capturing traffic on a server port, or replay a pcap file. Packets are always captured and stored; an optional plugin enables protocol decoding."),
		mcp.WithNumber("port", mcp.Required(), mcp.Description("Server port to capture or filter, e.g. 8080")),
		mcp.WithString("plugin", mcp.Description("Optional plugin name for protocol decoding, e.g. http. If omitted or no matching plugin is found, only raw packets are stored.")),
		mcp.WithString("pcap_file", mcp.Description("Optional pcap file to replay instead of live capture")),
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

	s.AddTool(mcp.NewTool("get_registry_addr",
		mcp.WithDescription("Return the registry address the pipeline is currently listening on (its -registry-addr, e.g. :9091). Plugins MUST connect here by setting GTA_REGISTRY_ADDR at startup; this tool removes the guesswork of reading pipeline startup logs. Use it to learn where a freshly launched plugin should register, or to confirm activate_plugin's resolved address."),
	), capture.handleGetRegistryAddr)

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
		mcp.WithDescription("Read-only escape hatch for internal projection/audit tables that have no dedicated tool. Whitelisted tables: event_index (schema indexable_fields projection), plugin_debug_access (audit trail of sampled bytes), raw_packets, events, state_changes, aggregated_metrics, evidence_nodes, evidence_edges. The table name is constrained to the allowlist (no SQL injection possible); limit/offset are parameterized. Use this to verify event_index projections were built, or to inspect plugin_debug_access audit rows."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session ID to query")),
		mcp.WithString("table", mcp.Required(), mcp.Description("Whitelisted table: event_index | plugin_debug_access | raw_packets | events | state_changes | aggregated_metrics | evidence_nodes | evidence_edges")),
		mcp.WithNumber("limit", mcp.DefaultNumber(100), mcp.Description("Max rows (clamped to 1000)")),
		mcp.WithNumber("offset", mcp.DefaultNumber(0), mcp.Description("Offset")),
	), capture.handleQueryCaptureTable)

	s.AddTool(mcp.NewTool("aggregate_query",
		mcp.WithDescription("Query aggregated metrics/statistics using an expr expression over {name, window, value, group}."),
		mcp.WithString("expression", mcp.Required(), mcp.Description("expr expression, e.g. name == 'http_req_count' && value > 0")),
		mcp.WithString("session_id", mcp.Description("Optional session ID to query; defaults to current session")),
	), capture.handleAggregateQuery)

	// 证据图查询工具。
	s.AddTool(mcp.NewTool("query_evidence_graph",
		mcp.WithDescription("Query the evidence graph (nodes + edges) built by the semantic analysis engine. Supports filtering by node kind, flow ID, edge type, and confidence threshold. Use root_node_id + max_depth to expand a neighbourhood subgraph from a specific node."),
		mcp.WithString("session_id", mcp.Description("Optional session ID to query; defaults to current session")),
		mcp.WithNumber("limit", mcp.DefaultNumber(100), mcp.Description("Max results to return")),
		mcp.WithNumber("offset", mcp.DefaultNumber(0), mcp.Description("Offset")),
		mcp.WithString("node_kind", mcp.Description("Filter by node kind: event | raw_packet | entity | state_change")),
		mcp.WithString("flow_id", mcp.Description("Filter by flow ID")),
		mcp.WithString("edge_type", mcp.Description("Filter edges by type: response_to | decoded_from | updates | caused_by | correlated_with | parameter_from | possible_followup")),
		mcp.WithNumber("min_confidence", mcp.Description("Min confidence threshold [0.0-1.0]; only edges at or above this value are returned")),
		mcp.WithString("root_node_id", mcp.Description("Start node ID for neighbourhood traversal; set max_depth to control expansion radius")),
		mcp.WithNumber("max_depth", mcp.Description("Max BFS depth from root_node_id, 0=no traversal")),
	), capture.handleQueryEvidenceGraph)

	// 事件链追踪工具。
	s.AddTool(mcp.NewTool("trace_event_chain",
		mcp.WithDescription("Trace the complete upstream and downstream evidence chain for an event. Starting from an event_id or node_id, performs BFS along evidence graph edges to show: upstream (who caused this event), downstream (what this event caused). Returns structured nodes, edges, and depth-organized upstream/downstream lists."),
		mcp.WithString("session_id", mcp.Description("Optional session ID; defaults to current session")),
		mcp.WithString("event_id", mcp.Description("Event ID (e.g., session-local event ID) to trace. Mutually exclusive with node_id; at least one is required.")),
		mcp.WithString("node_id", mcp.Description("Evidence graph node ID to trace from. Mutually exclusive with event_id.")),
		mcp.WithNumber("max_depth", mcp.DefaultNumber(5), mcp.Description("Max BFS depth (default 5)")),
		mcp.WithNumber("min_confidence", mcp.DefaultNumber(0.5), mcp.Description("Min edge confidence [0.0-1.0] to follow")),
	), capture.handleTraceEventChain)

	// 协议模式分析工具：为 AI Skill 链路规则发现提供数据基础。
	s.AddTool(mcp.NewTool("analyze_protocol_patterns",
		mcp.WithDescription("Analyze protocol patterns in captured traffic to support AI-driven link rule discovery. Returns flow statistics, message type distribution, entity/state change patterns, and evidence graph structure. Use this to understand protocol structure before suggesting link rules."),
		mcp.WithString("session_id", mcp.Description("Optional session ID to analyze; defaults to current session")),
		mcp.WithNumber("sample_limit", mcp.DefaultNumber(200), mcp.Description("Max entries per category")),
	), capture.handleAnalyzeProtocolPatterns)

	// 链路规则建议工具：基于证据图自动生成 link rule 建议。
	s.AddTool(mcp.NewTool("suggest_link_rules",
		mcp.WithDescription("Suggest link rules based on evidence graph analysis. Analyzes response_to, updates, caused_by, and other edge patterns from the semantic engine, then generates structured rule suggestions with confidence scores, occurrence counts, and human-readable rule templates. Use this after capturing and decoding traffic to discover protocol relationships."),
		mcp.WithString("session_id", mcp.Description("Optional session ID to analyze; defaults to current session")),
		mcp.WithNumber("min_confidence", mcp.DefaultNumber(0.6), mcp.Description("Min edge confidence [0.0-1.0] to include in suggestions (default 0.6)")),
		mcp.WithNumber("min_occurrences", mcp.DefaultNumber(3), mcp.Description("Min number of occurrences for a pattern to be suggested (default 3)")),
	), capture.handleSuggestLinkRules)

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
		mcp.WithDescription("Build the chronological evidence chain (causation chain) for one behavior. Stitches request/response/push/state_diff across the run window. Returns steps or file_path for large results."),
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
		mcp.WithDescription("List all capture sessions with their metadata (including stopped/offline sessions)"),
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

	// CORS 中间件：浏览器端 MCP 客户端（如 Claude Desktop 的 webview，origin 异于 8087）
	// 发起跨域请求时，浏览器先发 OPTIONS 预检。mux 默认对 OPTIONS 返回 404 且无 CORS 头，
	// 会导致 "Response to preflight request doesn't pass access control check"。
	// 这里统一处理预检并给所有响应加 Access-Control-Allow-Origin。
	corsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers",
				"Content-Type, Accept, Authorization, Mcp-Session-Id, Last-Event-ID, X-Requested-With")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mux.ServeHTTP(w, r)
	})

	customServer := &http.Server{
		Addr:    *addr,
		Handler: corsHandler,
	}
	slog.Info("mcp server listening", "addr", *addr, "endpoints", []string{"/sse", "/message", "/mcp"}, "raw_debug_enabled", capture.enableRawDebug)
	if err := customServer.ListenAndServe(); err != nil {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}
