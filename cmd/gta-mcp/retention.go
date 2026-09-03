// retention.go — 会话保留策略（TTL + 最大会话数），防止 sessions/ 目录无限膨胀。
//
// 背景：会话由 start_capture / agent 下载等入口自动创建，仅靠用户手动 delete_session
// 清理时，长时间运行的工作目录会积累大量历史 capture.sqlite（每个会话一份库文件）。
// 本策略在 gta-mcp 内周期执行两阶段清理：
//
//	阶段 1（TTL）    无写入活动超过 TTL 的会话直接删除；
//	阶段 2（数量上限）会话数超过 MaxSessions 时，从最旧的非 running 会话开始删除。
//
// 安全性设计：
//   - "活跃"以数据写入（capture.sqlite / 会话目录 mtime）为准，而非仅看 status：
//     崩溃残留、agent 下载后从未上报的 "running" 会话同样超期即清，
//     而真正在抓的会话（持续写库）天然受 mtime 保护；
//   - 指向被删会话的 current 分片（current.json / current.<owner>.json）原子重置为 {}，
//     语义与手动 delete_session 一致；
//   - 删除失败不重试、不中断本轮（下轮再来），仅告警。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gta/pkg/store"
)

// configEnvInt 读取整数环境变量作为 flag 默认值：未设置或解析失败时用 def。
// 环境变量显式设为 "0" 视为有效值（用于关闭对应策略）。
func configEnvInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return def
}

// sessionIDLayout 是会话 ID 的时间编码格式（generateSessionID 产出）。
// 会话 ID 格式为 "20060102_150405.000_XXXX"，其中 XXXX 是随机后缀。
const sessionIDLayout = "20060102_150405.000"

// sessionIDTimestampLength 是会话 ID 中时间戳部分的长度（不含随机后缀）。
const sessionIDTimestampLength = 19

// retentionPolicy 是会话清理策略。
type retentionPolicy struct {
	// TTL 是无活动会话的保留时长，超期删除；<=0 表示禁用 TTL 清理。
	TTL time.Duration
	// MaxSessions 是保留的最大会话总数（含 running）；超出时从最旧的非 running
	// 会话开始删除。<=0 表示不限制数量。
	MaxSessions int
}

// runRetentionLoop 启动即执行一轮清理，之后每 interval 周期执行。
// 进程生命周期内常驻；清理失败仅记日志，不影响主服务。
func (m *mcpCapture) runRetentionLoop(policy retentionPolicy, interval time.Duration) {
	if policy.TTL <= 0 && policy.MaxSessions <= 0 {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	m.runRetentionSweep(policy)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		m.runRetentionSweep(policy)
	}
}

// runRetentionSweep 执行单轮清理：文件系统层删除 + control.sqlite 元数据同步删除。
func (m *mcpCapture) runRetentionSweep(policy retentionPolicy) {
	deleted := m.sessionMgr.enforceRetention(policy)
	if len(deleted) == 0 {
		return
	}
	// 同步清理 control.sqlite 的会话行：残留行会让 authorizeSession 通过而
	// db_path 指向已删除文件，删除行保持两处一致。
	for _, id := range deleted {
		if m.controlStore != nil {
			if err := m.controlStore.DeleteSession(context.Background(), id); err != nil {
				slog.Warn("retention: delete control store row failed", "session_id", id, "error", err)
			}
		}
	}
	slog.Info("retention sweep completed", "deleted", len(deleted), "ttl", policy.TTL.String(), "max_sessions", policy.MaxSessions)
}

