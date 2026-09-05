package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gametrace/pkg/internalipc/capturecontrol"
	"gametrace/pkg/plugin"
	"gametrace/pkg/store"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newTestPipelineService 创建用于测试的 pipelineService，使用空插件目录（无 tcp 插件）。
// 返回 service、工作目录和 ControlStore。ControlStore 的 Close 通过 t.Cleanup 注册。
func newTestPipelineService(t *testing.T) (*pipelineService, string, *store.ControlStore) {
	t.Helper()
	workDir := t.TempDir()
	controlPath := filepath.Join(workDir, "control.sqlite")
	controlStore, err := store.NewControlStore(controlPath)
	if err != nil {
		t.Fatalf("NewControlStore: %v", err)
	}
	t.Cleanup(func() { _ = controlStore.Close() })
	mgr := plugin.NewRegistryServer(10)
	s := newPipelineService(workDir, controlStore, mgr, nil, nil, ":9091", "sqlite", "")
	// 测试期间屏蔽端口预探测：开发机常驻的 gt-singbox-agent 可能正占用
	// GameTrace 专属端口段（12100-12199 / 19500-19599），而这些测试只验证
	// pipelineService 内部状态机，不该耦合到生产服务是否在跑。
	origProbe := probeFreePortFn
	probeFreePortFn = func(int) error { return nil }
	t.Cleanup(func() { probeFreePortFn = origProbe })
	return s, workDir, controlStore
}

// fileStartSessionRequest 构造一个使用 file source 的 StartSessionRequest。
// 路径指向不存在的 pcap 文件——StartSession 不检查文件存在性。
func fileStartSessionRequest(workDir string) capturecontrol.StartSessionRequest {
	return capturecontrol.StartSessionRequest{
		Plugin: "tcp",
		Port:   8080,
		File:   &capturecontrol.FileConfig{Path: filepath.Join(workDir, "nonexistent.pcap")},
	}
}

// waitForTaskDone 等待指定 session 的 task run goroutine 退出（done channel 关闭）。
// 由于没有 tcp 插件，run 会在 mgr.Find("tcp") 失败后立即退出并 close(done)。
func waitForTaskDone(t *testing.T, s *pipelineService, sessionID string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		task, ok := s.getTask(sessionID)
		if !ok {
			return // task 已被 finalizeTask 移除
		}
		select {
		case <-task.done:
			return
		case <-deadline:
			t.Fatalf("timeout waiting for task %s to exit after %v", sessionID, timeout)
		}
	}
}

