package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/gopacket/pcap"

	"gta/pkg/analyze"
	"gta/pkg/capture"
	"gta/pkg/internalipc"
	"gta/pkg/internalipc/capturecontrol"
	"gta/pkg/logging"
	"gta/pkg/plugin"
	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
	"gta/pkg/store"
)

// pipelineService 是进程级常驻对象，实现 capturecontrol.CaptureEngine 接口。
// 持有进程级资源（controlStore/registry/rules/workDir）和会话级注册表（tasks map）。
type pipelineService struct {
	controlStore *store.ControlStore
	registry     *plugin.RegistryServer
	rules        []*analyze.CompiledRule
	workDir      string
	logger       *slog.Logger // 进程级 logger，带 component=pipeline_service

	// registryAddr 是插件应连接的注册中心地址（即 -registry-addr 的值，如 :9091）。
	// 通过 GetRegistryAddr 暴露给 gta-mcp，供其 / 插件启动时获知 GTA_REGISTRY_ADDR。
	registryAddr string

	mu    sync.RWMutex
	tasks map[string]*captureTask
}

// newPipelineService 构造 pipelineService，不启动任何会话。
func newPipelineService(workDir string, controlStore *store.ControlStore, registry *plugin.RegistryServer, rules []*analyze.CompiledRule, registryAddr string) *pipelineService {
	return &pipelineService{
		workDir:      workDir,
		controlStore: controlStore,
		registry:     registry,
		rules:        rules,
		logger:       logging.With("component", "pipeline_service"),
		registryAddr: registryAddr,
		tasks:        make(map[string]*captureTask),
	}
}

// 编译期断言：pipelineService 实现 capturecontrol.CaptureEngine 接口。
var _ capturecontrol.CaptureEngine = (*pipelineService)(nil)

// addTask 注册 task 到 map（写锁）。
func (s *pipelineService) addTask(t *captureTask) {
	s.mu.Lock()
	s.tasks[t.sessionID] = t
	s.mu.Unlock()
}

// getTask 查询 task（读锁）。
func (s *pipelineService) getTask(sessionID string) (*captureTask, bool) {
	s.mu.RLock()
	t, ok := s.tasks[sessionID]
	s.mu.RUnlock()
	return t, ok
}

// removeTask 移除 task（写锁），返回被移除的 task。
func (s *pipelineService) removeTask(sessionID string) (*captureTask, bool) {
	s.mu.Lock()
	t, ok := s.tasks[sessionID]
	if ok {
		delete(s.tasks, sessionID)
	}
	s.mu.Unlock()
	return t, ok
}

// StartSession 创建 captureTask，注入 onFinalize 回调，Start()，注册。
func (s *pipelineService) StartSession(ctx context.Context, req capturecontrol.StartSessionRequest) (capturecontrol.StartSessionResult, error) {
	var iface, pcapFile, sourceName string
	var liveCfg *capturecontrol.LiveConfig
	switch {
	case req.File != nil && req.File.Path != "":
		pcapFile = req.File.Path
		if !filepath.IsAbs(pcapFile) {
			pcapFile, _ = filepath.Abs(pcapFile)
		}
		sourceName = "pcap-file"
	case req.Live != nil:
		iface = req.Live.Device
		sourceName = "pcap-live"
		liveCfg = req.Live
	default:
		return capturecontrol.StartSessionResult{}, internalipc.ErrSourceEmpty
	}

	sessionID := nowSessionID()
	sessionDir := filepath.Join(s.workDir, "sessions", sessionID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return capturecontrol.StartSessionResult{}, fmt.Errorf("create session dir: %w", err)
	}
	dbPath := filepath.Join(sessionDir, "capture.sqlite")

	st, err := store.NewSQLiteStore(dbPath, nil)
	if err != nil {
		return capturecontrol.StartSessionResult{}, fmt.Errorf("open sqlite store: %w", err)
	}

	startTime := time.Now()
	if err := s.controlStore.CreateSession(ctx, store.SessionMeta{
		SessionID: sessionID,
		StartedAt: startTime,
		Status:    "running",
		Port:      req.Port,
		Plugin:    req.Plugin,
		Interface: iface,
		PCAPFile:  pcapFile,
		DBPath:    dbPath,
	}); err != nil {
		_ = st.Close()
		return capturecontrol.StartSessionResult{}, fmt.Errorf("create session record: %w", err)
	}

	sessionCtx, cancel := context.WithCancel(context.Background())

	task := &captureTask{
		sessionID:   sessionID,
		dbPath:      dbPath,
		port:        req.Port,
		plugin:      req.Plugin,
		iface:       iface,
		pcapFile:    pcapFile,
		sourceName:  sourceName,
		liveCfg:     liveCfg,
		start:       startTime,
		reresolve:   make(chan struct{}, 1),
		registry:    s.registry,
		rules:       s.rules,
		logger:      s.logger.With("session_id", sessionID),
		ctx:         sessionCtx,
		cancel:      cancel,
		done:        make(chan struct{}),
		sqliteStore: st,
		onFinalize:  s.finalizeTask,
	}

	// 持有写锁注册 task 并 Start，避免 run goroutine 在 addTask 前退出导致 race
	s.mu.Lock()
	s.tasks[sessionID] = task
	if err := task.Start(); err != nil {
		delete(s.tasks, sessionID)
		s.mu.Unlock()
		cancel()
		_ = st.Close()
		return capturecontrol.StartSessionResult{}, fmt.Errorf("start task: %w", err)
	}
	s.mu.Unlock()

	s.logger.Info("capture session started",
		"session_id", sessionID, "port", req.Port, "plugin", req.Plugin,
		"interface", iface, "pcap_file", pcapFile, "db_path", dbPath)

	return capturecontrol.StartSessionResult{
		SessionID: sessionID,
		State:     capture.StateRunning.String(),
		DBPath:    dbPath,
	}, nil
}

