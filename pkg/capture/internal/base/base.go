// Package base 提供 capture.Source 实现通用的生命周期状态机与统计骨架，
// 避免每个 Source 子包重复维护 state/err/startOnce/closeOnce/ctx/wg/stats。
package base

import (
	"context"
	"sync"
	"sync/atomic"

	"gta/pkg/capture"
)

// Lifecycle 管理 Source 的公共生命周期：状态机、单次启动/关闭、context 与 waitgroup。
type Lifecycle struct {
	state     atomic.Int32
	err       atomic.Value
	closeOnce sync.Once
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// Start 执行一次性启动：
//   - setup 中完成资源初始化（如打开网卡/文件），失败时返回错误；
//   - run 在独立的 goroutine 中执行，返回时 Lifecycle 会自动把状态置为 Closed。
//
// 使用 CAS 保证仅第一次调用能成功；后续调用（包括 Source 已关闭后）均返回
// capture.ErrAlreadyStarted。
func (l *Lifecycle) Start(parent context.Context, setup func() error, run func(ctx context.Context)) error {
	if !l.state.CompareAndSwap(int32(capture.StateCreated), int32(capture.StateRunning)) {
		return capture.ErrAlreadyStarted
	}

	l.ctx, l.cancel = context.WithCancel(parent)
	if err := setup(); err != nil {
		l.state.Store(int32(capture.StateClosed))
		l.cancel()
		l.cancel = nil
		return err
	}

	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		run(l.ctx)
		l.state.Store(int32(capture.StateClosed))
		if l.cancel != nil {
			l.cancel()
		}
	}()

	return nil
}

// Close 执行一次性关闭：设置状态、cancel context、等待 run goroutine 退出。
func (l *Lifecycle) Close() error {
	l.closeOnce.Do(func() {
		l.state.Store(int32(capture.StateClosed))
		if l.cancel != nil {
			l.cancel()
		}
		l.wg.Wait()
	})
	return nil
}

// State 返回当前生命周期状态。
func (l *Lifecycle) State() capture.State { return capture.State(l.state.Load()) }

// Context 返回内部 context（仅应在 run goroutine 中使用）。
func (l *Lifecycle) Context() context.Context { return l.ctx }

// SetErr 保存运行期错误，供 Err() 返回。
func (l *Lifecycle) SetErr(err error) { l.err.Store(err) }

// Err 返回运行期错误。
func (l *Lifecycle) Err() error {
	v := l.err.Load()
	if v == nil {
		return nil
	}
	return v.(error)
}

// StatTracker 是 capture.Stats 的线程安全包装，供各 Source 复用。
type StatTracker struct {
	mu    sync.RWMutex
	stats capture.Stats
}

// Init 初始化 Extra map，应在 Source 构造时调用。
func (s *StatTracker) Init() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Extra = make(map[string]any)
}

// AddIn 累计收到的包数与字节数。
func (s *StatTracker) AddIn(bytes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.PacketsIn++
	s.stats.BytesIn += uint64(bytes)
}

// SetDrops 设置内核/驱动层丢包数（来自 pcap.Handle.Stats()）。
// 用 Set 而非 Add：pcap 统计是累计值，每次读取覆盖前值即可。
func (s *StatTracker) SetDrops(drops uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Drops = drops
}

// AddBlocked 累计因 channel 背压而阻塞的次数。
// 用于评估消费速度是否跟上抓包速度，配合 capture_quality 指标使用。
func (s *StatTracker) AddBlocked() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Blocked++
}

// SetExtra 设置 source 专有指标。
func (s *StatTracker) SetExtra(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stats.Extra == nil {
		s.stats.Extra = make(map[string]any)
	}
	s.stats.Extra[key] = value
}

// Stats 返回当前统计值的副本。
func (s *StatTracker) Stats() capture.Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	extraCopy := make(map[string]any, len(s.stats.Extra))
	for k, v := range s.stats.Extra {
		extraCopy[k] = v
	}
	cp := s.stats
	cp.Extra = extraCopy
	return cp
}