// enforceRetention 在文件系统层执行两阶段清理，返回被删除的会话 ID。
func (sm *sessionManager) enforceRetention(policy retentionPolicy) []string {
	if policy.TTL <= 0 && policy.MaxSessions <= 0 {
		return nil
	}
	sessions, err := sm.listSessions(store.SessionOwnerFilter{AllOwners: true})
	if err != nil {
		slog.Warn("retention: list sessions failed", "error", err)
		return nil
	}
	if len(sessions) == 0 {
		return nil
	}

	now := time.Now()
	pointers := sm.currentPointers()

	// 阶段 1：TTL 清理（按数据写入活跃度，见 sessionActivity 注释）。
	var deleted []string
	var survivors []sessionMetadata
	for _, meta := range sessions {
		age := now.Sub(sm.sessionActivity(meta))
		if policy.TTL > 0 && age > policy.TTL {
			if err := sm.deleteSessionForRetention(meta, pointers); err != nil {
				slog.Warn("retention: delete expired session failed", "session_id", meta.SessionID, "error", err)
				survivors = append(survivors, meta)
				continue
			}
			deleted = append(deleted, meta.SessionID)
			slog.Info("retention: deleted expired session", "session_id", meta.SessionID, "age", age.Round(time.Second))
		} else {
			survivors = append(survivors, meta)
		}
	}

	// 阶段 2：数量上限。survivors 按 started_at 降序，尾部为最旧；
	// running 会话不按数量裁剪（活跃工作交给 TTL 的活跃度判定）。
	if policy.MaxSessions > 0 && len(survivors) > policy.MaxSessions {
		for _, meta := range survivors[policy.MaxSessions:] {
			if meta.Status == "running" {
				continue
			}
			if err := sm.deleteSessionForRetention(meta, pointers); err != nil {
				slog.Warn("retention: delete overflow session failed", "session_id", meta.SessionID, "error", err)
				continue
			}
			deleted = append(deleted, meta.SessionID)
			slog.Info("retention: deleted session over limit", "session_id", meta.SessionID, "started_at", meta.StartedAt)
		}
	}
	return deleted
}

// sessionActivity 返回会话最近一次数据写入时间：
// max(started_at, stopped_at, capture.sqlite mtime, 会话目录 mtime, sessionID 编码时间)。
//
// 以 mtime 为核心信号：抓包运行期间 capture.sqlite 持续更新，真正活跃的会话
// 天然"年轻"；metadata 中的时间用于兜底（如目录被整体拷贝导致 mtime 失真时，
// stopped_at/started_at 仍给出真实时间锚点）。零值表示无法判定（按不会过期处理，
// 由调用方的比较自然短路）。
func (sm *sessionManager) sessionActivity(meta sessionMetadata) time.Time {
	var latest time.Time
	for _, ts := range []string{meta.StartedAt, meta.StoppedAt} {
		if t, err := time.Parse(time.RFC3339, ts); err == nil && t.After(latest) {
			latest = t
		}
	}
	for _, p := range []string{sm.dbPath(meta.SessionID), sm.sessionDir(meta.SessionID)} {
		if info, err := os.Stat(p); err == nil && info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	// 会话 ID 格式为 "20060102_150405.000_XXXX"，仅解析时间戳部分。
	if len(meta.SessionID) >= sessionIDTimestampLength {
		if t, err := time.Parse(sessionIDLayout, meta.SessionID[:sessionIDTimestampLength]); err == nil && t.After(latest) {
			latest = t
		}
	}
	return latest
}

// currentPointers 收集所有 current 分片（current.json / current.<owner>.json），
// 返回 sessionID → 分片文件路径。删除会话时需同步重置指向它的分片，
// 避免留下指向已删目录的悬空指针。
func (sm *sessionManager) currentPointers() map[string]string {
	shards, err := filepath.Glob(filepath.Join(sm.workDir, "current*.json"))
	if err != nil {
		return map[string]string{}
	}
	pointers := make(map[string]string, len(shards))
	for _, shard := range shards {
		data, err := os.ReadFile(shard)
		if err != nil {
			continue
		}
		var meta sessionMetadata
		if json.Unmarshal(data, &meta) != nil || meta.SessionID == "" {
			continue
		}
		pointers[meta.SessionID] = shard
	}
	return pointers
}

// deleteSessionForRetention 删除会话目录并重置指向它的 current 分片。
// 与 deleteSession 的差别：保留策略跨 owner 全量扫描，无法预知 owner，
// 直接按收集到的分片文件路径操作；删除前重新读分片确认仍指向该会话
// （收集与删除之间存在竞争窗口，若分片已被切换则不动它）。
func (sm *sessionManager) deleteSessionForRetention(meta sessionMetadata, pointers map[string]string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if shard, ok := pointers[meta.SessionID]; ok {
		if data, err := os.ReadFile(shard); err == nil {
			var cur sessionMetadata
			if json.Unmarshal(data, &cur) == nil && cur.SessionID == meta.SessionID {
				if err := resetCurrentShard(shard); err != nil {
					return fmt.Errorf("reset current shard: %w", err)
				}
			}
		}
		delete(pointers, meta.SessionID)
	}
	return os.RemoveAll(sm.sessionDir(meta.SessionID))
}

// resetCurrentShard 将 current 分片原子重置为空对象（{}），与 deleteSession 语义一致。
func resetCurrentShard(shard string) error {
	tmp := shard + ".tmp"
	if err := os.WriteFile(tmp, []byte("{}"), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, shard)
}