// StopSession 停止抓包会话，返回最终统计。
// 零副作用：只 cancel + wait + 返回 stats。
// 不写 ControlStore，不 removeTask——由 finalizeTask 统一处理。
func (s *pipelineService) StopSession(ctx context.Context, sessionID string) (capturecontrol.StopSessionResult, error) {
	task, ok := s.getTask(sessionID)
	if !ok {
		return capturecontrol.StopSessionResult{}, internalipc.ErrNoActiveCapture
	}
	snap, err := task.Stop(ctx)
	if err != nil {
		return capturecontrol.StopSessionResult{}, err
	}
	duration := time.Since(task.start)
	s.logger.Info("capture session stopped",
		"session_id", sessionID, "raw", snap.RawCount, "events", snap.EventCount,
		"metrics", snap.MetricCount, "decode_errors", snap.DecodeErrors, "duration_sec", duration.Seconds())
	return capturecontrol.StopSessionResult{
		State:        capture.StateClosed.String(),
		RawPackets:   snap.RawCount,
		Events:       snap.EventCount,
		Metrics:      snap.MetricCount,
		DecodeErrors: snap.DecodeErrors,
		DurationSec:  duration.Seconds(),
	}, nil
}

// GetStatus 查询会话状态与统计。从 map 取 task，读 Snapshot()（无锁）。
// sessionID 不匹配或会话未活跃时返回 State="closed"。
func (s *pipelineService) GetStatus(ctx context.Context, sessionID string) (capturecontrol.StatusResult, error) {
	task, ok := s.getTask(sessionID)
	if !ok {
		return capturecontrol.StatusResult{State: capture.StateClosed.String()}, nil
	}
	snap := task.Snapshot()
	return capturecontrol.StatusResult{
		State:        task.State().String(),
		SourceName:   task.sourceName,
		PacketsIn:    snap.PacketsIn,
		PacketsOut:   snap.PacketsOut,
		BytesIn:      snap.BytesIn,
		BytesOut:     snap.BytesOut,
		Drops:        snap.Drops,
		Errors:       snap.Errors,
		Err:          snap.Err,
		RawCount:     snap.RawCount,
		EventCount:   snap.EventCount,
		MetricCount:  snap.MetricCount,
		DecodeErrors: snap.DecodeErrors,
	}, nil
}

// ListSessions 遍历 tasks，返回所有活跃 session 摘要。
func (s *pipelineService) ListSessions(ctx context.Context) ([]capturecontrol.SessionSummary, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]capturecontrol.SessionSummary, 0, len(s.tasks))
	for _, task := range s.tasks {
		out = append(out, capturecontrol.SessionSummary{
			SessionID:  task.sessionID,
			State:      task.State().String(),
			SourceName: task.sourceName,
			Port:       task.port,
			Plugin:     task.getPlugin(),
			Interface:  task.iface,
			PCAPFile:   task.pcapFile,
			Start:      task.start,
		})
	}
	return out, nil
}

// ListInterfaces 列出可用网卡名称。
func (s *pipelineService) ListInterfaces(ctx context.Context) ([]string, error) {
	devs, err := pcap.FindAllDevs()
	if err != nil {
		return nil, fmt.Errorf("list interfaces: %w", err)
	}
	names := make([]string, 0, len(devs))
	for _, dev := range devs {
		names = append(names, dev.Name)
	}
	return names, nil
}

// finalizeTask 是 captureTask run 退出时的回调（自动结束或显式停止都会触发）。
// 唯一负责：写 ControlStore 最终统计 + removeTask。
// sqliteStore 由 captureTask 自己在 run defer 中关闭。
func (s *pipelineService) finalizeTask(task *captureTask) {
	snap := task.Snapshot()
	stoppedAt := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.controlStore.UpdateSession(ctx, store.SessionMeta{
		SessionID:    task.sessionID,
		StartedAt:    task.start,
		StoppedAt:    &stoppedAt,
		Status:       "stopped",
		Port:         task.port,
		Plugin:       task.getPlugin(),
		Interface:    task.iface,
		PCAPFile:     task.pcapFile,
		RawPackets:   snap.RawCount,
		Events:       snap.EventCount,
		Metrics:      snap.MetricCount,
		DecodeErrors: snap.DecodeErrors,
		DurationSec:  time.Since(task.start).Seconds(),
		DBPath:       task.dbPath,
	}); err != nil {
		s.logger.Error("finalizeTask: update control store", "error", err, "session_id", task.sessionID)
	}
	s.removeTask(task.sessionID)
}

