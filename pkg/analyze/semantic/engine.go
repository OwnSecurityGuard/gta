package semantic

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gta/pkg/event"
)

// requestKey 用于在同流内匹配请求与响应。
type requestKey struct {
	FlowID         string
	CorrelationKey string
}

// pendingRequest 记录一个尚未收到响应的请求事件。
type pendingRequest struct {
	EventID   event.EventID
	EventType string
	Timestamp time.Time
}

// lastEventInfo 记录某 flow 的最近事件及其时间戳。
type lastEventInfo struct {
	ID        event.EventID
	Timestamp time.Time
}

// Engine 是链路规则语义分析引擎。
//
// 线程安全：Process 可被并发调用；内部使用锁保护图与状态。
type Engine struct {
	config       Config
	baseline     *BaselineManager
	graph        *EvidenceGraph
	mu           sync.Mutex
	pendingReqs  map[requestKey]*pendingRequest
	lastEvents   map[string]lastEventInfo // flow_id -> 最近事件
	rawPacketIds map[string]string        // raw_packet_id -> 节点 ID
	entityIds    map[EntityKey]string     // entity key -> 节点 ID
	eventIds     map[event.EventID]string // event id -> 节点 ID

	// nodeIDs 是图中所有已存在节点 ID 的集合，用于在建边时强制校验
	// Graph Integrity 不变量：EvidenceEdge.Source / Target 必须存在于 Nodes。
	nodeIDs map[string]struct{}

	// projector 是确定性语义投影器，用于为事件节点填充 Semantic（Phase 2 投影结果）。
	projector *SemanticProjector

	// 时间聚类状态
	activeTransactions map[string]*activeTransaction // flow_id -> 当前活跃事务
	transactionSeq     int64                         // 事务序号计数器
}

// activeTransaction 记录一个尚未关闭的事务组。
type activeTransaction struct {
	ID        string
	FlowID    string
	StartTime time.Time
	LastTime  time.Time
	EventIDs  []event.EventID
	NodeIDs   []string
	Count     int
}

// NewEngine 创建语义分析引擎。
func NewEngine(config Config, baseline *BaselineManager) *Engine {
	if baseline == nil {
		baseline = NewBaselineManager(nil)
	}
	return &Engine{
		config:             config,
		baseline:           baseline,
		graph:              &EvidenceGraph{},
		pendingReqs:        make(map[requestKey]*pendingRequest),
		lastEvents:         make(map[string]lastEventInfo),
		rawPacketIds:       make(map[string]string),
		entityIds:          make(map[EntityKey]string),
		eventIds:           make(map[event.EventID]string),
		nodeIDs:            make(map[string]struct{}),
		activeTransactions: make(map[string]*activeTransaction),
		projector:          NewSemanticProjector(),
	}
}

