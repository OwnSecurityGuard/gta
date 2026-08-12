package semantic

import (
	"fmt"
	"sync"

	"gta/pkg/event"
)

// BaselineStore 维护按上下文隔离的实体基线。
// 实现必须保证：同一 EntityKey 在不同会话/流/账号间互不干扰。
type BaselineStore interface {
	// Get 查询实体基线；不存在时返回 (nil, false)。
	Get(key EntityKey) (*EntityBaseline, bool)
	// Put 写入或更新实体基线。
	Put(base *EntityBaseline)
}

// MemoryBaselineStore 是内存中的实体基线存储。
type MemoryBaselineStore struct {
	mu    sync.RWMutex
	items map[EntityKey]*EntityBaseline
}

// NewMemoryBaselineStore 创建内存基线存储。
func NewMemoryBaselineStore() *MemoryBaselineStore {
	return &MemoryBaselineStore{
		items: make(map[EntityKey]*EntityBaseline),
	}
}

// Get 查询实体基线。
func (m *MemoryBaselineStore) Get(key EntityKey) (*EntityBaseline, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.items[key]
	if !ok {
		return nil, false
	}
	return b, true
}

// Put 写入实体基线。
func (m *MemoryBaselineStore) Put(base *EntityBaseline) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[base.Key] = base
}

// BaselineManager 负责维护实体基线并生成带 before/after 的 StateChange。
type BaselineManager struct {
	store BaselineStore
	mu    sync.Mutex
}

// NewBaselineManager 创建基线管理器。
func NewBaselineManager(store BaselineStore) *BaselineManager {
	if store == nil {
		store = NewMemoryBaselineStore()
	}
	return &BaselineManager{store: store}
}

// Apply 将事件中的 StateChange 与当前基线对比，生成 EnrichedStateChange 并更新基线。
// 无基线时 BeforeResolved 为 false，不会伪造旧值。
func (bm *BaselineManager) Apply(ev *event.Event, sessionID string) ([]EnrichedStateChange, error) {
	if ev == nil {
		return nil, nil
	}

	changes := ev.ExtractStateChanges()
	if len(changes) == 0 {
		return nil, nil
	}

	bm.mu.Lock()
	defer bm.mu.Unlock()

	flowID := ev.Context.FlowID
	if flowID == "" {
		flowID = extractFlowIDFromEvent(ev)
	}

	var result []EnrichedStateChange
	now := ev.Identity.Timestamp

	for _, sc := range changes {
		if err := sc.Validate(); err != nil {
			return nil, fmt.Errorf("invalid state change in event %s: %w", ev.Identity.ID, err)
		}

		key := EntityKey{
			SessionID:   sessionID,
			FlowID:      flowID,
			SubjectType: sc.SubjectType,
			SubjectID:   sc.SubjectID,
		}

		base, exists := bm.store.Get(key)
		if !exists {
			base = &EntityBaseline{
				Key:        key,
				Version:    0,
				State:      make(map[string]event.Value),
				FirstSeen:  now,
				LastSeen:   now,
				HasHistory: false,
			}
		}

		enriched := EnrichedStateChange{
			StateChange:    sc,
			EventID:        ev.Identity.ID,
			FlowID:         flowID,
			Timestamp:      now,
			EntityVersion:  sc.Version,
			BeforeResolved: false,
			AfterResolved:  false,
		}

		// 按 path 记录 before：只有基线存在且该 path 有历史时才 resolved
		if exists && base.HasHistory {
			if oldVal, ok := base.State[sc.Path]; ok {
				enriched.Before = oldVal
				enriched.BeforeResolved = true
			}
		}

		// 更新基线：after 非空时写入
		if sc.After.Kind != event.Null {
			base.State[sc.Path] = sc.After
			base.LastSeen = now
			base.HasHistory = true
			if sc.Version > base.Version {
				base.Version = sc.Version
			}
			enriched.AfterResolved = true
			bm.store.Put(base)
		}

		result = append(result, enriched)
	}

	return result, nil
}

// extractFlowIDFromEvent 从 Event 提取 flow_id：优先 Context，其次 _meta，最后顶层 payload。
func extractFlowIDFromEvent(ev *event.Event) string {
	if ev == nil {
		return ""
	}
	if ev.Context.FlowID != "" {
		return ev.Context.FlowID
	}
	obj, ok := ev.Payload.Value.AsObject()
	if !ok {
		return ""
	}
	if meta, ok := obj["_meta"]; ok {
		if metaObj, ok := meta.AsObject(); ok {
			if v, ok := metaObj["flow_id"]; ok {
				if s, ok := v.AsString(); ok {
					return s
				}
			}
		}
	}
	if v, ok := obj["flow_id"]; ok {
		if s, ok := v.AsString(); ok {
			return s
		}
	}
	return ""
}
