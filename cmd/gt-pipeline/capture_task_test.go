package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gametrace/pkg/capture"
	"gametrace/pkg/internalipc"
	"gametrace/pkg/plugin"
	"gametrace/pkg/store"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newTestCaptureTask 创建用于测试的 captureTask，使用空插件目录（无 tcp 插件）。
// run goroutine 会在 mgr.Find("tcp") 失败后立即退出。
// sqliteStore 的 Close 通过 run defer 中的 finalize 完成。
func newTestCaptureTask(t *testing.T) *captureTask {
	t.Helper()
	workDir := t.TempDir()
	sessionID := nowSessionID()
	sessionDir := filepath.Join(workDir, "sessions", sessionID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	dbPath := filepath.Join(sessionDir, "capture.sqlite")
	st, err := store.NewSQLiteStore(dbPath, nil)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	mgr := plugin.NewRegistryServer(10)
	ctx, cancel := context.WithCancel(context.Background())
	return &captureTask{
		sessionID:   sessionID,
		dbPath:      dbPath,
		port:        8080,
		plugin:      "tcp",
		sourceName:  "pcap-file",
		pcapFile:    filepath.Join(workDir, "nonexistent.pcap"),
		start:       time.Now(),
		registry:    mgr,
		rules:       nil,
		logger:      slog.Default(),
		ctx:         ctx,
		cancel:      cancel,
		done:        make(chan struct{}),
		sqliteStore: st,
	}
}

// TestCaptureTask_StartCASDuplicate 验证重复 Start 返回 ErrAlreadyStarted。
func TestCaptureTask_StartCASDuplicate(t *testing.T) {
	task := newTestCaptureTask(t)
	t.Cleanup(func() { <-task.done }) // 等 run goroutine 退出

	if err := task.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	// 等 run goroutine 退出（无 tcp 插件，立即退出）
	<-task.done

	// 第二次 Start 应返回 ErrAlreadyStarted（state 已是 Closed，CAS 失败）
	err := task.Start()
	if err == nil {
		t.Fatal("second Start: expected error, got nil")
	}
	if status.Code(err) != codes.Internal {
		t.Errorf("second Start error code = %v, want %v", status.Code(err), codes.Internal)
	}
}

// TestCaptureTask_StopWithCtxTimeout 验证 Stop 在 ctx 超时时返回 ctx.Err()。
// 通过阻塞 onFinalize 模拟 run goroutine 卡在 finalize 阶段。
func TestCaptureTask_StopWithCtxTimeout(t *testing.T) {
	task := newTestCaptureTask(t)

	finalizeStarted := make(chan struct{})
	blockFinalize := make(chan struct{})
	task.onFinalize = func(t *captureTask) {
		close(finalizeStarted)
		<-blockFinalize
	}

	if err := task.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 等 run goroutine 到达 finalize
	select {
	case <-finalizeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for finalize to start")
	}

	// Stop with short timeout — 应超时因为 finalize 被阻塞
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := task.Stop(ctx)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}

	// 解除阻塞，让 run goroutine 完成
	close(blockFinalize)
	<-task.done
}

// TestCaptureTask_AutoFinalize 验证 run goroutine 退出时 onFinalize 被调用，
// 且 state 变为 Closed。
func TestCaptureTask_AutoFinalize(t *testing.T) {
	task := newTestCaptureTask(t)

	finalized := make(chan struct{})
	var capturedTask *captureTask
	task.onFinalize = func(t *captureTask) {
		capturedTask = t
		close(finalized)
	}

	if err := task.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 等待 onFinalize 被调用
	select {
	case <-finalized:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for onFinalize")
	}

	if capturedTask == nil || capturedTask != task {
		t.Error("onFinalize received wrong task")
	}

	// 等 done 关闭（finalize 完成后 close(done)）
	<-task.done

	// 验证 state 为 Closed
	if task.State() != capture.StateClosed {
		t.Errorf("State = %v, want %v", task.State(), capture.StateClosed)
	}
}

// TestCaptureTask_SnapshotEmpty 验证未启动的 task Snapshot 返回零值。
func TestCaptureTask_SnapshotEmpty(t *testing.T) {
	task := newTestCaptureTask(t)
	// 该测试不启动 run goroutine（其 finalize 负责关闭 sqliteStore），
	// 需显式关闭 store 以释放 Windows 下的文件句柄，否则 TempDir 清理失败。
	t.Cleanup(func() { _ = task.sqliteStore.Close() })
	snap := task.Snapshot()
	if snap.RawCount != 0 || snap.EventCount != 0 {
		t.Errorf("expected zero snapshot, got %+v", snap)
	}
}

// 确保未使用的 import 不会导致编译错误
var _ = internalipc.ErrAlreadyStarted
