package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// RunRecord 是一次用户操作的元数据与边界。
type RunRecord struct {
	RunID       string     `json:"run_id"`
	SessionID   string     `json:"session_id"`
	FeatureName string     `json:"feature_name"`
	ProjectPath string     `json:"project_path,omitempty"`
	PluginName  string     `json:"plugin_name,omitempty"`
	Device      string     `json:"device,omitempty"`
	Filter      string     `json:"filter,omitempty"`
	Port        int        `json:"port,omitempty"`
	TimeFrom    time.Time  `json:"time_from"`
	TimeTo      *time.Time `json:"time_to,omitempty"`
	DurationMs  int64      `json:"duration_ms,omitempty"`

	// capture 隔离模式
	IsolationMode string `json:"isolation_mode"` // reuse_existing | auto_start | time_window_only
	CaptureStatus string `json:"capture_status"` // running | stopped | not_started

	// baseline 快照（begin 时 active capture 的累计计数）
	Baseline snapshotBaseline `json:"baseline"`

	// end 时填充的窗口内增量
	Summary *RunSummary `json:"summary,omitempty"`

	// 状态
	Ended bool `json:"ended"`
}

// snapshotBaseline 是 begin_capture_run 时的 active capture 累计计数快照。
// end_capture_run 时取差，得到窗口内增量。
type snapshotBaseline struct {
	RawPackets   int64 `json:"raw_packets"`
	Events       int64 `json:"events"`
	Metrics      int64 `json:"metrics"`
	DecodeErrors int64 `json:"decode_errors"`
}

// RunSummary 是 end_capture_run 时的窗口内增量统计。
type RunSummary struct {
	CapturedFlowCount    int64 `json:"captured_flow_count"` // -1 表示未落地（结构化字段缺失）
	CapturedMessageCount int64 `json:"captured_message_count"`
	ClientRequestCount   int64 `json:"client_request_count"` // -1 表示未落地
	ServerMessageCount   int64 `json:"server_message_count"` // -1 表示未落地
	DecodeErrorCount     int64 `json:"decode_error_count"`
}

// RunRegistry 管理所有 run 记录，内存 + 文件持久化。
type RunRegistry struct {
	mu         sync.RWMutex
	activeRuns map[string]*RunRecord
	workDir    string
}

// NewRunRegistry 创建 RunRegistry 并从文件系统恢复未结束的 run。
func NewRunRegistry(workDir string) (*RunRegistry, error) {
	r := &RunRegistry{
		activeRuns: make(map[string]*RunRecord),
		workDir:    workDir,
	}
	if err := r.recover(); err != nil {
		return nil, fmt.Errorf("recover runs: %w", err)
	}
	return r, nil
}

// recover 从 workDir/runs/ 恢复未结束的 run。
// 策略：
//   - 未结束的 run：begin 距今 >1h 标记 stale ended（summary 全 -1），≤1h 恢复为 active
func (r *RunRegistry) recover() error {
	runsDir := filepath.Join(r.workDir, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 目录不存在，无历史 run
		}
		return err
	}

	now := time.Now()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runID := entry.Name()
		path := filepath.Join(runsDir, runID, "run.json")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec RunRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			continue
		}
		if rec.Ended {
			continue // 已结束，跳过
		}
		// 未结束的 run
		age := now.Sub(rec.TimeFrom)
		if age > time.Hour {
			// stale：标记为 ended，summary 全 -1
			rec.Ended = true
			rec.Summary = &RunSummary{
				CapturedFlowCount:    -1,
				CapturedMessageCount: -1,
				ClientRequestCount:   -1,
				ServerMessageCount:   -1,
				DecodeErrorCount:     -1,
			}
			_ = r.persist(&rec)
		} else {
			r.activeRuns[rec.RunID] = &rec
		}
	}
	return nil
}

// Begin 创建新的 run 记录。
func (r *RunRegistry) Begin(rec RunRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.activeRuns[rec.RunID]; exists {
		return fmt.Errorf("run_id %s already exists", rec.RunID)
	}
	r.activeRuns[rec.RunID] = &rec
	return r.persist(&rec)
}

// Get 获取 run 记录（active 或已结束）。
func (r *RunRegistry) Get(runID string) (*RunRecord, error) {
	r.mu.RLock()
	rec, ok := r.activeRuns[runID]
	r.mu.RUnlock()
	if ok {
		return rec, nil
	}
	// 尝试从文件加载（已结束的 run）
	path := filepath.Join(r.workDir, "runs", runID, "run.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("run %s not found: %w", runID, err)
	}
	var loaded RunRecord
	if err := json.Unmarshal(data, &loaded); err != nil {
		return nil, fmt.Errorf("unmarshal run %s: %w", runID, err)
	}
	return &loaded, nil
}

// End 关闭 run 窗口，填充 summary。
func (r *RunRegistry) End(runID string, timeTo time.Time, summary RunSummary) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.activeRuns[runID]
	if !ok {
		return fmt.Errorf("run %s not found or already ended", runID)
	}
	rec.TimeTo = &timeTo
	rec.DurationMs = timeTo.Sub(rec.TimeFrom).Milliseconds()
	rec.Summary = &summary
	rec.Ended = true
	delete(r.activeRuns, runID)
	return r.persist(rec)
}

// persist 将 run 记录持久化到文件。
func (r *RunRegistry) persist(rec *RunRecord) error {
	runDir := filepath.Join(r.workDir, "runs", rec.RunID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		return err
	}
	path := filepath.Join(runDir, "run.json")
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// GenerateRunID 生成 run_id：run_{session_id}_{seq}。
// seq 扫描 runs 目录取最大 +1。
func (r *RunRegistry) GenerateRunID(sessionID string) string {
	runsDir := filepath.Join(r.workDir, "runs")
	entries, _ := os.ReadDir(runsDir)
	prefix := fmt.Sprintf("run_%s_", sessionID)
	maxSeq := 0
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, prefix) {
			seqStr := strings.TrimPrefix(name, prefix)
			var seq int
			fmt.Sscanf(seqStr, "%d", &seq)
			if seq > maxSeq {
				maxSeq = seq
			}
		}
	}
	return fmt.Sprintf("%s%03d", prefix, maxSeq+1)
}

// ListActive 返回所有 active run（按 time_from 降序）。
func (r *RunRegistry) ListActive() []*RunRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var runs []*RunRecord
	for _, rec := range r.activeRuns {
		runs = append(runs, rec)
	}
	sort.Slice(runs, func(i, j int) bool {
		return runs[i].TimeFrom.After(runs[j].TimeFrom)
	})
	return runs
}