// StopAll 优雅停止所有活跃抓包会话，并等待各自 finalize 完成（写 ControlStore running→stopped）。
//
// 在进程收到退出信号时调用，确保本进程遗留的 running 会话在退出前被正确置为 stopped，
// 避免重启后 ControlStore 中残留 running 导致前端持续显示"运行中"。
// 超时由 ctx 控制；即便超时，cancel 已触发，run goroutine 仍会在后台完成 finalize 写库。
func (s *pipelineService) StopAll(ctx context.Context) {
	s.mu.RLock()
	tasks := make([]*captureTask, 0, len(s.tasks))
	for _, t := range s.tasks {
		tasks = append(tasks, t)
	}
	s.mu.RUnlock()

	if len(tasks) == 0 {
		return
	}
	s.logger.Info("graceful shutdown: stopping capture sessions", "count", len(tasks))
	for _, t := range tasks {
		if _, err := t.Stop(ctx); err != nil {
			s.logger.Warn("graceful shutdown: stop session timed out or errored",
				"session_id", t.sessionID, "error", err)
		}
	}
}

// ListPlugins 列出当前已注册的插件摘要。
func (s *pipelineService) ListPlugins(_ context.Context) ([]capturecontrol.PluginSummary, error) {
	summaries := s.registry.List()
	out := make([]capturecontrol.PluginSummary, 0, len(summaries))
	for _, sp := range summaries {
		out = append(out, capturecontrol.PluginSummary{
			InstanceID:    sp.InstanceID,
			Name:          sp.Name,
			Protocol:      sp.Protocol,
			Type:          sp.Type,
			APIVersion:    sp.APIVersion,
			SocketPath:    sp.SocketPath,
			Online:        sp.Online,
			LastHeartbeat: sp.LastHeartbeat,
		})
	}
	return out, nil
}

// GetPluginManifest 获取指定插件的 manifest bytes（plugin.yaml 原始内容）。
func (s *pipelineService) GetPluginManifest(_ context.Context, name string) ([]byte, error) {
	return s.registry.GetPluginManifest(name)
}

// GetRegistryAddr 返回插件应连接的注册中心地址（即 -registry-addr 的值）。
// gta-mcp 借此向调用方/插件暴露 GTA_REGISTRY_ADDR，避免插件「不知道该连哪里」。
func (s *pipelineService) GetRegistryAddr(_ context.Context) (string, error) {
	return s.registryAddr, nil
}

// DeregisterPlugin 注销指定插件（按 instance_id 或 name）。
func (s *pipelineService) DeregisterPlugin(ctx context.Context, instanceID, name string) (string, error) {
	if instanceID != "" {
		req := &pb.DeregisterRequest{InstanceId: instanceID}
		_, err := s.registry.Deregister(ctx, req)
		if err != nil {
			return "", err
		}
		return instanceID, nil
	}

	if name != "" {
		summaries := s.registry.List()
		for _, sp := range summaries {
			if sp.Name == name {
				req := &pb.DeregisterRequest{InstanceId: sp.InstanceID}
				_, err := s.registry.Deregister(ctx, req)
				if err != nil {
					return "", err
				}
				return sp.InstanceID, nil
			}
		}
		return "", fmt.Errorf("plugin %q not found", name)
	}

	return "", fmt.Errorf("instance_id or name is required")
}

// SetSessionPlugin 运行中热切换某抓包会话的解码插件绑定。
// 委托给对应 captureTask；会话不存在或已结束时返回 ErrNoActiveCapture。
func (s *pipelineService) SetSessionPlugin(_ context.Context, sessionID, plugin string) (string, error) {
	task, ok := s.getTask(sessionID)
	if !ok {
		return "", internalipc.ErrNoActiveCapture
	}
	return task.SetSessionPlugin(context.Background(), plugin)
}

// SubscribePlugins 订阅插件注册表状态变化事件流。
// 委托给 registry 的 Subscribe，随 ctx 取消自动退订，并把 plugin.PluginEvent
// 映射为 capturecontrol.PluginEvent 转发给 gRPC 流的消费方。
func (s *pipelineService) SubscribePlugins(ctx context.Context) (<-chan capturecontrol.PluginEvent, error) {
	if s.registry == nil {
		return nil, fmt.Errorf("registry not available")
	}
	rawCh, unsub := s.registry.Subscribe()
	out := make(chan capturecontrol.PluginEvent, 16)
	go func() {
		defer close(out)
		defer unsub()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-rawCh:
				if !ok {
					return
				}
				out <- capturecontrol.PluginEvent{
					Type:       string(ev.Type),
					InstanceID: ev.InstanceID,
					Name:       ev.Name,
					Online:     ev.Online,
					Timestamp:  ev.Timestamp,
				}
			}
		}
	}()
	return out, nil
}