// Process 处理单个事件，更新基线并生成语义关系。
func (e *Engine) Process(ev *event.Event) ([]EnrichedStateChange, error) {
	if ev == nil {
		return nil, nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// 1. 建立事件节点与 decoded_from 关系
	e.ensureEventNode(ev)
	e.linkDecodedFrom(ev)

	// 2. 处理 StateChange，更新基线，建立 updates 关系
	enriched, err := e.baseline.Apply(ev, ev.Identity.SessionID)
	if err != nil {
		return nil, err
	}
	for _, esc := range enriched {
		e.ensureStateChangeNode(ev, esc)
	}

	// 3. 处理 response_to / correlated_with
	e.linkResponseOrCorrelation(ev)

	// 4. 处理 possible_followup（低置信时间邻近）
	e.linkPossibleFollowup(ev)

	// 5. 记录为 flow 最近事件
	if ev.Context.FlowID != "" {
		e.lastEvents[ev.Context.FlowID] = lastEventInfo{
			ID:        ev.Identity.ID,
			Timestamp: ev.Identity.Timestamp,
		}
	}

	// 6. 时间聚类：将事件归入事务组
	e.clusterTransaction(ev)

	return enriched, nil
}

// Graph 返回当前累积的证据图副本。
func (e *Engine) Graph() *EvidenceGraph {
	e.mu.Lock()
	defer e.mu.Unlock()

	// 关闭所有剩余活跃事务
	if e.config.TransactionClustering != nil {
		for _, tx := range e.activeTransactions {
			if tx.Count > 0 {
				e.finalizeTransaction(tx)
			}
		}
		e.activeTransactions = make(map[string]*activeTransaction)
	}

	out := &EvidenceGraph{
		Nodes:         make([]EvidenceNode, len(e.graph.Nodes)),
		Edges:         make([]EvidenceEdge, len(e.graph.Edges)),
		Uncertainties: append([]string(nil), e.graph.Uncertainties...),
	}
	copy(out.Nodes, e.graph.Nodes)
	copy(out.Edges, e.graph.Edges)
	return out
}

// addNode 将节点写入图并登记到 nodeIDs 集合。
//
// 所有节点都必须经由此方法加入图，否则 addEdgeFromNode 的完整性校验会
// 把指向它的边判为悬空并丢弃。
func (e *Engine) addNode(node EvidenceNode) string {
	e.graph.Nodes = append(e.graph.Nodes, node)
	e.nodeIDs[node.ID] = struct{}{}
	return node.ID
}

// eventNodeID 返回事件在证据图中的节点 ID。
//
// 节点 ID 的构造是确定性的（evt_<event_id>）。集中封装在此处，避免调用方
// 各自拼接、或误把裸 event.EventID 当作节点 ID 传给 addEdge/addEdgeFromNode，
// 从而产生 Target 无法在 Nodes 中找到的悬空边。
func eventNodeID(id event.EventID) string {
	return fmt.Sprintf("evt_%s", id)
}

// resolveEventNode 查找某事件已注册的图节点 ID。
//
// Graph Integrity 不变量：EvidenceEdge.Source / EvidenceEdge.Target 必须能在
// EvidenceGraph.Nodes 中找到。因此建边前必须确认目标事件节点确实已创建；
// 若未创建则返回 false，调用方必须放弃建边而不是拼一个可能不存在的 ID。
func (e *Engine) resolveEventNode(id event.EventID) (string, bool) {
	nodeID, ok := e.eventIds[id]
	return nodeID, ok
}

// ensureEventNode 确保存在对应的事件节点。
func (e *Engine) ensureEventNode(ev *event.Event) string {
	if id, ok := e.eventIds[ev.Identity.ID]; ok {
		return id
	}
	semantic := e.projector.Project(ev)
	node := EvidenceNode{
		ID:        eventNodeID(ev.Identity.ID),
		Kind:      NodeEvent,
		SessionID: ev.Identity.SessionID,
		FlowID:    ev.Context.FlowID,
		Timestamp: ev.Identity.Timestamp,
		Labels: map[string]string{
			"type":      string(ev.Identity.Type),
			"schema_id": ev.Identity.SchemaID,
			"source":    string(ev.Identity.Source),
		},
		Properties: map[string]any{
			"event_id":  string(ev.Identity.ID),
			"direction": ev.Context.Direction,
		},
		// Semantic 由 Phase 2 确定性投影器填充（不修改 Event，纯投影）。
		Semantic: &semantic,
	}
	e.addNode(node)
	e.eventIds[ev.Identity.ID] = node.ID
	return node.ID
}

// linkDecodedFrom 建立事件到原始包的 decoded_from 边。
func (e *Engine) linkDecodedFrom(ev *event.Event) {
	rawID := ev.Context.RawPacketID
	if rawID == "" {
		return
	}

	nodeID, ok := e.rawPacketIds[rawID]
	if !ok {
		node := EvidenceNode{
			ID:        fmt.Sprintf("pkt_%s", rawID),
			Kind:      NodeRawPacket,
			SessionID: ev.Identity.SessionID,
			FlowID:    ev.Context.FlowID,
			Timestamp: ev.Identity.Timestamp,
			Labels: map[string]string{
				"raw_packet_id": rawID,
			},
		}
		nodeID = e.addNode(node)
		e.rawPacketIds[rawID] = nodeID
	}

	e.addEdge(ev.Identity.ID, nodeID, DecodedFrom, 1.0,
		fmt.Sprintf("event decoded from raw packet %s", rawID),
		map[string]any{"raw_packet_id": rawID},
		edgeMeta{Strength: EvidenceObserved, Method: MethodPlugin, EvidenceIDs: []string{rawID}})
}

// ensureStateChangeNode 确保存在 StateChange 节点并建立与实体、事件的关系。
func (e *Engine) ensureStateChangeNode(ev *event.Event, esc EnrichedStateChange) {
	scNodeID := fmt.Sprintf("sc_%s_%s_%s", ev.Identity.ID, esc.SubjectType, esc.Path)
	scNode := EvidenceNode{
		ID:        scNodeID,
		Kind:      NodeStateChange,
		SessionID: ev.Identity.SessionID,
		FlowID:    ev.Context.FlowID,
		Timestamp: ev.Identity.Timestamp,
		Labels: map[string]string{
			"subject_type": esc.SubjectType,
			"subject_id":   esc.SubjectID,
			"op":           esc.Op,
			"path":         esc.Path,
		},
		Properties: map[string]any{
			"before_resolved": esc.BeforeResolved,
			"after_resolved":  esc.AfterResolved,
			"version":         esc.EntityVersion,
		},
	}
	e.addNode(scNode)

	// state_change -> event (caused_by)
	// 目标必须是事件节点 ID（evt_<id>），不能是裸 EventID，否则形成悬空边。
	if evNodeID, ok := e.resolveEventNode(ev.Identity.ID); ok {
		e.addEdgeFromNode(scNodeID, evNodeID, CausedBy, 1.0,
			"state change caused by event", nil,
			edgeMeta{Strength: EvidenceObserved, Method: MethodStateProjection, EvidenceIDs: []string{string(ev.Identity.ID)}})
	} else {
		slog.Warn("semantic engine: skip caused_by edge, event node missing", "event_id", ev.Identity.ID)
	}

	// 实体节点
	key := EntityKey{
		SessionID:   ev.Identity.SessionID,
		FlowID:      ev.Context.FlowID,
		SubjectType: esc.SubjectType,
		SubjectID:   esc.SubjectID,
	}
	entityNodeID, ok := e.entityIds[key]
	if !ok {
		entityNode := EvidenceNode{
			ID:        fmt.Sprintf("ent_%s_%s_%s_%s", key.SessionID, key.FlowID, key.SubjectType, key.SubjectID),
			Kind:      NodeEntity,
			SessionID: key.SessionID,
			FlowID:    key.FlowID,
			Timestamp: ev.Identity.Timestamp,
			Labels: map[string]string{
				"subject_type": key.SubjectType,
				"subject_id":   key.SubjectID,
			},
		}
		entityNodeID = e.addNode(entityNode)
		e.entityIds[key] = entityNodeID
	}

	// state_change -> entity (updates)
	e.addEdgeFromNode(scNodeID, entityNodeID, Updates, 1.0,
		fmt.Sprintf("%s %s = %v", esc.Op, esc.Path, esc.After.ToAny()),
		map[string]any{
			"path":            esc.Path,
			"op":              esc.Op,
			"before_resolved": esc.BeforeResolved,
			"after_resolved":  esc.AfterResolved,
			"entity_version":  esc.EntityVersion,
		},
		edgeMeta{Strength: EvidenceDerived, Method: MethodStateProjection, EvidenceIDs: []string{string(ev.Identity.ID)}})
}

// linkResponseOrCorrelation 处理 response_to 与 correlated_with。
//
// 匹配优先级：
//  1. 同 flow 内 correlation_key 精确匹配 → response_to（置信度 1.0）
//  2. 同 flow 内消息命名模式匹配（如 *Req→*Resp）→ response_to（置信度 0.85）
//  3. 无匹配且 server_to_client 带 correlation_key → uncertainty
func (e *Engine) linkResponseOrCorrelation(ev *event.Event) {
	flowID := ev.Context.FlowID
	correlationKey := ev.Relation.CorrelationID
	if flowID == "" {
		return
	}

	// --- 响应方向 ---
	if ev.Context.Direction == "server_to_client" {
		// 策略 1：correlation_key 精确匹配
		if correlationKey != "" {
			key := requestKey{FlowID: flowID, CorrelationKey: correlationKey}
			if req, ok := e.pendingReqs[key]; ok {
				reqNodeID, found := e.resolveEventNode(req.EventID)
				if !found {
					slog.Warn("semantic engine: skip response_to edge, request node missing", "request_event_id", req.EventID)
					delete(e.pendingReqs, key)
					return
				}
				e.addEdge(ev.Identity.ID, reqNodeID, ResponseTo, 1.0,
					fmt.Sprintf("response matched request by correlation_key=%s", correlationKey),
					map[string]any{"correlation_key": correlationKey, "match_method": "correlation_key"},
					edgeMeta{Strength: EvidenceDerived, Method: MethodCorrelation, RuleID: "correlation_key", EvidenceIDs: []string{string(req.EventID), string(ev.Identity.ID)}})
				delete(e.pendingReqs, key)
				return
			}
			// 未匹配到：记录 uncertainty
			e.graph.Uncertainties = append(e.graph.Uncertainties,
				fmt.Sprintf("server_to_client event %s has correlation_key=%s but no pending request", ev.Identity.ID, correlationKey))
			return
		}

		// 策略 2：无 correlation_key，尝试消息命名模式匹配
		if matched := e.linkResponseByPattern(ev, flowID); matched {
			return
		}
		return
	}

	// --- 请求方向：记录 pending request ---
	if ev.Context.Direction == "client_to_server" {
		reqType := string(ev.Identity.Type)
		if correlationKey != "" {
			key := requestKey{FlowID: flowID, CorrelationKey: correlationKey}
			e.pendingReqs[key] = &pendingRequest{
				EventID:   ev.Identity.ID,
				EventType: reqType,
				Timestamp: ev.Identity.Timestamp,
			}
		} else {
			// 无 correlation_key，存入无 key 队列供模式匹配使用
			e.pendingReqs[requestKey{FlowID: flowID}] = &pendingRequest{
				EventID:   ev.Identity.ID,
				EventType: reqType,
				Timestamp: ev.Identity.Timestamp,
			}
		}
	}
}

// linkResponseByPattern 尝试通过消息命名模式匹配请求-响应。
// 当 server_to_client 事件无 correlation_key 时，尝试将 pending client_to_server 事件的类型
// 按命名模式转换为响应类型，若匹配则建立 response_to 边。
//
// 返回 true 表示成功匹配。
func (e *Engine) linkResponseByPattern(ev *event.Event, flowID string) bool {
	respType := string(ev.Identity.Type)

	// 收集该 flow 上所有无 correlation_key 的 pending 请求（按时间倒序）
	key := requestKey{FlowID: flowID}
	req, ok := e.pendingReqs[key]
	if !ok {
		return false
	}

	reqNodeID, found := e.resolveEventNode(req.EventID)
	if !found {
		slog.Warn("semantic engine: skip pattern response_to edge, request node missing", "request_event_id", req.EventID)
		return false
	}

	// 尝试每个命名模式
	for _, pat := range e.config.ResponseNamePatterns {
		predicted := trySwapSuffix(req.EventType, pat.RequestSuffix, pat.ResponseSuffix)
		if predicted == respType {
			e.addEdge(ev.Identity.ID, reqNodeID, ResponseTo, 0.85,
				fmt.Sprintf("response matched request by naming pattern: %s→%s", req.EventType, respType),
				map[string]any{"match_method": "naming_pattern", "request_type": req.EventType, "response_type": respType},
				edgeMeta{Strength: EvidenceDerived, Method: MethodNamePattern, RuleID: fmt.Sprintf("name_pattern:%s→%s", pat.RequestSuffix, pat.ResponseSuffix), EvidenceIDs: []string{string(req.EventID), string(ev.Identity.ID)}})
			delete(e.pendingReqs, key)
			return true
		}
	}
	return false
}

// trySwapSuffix 尝试将 s 的 old 部分替换为 new。
// 先检查后缀（如 "Req"→"Resp"），再检查前缀（如 "CS"→"SC"）。
// 返回替换后的字符串；若不匹配则返回空字符串。
func trySwapSuffix(s, old, new string) string {
	if s == "" {
		return ""
	}
	if strings.HasSuffix(s, old) {
		return s[:len(s)-len(old)] + new
	}
	if strings.HasPrefix(s, old) {
		return new + s[len(old):]
	}
	return ""
}

// linkPossibleFollowup 与同一 flow 的上一个事件建立低置信时间邻近关系。
func (e *Engine) linkPossibleFollowup(ev *event.Event) {
	flowID := ev.Context.FlowID
	if flowID == "" {
		return
	}
	last, ok := e.lastEvents[flowID]
	if !ok || last.ID == ev.Identity.ID {
		return
	}
	delta := ev.Identity.Timestamp.Sub(last.Timestamp)
	if delta < 0 {
		delta = -delta
	}
	if delta > e.config.PossibleFollowupWindow {
		return
	}
	lastNodeID, found := e.resolveEventNode(last.ID)
	if !found {
		slog.Warn("semantic engine: skip possible_followup edge, previous event node missing", "event_id", last.ID)
		return
	}
	e.addEdge(ev.Identity.ID, lastNodeID, PossibleFollowup, 0.3,
		"time-adjacent event in same flow (low confidence)",
		map[string]any{"time_delta_ms": delta.Milliseconds()},
		edgeMeta{Strength: EvidenceInferred, Method: MethodTemporal, EvidenceIDs: []string{string(last.ID), string(ev.Identity.ID)}})
}

// edgeMeta 携带 EvidenceEdge 的 v1 结构化字段（Strength/Method/RuleID/EvidenceIDs），
// 与 Confidence（判定可信度）分离，保证 Evidence 可解释、可溯源。
type edgeMeta struct {
	Strength    EvidenceStrength
	Method      EvidenceMethod
	RuleID      string
	EvidenceIDs []string
}

// addEdge 添加一条从事件节点出发的有向边。
func (e *Engine) addEdge(sourceEventID event.EventID, targetNodeID string, edgeType RelationType, confidence float64, reason string, props map[string]any, meta edgeMeta) {
	sourceNodeID, ok := e.eventIds[sourceEventID]
	if !ok {
		slog.Warn("semantic engine: source event node not found", "event_id", sourceEventID)
		return
	}
	e.addEdgeFromNode(sourceNodeID, targetNodeID, edgeType, confidence, reason, props, meta)
}

// resolveEvidenceID 校验一个 EvidenceID 是否指向图中真实存在的证据。
//
// v1 规定：EvidenceEdge.EvidenceIDs 只能引用图中真实存在的实体，否则 Agent 看到
// evidence_ids 却无法追溯（与悬空端点同为"静默不可信"问题）。合法引用包括：
//   - 图中已存在的节点 ID（直接形式，如 evt_xxx / pkt_xxx / sc_xxx / ent_xxx / txn_xxx）
//   - 裸事件 ID（eventIds 登记的事件，对应 evt_<id> 节点）
//   - 裸原始包 ID（rawPacketIds 登记的原始包，对应 pkt_<id> 节点）
//
// 无法解析的 EvidenceID 视为悬空证据引用，由 addEdgeFromNode 在建边时丢弃该边。
func (e *Engine) resolveEvidenceID(id string) bool {
	if id == "" {
		return false
	}
	if _, ok := e.nodeIDs[id]; ok {
		return true
	}
	if _, ok := e.eventIds[event.EventID(id)]; ok {
		return true
	}
	if _, ok := e.rawPacketIds[id]; ok {
		return true
	}
	return false
}

// addEdgeFromNode 添加一条有向边。
//
// Graph Integrity 不变量在此强制执行，分两层：
//  1. 端点层：source 与 target 都必须是图中已存在的节点 ID，否则这条边会被丢弃。
//  2. 证据引用层：meta.EvidenceIDs 中每个 ID 都必须能在图中解析到真实存在的
//     事件/原始包/节点，否则这条边同样被丢弃（明确降级）并记录 warn。
//
// 两层都保证了：即使未来新增建边逻辑时误传了裸 EventID、未创建的节点或凭空捏造的
// evidence_id，也不会污染证据图（trace_event_chain / BFS / UI 渲染都依赖端点与证据可达）。
func (e *Engine) addEdgeFromNode(sourceNodeID, targetNodeID string, edgeType RelationType, confidence float64, reason string, props map[string]any, meta edgeMeta) {
	if _, ok := e.nodeIDs[sourceNodeID]; !ok {
		slog.Warn("semantic engine: drop edge, source node not in graph",
			"source", sourceNodeID, "target", targetNodeID, "type", edgeType)
		return
	}
	if _, ok := e.nodeIDs[targetNodeID]; !ok {
		slog.Warn("semantic engine: drop edge, target node not in graph",
			"source", sourceNodeID, "target", targetNodeID, "type", edgeType)
		return
	}
	for _, evID := range meta.EvidenceIDs {
		if !e.resolveEvidenceID(evID) {
			slog.Warn("semantic engine: drop edge, evidence_id not resolvable",
				"source", sourceNodeID, "target", targetNodeID, "type", edgeType, "evidence_id", evID)
			return
		}
	}
	edge := EvidenceEdge{
		ID:          fmt.Sprintf("edge_%s_%s_%s", sourceNodeID, targetNodeID, edgeType),
		Source:      sourceNodeID,
		Target:      targetNodeID,
		Type:        edgeType,
		Confidence:  confidence,
		Reason:      reason,
		Properties:  props,
		Strength:    meta.Strength,
		Method:      meta.Method,
		RuleID:      meta.RuleID,
		EvidenceIDs: meta.EvidenceIDs,
	}
	e.graph.Edges = append(e.graph.Edges, edge)
}

// clusterTransaction 将事件按时间聚类归入事务组。
//
// 规则：
//   - 每个 client_to_server 事件触发新事务（NewTransactionOnRequest=true）
//   - 若与上一个事务间隔 < MergeGap，合并到同一事务
//   - 非请求事件加入当前活跃事务
//   - 事务关闭时将 Transaction 节点与 contains 边写入证据图
func (e *Engine) clusterTransaction(ev *event.Event) {
	cfg := e.config.TransactionClustering
	if cfg == nil {
		return
	}

	flowID := ev.Context.FlowID
	if flowID == "" {
		return
	}

	now := ev.Identity.Timestamp
	eventNodeID, ok := e.eventIds[ev.Identity.ID]
	if !ok {
		return
	}

	active, exists := e.activeTransactions[flowID]

	isRequest := ev.Context.Direction == "client_to_server"

	if cfg.NewTransactionOnRequest && isRequest {
		if exists && active.Count > 0 {
			// 检查是否需要合并
			if cfg.MergeGap > 0 && now.Sub(active.LastTime) < cfg.MergeGap {
				// 合并到当前事务
				active.LastTime = now
				active.EventIDs = append(active.EventIDs, ev.Identity.ID)
				active.NodeIDs = append(active.NodeIDs, eventNodeID)
				active.Count++
				return
			}
			// 关闭当前事务
			e.finalizeTransaction(active)
		}
		// 开启新事务
		e.transactionSeq++
		txID := fmt.Sprintf("txn_%s_%d", flowID, e.transactionSeq)
		e.activeTransactions[flowID] = &activeTransaction{
			ID:        txID,
			FlowID:    flowID,
			StartTime: now,
			LastTime:  now,
			EventIDs:  []event.EventID{ev.Identity.ID},
			NodeIDs:   []string{eventNodeID},
			Count:     1,
		}
		return
	}

	// 非请求事件：加入当前活跃事务
	if exists {
		active.LastTime = now
		active.EventIDs = append(active.EventIDs, ev.Identity.ID)
		active.NodeIDs = append(active.NodeIDs, eventNodeID)
		active.Count++
		return
	}

	// 无活跃事务，开启一个
	e.transactionSeq++
	txID := fmt.Sprintf("txn_%s_%d", flowID, e.transactionSeq)
	e.activeTransactions[flowID] = &activeTransaction{
		ID:        txID,
		FlowID:    flowID,
		StartTime: now,
		LastTime:  now,
		EventIDs:  []event.EventID{ev.Identity.ID},
		NodeIDs:   []string{eventNodeID},
		Count:     1,
	}
}

// finalizeTransaction 关闭一个事务组，写入 Transaction 节点与 contains 边到证据图。
func (e *Engine) finalizeTransaction(tx *activeTransaction) {
	if tx.Count == 0 {
		return
	}

	// 创建 Transaction 节点
	txNode := EvidenceNode{
		ID:        tx.ID,
		Kind:      NodeTransaction,
		SessionID: "", // 从第一个事件推导，实际由上层 session 限定
		FlowID:    tx.FlowID,
		Timestamp: tx.StartTime,
		Labels: map[string]string{
			"flow_id":     tx.FlowID,
			"event_count": fmt.Sprintf("%d", tx.Count),
			"first_event": string(tx.EventIDs[0]),
			"last_event":  string(tx.EventIDs[tx.Count-1]),
		},
		Properties: map[string]any{
			"event_ids":   tx.EventIDs,
			"start_time":  tx.StartTime,
			"last_time":   tx.LastTime,
			"duration_ms": tx.LastTime.Sub(tx.StartTime).Milliseconds(),
		},
	}
	e.addNode(txNode)

	// 创建 contains 边：Transaction → each event node
	for i, nodeID := range tx.NodeIDs {
		e.addEdgeFromNode(tx.ID, nodeID, Contains, 0.95,
			fmt.Sprintf("event %s belongs to transaction %s", nodeID, tx.ID),
			map[string]any{"transaction_id": tx.ID},
			edgeMeta{Strength: EvidenceDerived, Method: MethodTransaction, EvidenceIDs: []string{string(tx.EventIDs[i])}})
	}
}
