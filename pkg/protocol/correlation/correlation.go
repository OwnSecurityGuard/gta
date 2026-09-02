// Package correlation 提供 Request/Response 关联的待处理请求跟踪。
//
// 职责：当消息解析出 correlation（方向为 request）时，"记住"它以待后续响应匹配；
// 当解析出方向为 response 时，用相同关联键"查询"之前的请求，从而定位其前驱事件。
//
// 键由调用方构造（通常为 flowID + rule + value），以隔离不同连接、不同规则的 seq 空间。
package correlation

import (
	"sync"
	"time"
)

// Pending 表示一条已记住、等待响应匹配的请求。
type Pending struct {
	// CausationID 是请求事件的事件 ID，供响应事件设置 as 前驱。
	CausationID string
	// Timestamp 是记住时间，用于过期回收。
	Timestamp time.Time
}

// Store 保存按关联键索引的待匹配请求。
// 并发安全。
type Store struct {
	mu      sync.Mutex
	pending map[string]Pending
	order   []string
	limit   int // 最多记住的请求数；超限时淘汰最旧
}

// New 创建关联存储，limit<=0 时退化为默认 1024。
func New(limit int) *Store {
	if limit <= 0 {
		limit = 1024
	}
	return &Store{
		pending: make(map[string]Pending),
		limit:   limit,
	}
}

// Remember 记录一条待匹配请求 (key → causationID)。
// 已存在同 key 时覆盖（后到者胜）。
func (s *Store) Remember(key, causationID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pending[key]; !ok {
		s.order = append(s.order, key)
	}
	s.pending[key] = Pending{CausationID: causationID, Timestamp: time.Now()}
	for len(s.order) > s.limit {
		old := s.order[0]
		s.order = s.order[1:]
		delete(s.pending, old)
	}
}

// Lookup 查询匹配到的请求。不删除条目——一次请求可能对应多条响应（如 notify）。
func (s *Store) Lookup(key string) (Pending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[key]
	return p, ok
}

// Forget 主动移除指定键（请求完成且不再需要匹配时调用）。
func (s *Store) Forget(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pending[key]; !ok {
		return
	}
	delete(s.pending, key)
	for i, k := range s.order {
		if k == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// Len 返回当前待匹配请求数量。
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}