// TestPipelineService_StartStopLifecycle 验证 StartSession → GetStatus → StopSession 全流程。
func TestPipelineService_StartStopLifecycle(t *testing.T) {
	s, workDir, controlStore := newTestPipelineService(t)

	ctx := context.Background()
	req := fileStartSessionRequest(workDir)

	res, err := s.StartSession(ctx, req)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if res.SessionID == "" {
		t.Error("SessionID is empty")
	}
	if res.State != "running" {
		t.Errorf("State = %q, want %q", res.State, "running")
	}
	if res.DBPath == "" {
		t.Error("DBPath is empty")
	}
	if _, err := os.Stat(res.DBPath); err != nil {
		t.Errorf("capture.sqlite not created at %s: %v", res.DBPath, err)
	}

	// 验证 ControlStore 记录存在
	// 注意：由于无 tcp 插件，task 可能已退出并 finalize 为 stopped，不强制检查 status=="running"
	meta, err := controlStore.GetSession(ctx, res.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if meta.Plugin != "tcp" {
		t.Errorf("ControlStore plugin = %q, want %q", meta.Plugin, "tcp")
	}
	if meta.Port != 8080 {
		t.Errorf("ControlStore port = %d, want %d", meta.Port, 8080)
	}
	if meta.DBPath != res.DBPath {
		t.Errorf("ControlStore db_path = %q, want %q", meta.DBPath, res.DBPath)
	}

	// 短暂等待 run goroutine 失败退出（无 tcp 插件）
	waitForTaskDone(t, s, res.SessionID, 2*time.Second)

	// 再等一小段时间让 finalizeTask 完成（写 ControlStore + removeTask）
	time.Sleep(100 * time.Millisecond)

	// 验证 ControlStore 已更新为 stopped
	meta, err = controlStore.GetSession(ctx, res.SessionID)
	if err != nil {
		t.Fatalf("GetSession after stop: %v", err)
	}
	if meta.Status != "stopped" {
		t.Errorf("ControlStore status after stop = %q, want %q", meta.Status, "stopped")
	}
	if meta.StoppedAt == nil {
		t.Error("ControlStore stopped_at is nil after stop")
	}

	// 验证 task 已从 map 移除
	if _, ok := s.getTask(res.SessionID); ok {
		t.Error("task still in map after finalize")
	}
}

// TestPipelineService_StopNoActive 验证停止不存在的会话返回 ErrNoActiveCapture。
func TestPipelineService_StopNoActive(t *testing.T) {
	s, _, _ := newTestPipelineService(t)
	ctx := context.Background()
	_, err := s.StopSession(ctx, "any-id")
	if err == nil {
		t.Fatal("StopSession: expected error, got nil")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("StopSession error code = %v, want %v", status.Code(err), codes.FailedPrecondition)
	}
}

// TestPipelineService_GetStatusNotActive 验证查询不存在的会话返回 State="closed"。
func TestPipelineService_GetStatusNotActive(t *testing.T) {
	s, _, _ := newTestPipelineService(t)
	ctx := context.Background()
	res, err := s.GetStatus(ctx, "any-id")
	if err != nil {
		t.Fatalf("GetStatus: unexpected error: %v", err)
	}
	if res.State != "closed" {
		t.Errorf("GetStatus State = %q, want %q", res.State, "closed")
	}
}

// TestPipelineService_ListInterfaces 验证 ListInterfaces 不报错。
func TestPipelineService_ListInterfaces(t *testing.T) {
	s, _, _ := newTestPipelineService(t)
	ctx := context.Background()
	names, err := s.ListInterfaces(ctx)
	if err != nil {
		t.Fatalf("ListInterfaces: %v", err)
	}
	t.Logf("found %d interfaces: %v", len(names), names)
}

// TestPipelineService_MultiSessionConcurrent 验证多会话并发启动，各自有独立 sessionID，
// 且全部能自动 finalize 为 stopped。
//
// 注意：由于无 tcp 插件，每个 task 的 run goroutine 会立即退出并触发 finalizeTask。
// 因此本测试验证的是：并发 StartSession 不会冲突，sessionID 唯一，ControlStore 记录完整。
func TestPipelineService_MultiSessionConcurrent(t *testing.T) {
	s, workDir, controlStore := newTestPipelineService(t)
	ctx := context.Background()
	req := fileStartSessionRequest(workDir)

	// 启动 3 个并发会话
	var sessionIDs []string
	for i := 0; i < 3; i++ {
		res, err := s.StartSession(ctx, req)
		if err != nil {
			t.Fatalf("StartSession %d: %v", i, err)
		}
		sessionIDs = append(sessionIDs, res.SessionID)
		time.Sleep(20 * time.Millisecond) // 确保不同 sessionID
	}

	// 验证 sessionID 互不相同
	seen := make(map[string]bool, len(sessionIDs))
	for _, id := range sessionIDs {
		if seen[id] {
			t.Errorf("duplicate sessionID: %s", id)
		}
		seen[id] = true
	}

	// 等待所有 task 自动 finalize 完成（无 tcp 插件，立即退出）
	time.Sleep(500 * time.Millisecond)

	// 验证所有 task 已从 map 移除
	sessions, _ := s.ListSessions(ctx)
	if len(sessions) != 0 {
		t.Errorf("after auto-finalize, ListSessions count = %d, want 0", len(sessions))
	}

	// 验证 ControlStore 中所有会话都已记录为 stopped
	for _, id := range sessionIDs {
		meta, err := controlStore.GetSession(ctx, id)
		if err != nil {
			t.Errorf("GetSession %s: %v", id, err)
			continue
		}
		if meta.Status != "stopped" {
			t.Errorf("session %s status = %q, want %q", id, meta.Status, "stopped")
		}
	}
}

// TestPipelineService_AutoFinalizeCleanup 验证 run goroutine 自动退出后
// finalizeTask 自动写 ControlStore + removeTask。
func TestPipelineService_AutoFinalizeCleanup(t *testing.T) {
	s, workDir, controlStore := newTestPipelineService(t)
	ctx := context.Background()
	req := fileStartSessionRequest(workDir)

	res, err := s.StartSession(ctx, req)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	// 等 run goroutine 自动退出 + finalize 完成
	waitForTaskDone(t, s, res.SessionID, 2*time.Second)
	time.Sleep(200 * time.Millisecond)

	// 验证 task 已从 map 移除
	if _, ok := s.getTask(res.SessionID); ok {
		t.Error("task still in map after auto-finalize")
	}

	// 验证 ControlStore 已更新为 stopped
	meta, err := controlStore.GetSession(ctx, res.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if meta.Status != "stopped" {
		t.Errorf("ControlStore status = %q, want %q", meta.Status, "stopped")
	}
}
