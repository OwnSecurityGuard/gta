package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"gta/pkg/event"
)

// TimelineProtocol 是 _meta.protocol 中投影的通信语义（Protocol Behavior Resolver 产出）。
// 它与原始业务 JSON 并存：有语义则增强展示，无语义则前端自动降级为普通 JSON 事件。
type TimelineProtocol struct {
	Message     string               `json:"message,omitempty"`
	Role        string               `json:"role,omitempty"` // request | response | push | unknown
	Delivery    string               `json:"delivery,omitempty"`
	Correlation *TimelineCorrelation `json:"correlation,omitempty"`
	Error       *TimelineError       `json:"error,omitempty"`
}

// TimelineCorrelation 描述一条消息的 Request/Response 关联。
type TimelineCorrelation struct {
	Direction string `json:"direction,omitempty"` // request | response
	Rule      string `json:"rule,omitempty"`
	Key       string `json:"key,omitempty"`
	Value     string `json:"value,omitempty"`
}

// TimelineError 描述一条消息的错误语义。
type TimelineError struct {
	Failed bool   `json:"failed"`
	Code   string `json:"code,omitempty"`
}

// TimelineNode 是会话时间线树的一个节点，对应一条已解码事件。
// 父子关系来自 TraceContext.CausationID（OpenTelemetry 的 parent span id）；
// 同一 correlation_id 的事件聚合为一个"会话/对话"（request/response 分组）。
// Children 使用指针，便于在单次遍历中无损挂载子树，序列化时由 encoding/json 自动解引用。
type TimelineNode struct {
	ID            string            `json:"id"`
	Timestamp     time.Time         `json:"timestamp"`
	SchemaID      string            `json:"schema_id,omitempty"`
	Type          string            `json:"type,omitempty"`
	MsgName       string            `json:"msg_name,omitempty"`
	Direction     string            `json:"direction,omitempty"`
	CorrelationID string            `json:"correlation_id,omitempty"`
	IsPush        bool              `json:"is_push,omitempty"`
	Proto         *TimelineProtocol `json:"proto,omitempty"`
	JSON          string            `json:"json,omitempty"` // 干净业务 JSON（不含 _meta），供 Raw JSON 视图
	Summary       string            `json:"summary,omitempty"`
	Children      []*TimelineNode   `json:"children,omitempty"`
}

// timelineProtocol 从事件 _meta.protocol 读取通信语义；不存在或不可解析时返回 nil。
func timelineProtocol(ev *event.Event) *TimelineProtocol {
	v, ok := ev.MetaValue("protocol")
	if !ok {
		return nil
	}
	b, err := v.ToJSON()
	if err != nil {
		return nil
	}
	var p TimelineProtocol
	if err := json.Unmarshal(b, &p); err != nil {
		return nil
	}
	return &p
}

// ConversationView 是同一 correlation_id 下事件的聚合视图。
type ConversationView struct {
	CorrelationID string `json:"correlation_id"`
	EventCount    int    `json:"event_count"`
}

// SessionTimeline 是 get_session_timeline 的完整输出。
type SessionTimeline struct {
	SessionID     string             `json:"session_id"`
	Plugin        string             `json:"plugin,omitempty"`
	Status        string             `json:"status,omitempty"`
	EventCount    int                `json:"event_count"`
	RootCount     int                `json:"root_count"`
	Conversations []ConversationView `json:"conversations,omitempty"`
	Roots         []TimelineNode     `json:"roots"`
	Uncertainties []string           `json:"uncertainties,omitempty"`
}

// handleGetSessionTimeline 构建一次抓包会话的完整时序树（request/response 拓扑）。
//
// 与 trace_protocol_flow（基于 run + flow 的细粒度执行链路）不同，本工具面向整 session，
// 直接从 events 表按 TraceContext 组装因果树，是"抓一次游戏：看到完整流程"的 MVP 视图。
func (m *mcpCapture) handleGetSessionTimeline(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID := req.GetString("session_id", "")
	if sessionID == "" {
		return errorResult(fmt.Errorf("session_id is required")), nil
	}

	limit := req.GetInt("limit", 500)
	if limit <= 0 {
		limit = 500
	}
	if limit > 5000 {
		limit = 5000
	}
	offset := req.GetInt("offset", 0)
	if offset < 0 {
		offset = 0
	}

	reader, err := m.openReader(ctx, sessionID)
	if err != nil {
		return errorResult(fmt.Errorf("open reader: %w", err)), nil
	}
	defer reader.Close()

	events, err := reader.QueryEvents(ctx, sessionID, limit, offset)
	if err != nil {
		return errorResult(fmt.Errorf("query events: %w", err)), nil
	}

	var uncertainties []string
	if len(events) >= limit {
		uncertainties = append(uncertainties,
			fmt.Sprintf("event window capped at limit=%d; tree may be partial, narrow with offset or use list_decoded_data", limit))
	}

	// 会话元数据（plugin/status）仅用于上下文标注，缺失不致命。
	var plugin, status string
	if m.controlStore != nil {
		if meta, merr := m.controlStore.GetSession(ctx, sessionID); merr == nil && meta != nil {
			plugin = meta.Plugin
			status = meta.Status
		}
	}

	timeline := buildTimeline(events, plugin, status)

	if dangling := countDanglingCausation(events); dangling > 0 {
		uncertainties = append(uncertainties,
			fmt.Sprintf("%d events reference a causation_id outside the queried window; shown as roots", dangling))
	}

	timeline.SessionID = sessionID
	timeline.Uncertainties = uncertainties
	slog.Info("get_session_timeline", "session_id", sessionID, "events", len(events),
		"roots", timeline.RootCount, "conversations", len(timeline.Conversations))
	return successResult(timeline), nil
}

// buildTimeline 从事件切片组装因果树。
//
// 关键不变量：Children 用指针，且挂载在 wrapByID 中原始的 *TimelineNode 上；
// 单次遍历内子节点可能在父节点之后被处理，但都追加到同一原始节点，故链接无损。
func buildTimeline(events []*event.Event, plugin, status string) *SessionTimeline {
	wrapByID := make(map[string]*timelineWrap, len(events))
	wraps := make([]*timelineWrap, 0, len(events))
	for _, ev := range events {
		w := &timelineWrap{
			node: &TimelineNode{
				ID:        string(ev.Identity.ID),
				Timestamp: ev.Identity.Timestamp,
				SchemaID:  ev.Identity.SchemaID,
				Type:      string(ev.Identity.Type),
			},
			ev: ev,
		}
		// 复用 eventToMessage 提取 msg_name / direction / is_push / 干净 JSON。
		if msg, merr := eventToMessage(ev); merr == nil {
			w.node.MsgName = msg.MsgName
			w.node.Direction = msg.Direction
			w.node.IsPush = msg.IsPush
			w.node.JSON = string(msg.JSON)
			w.node.Summary = truncateText(string(msg.JSON), 400)
		}
		w.node.Proto = timelineProtocol(ev)
		w.node.CorrelationID = ev.Trace.CorrelationID
		wraps = append(wraps, w)
		wrapByID[string(ev.Identity.ID)] = w
	}

	// 建立父子关系（DAG，按 causation_id 指向父节点）。
	roots := make([]*timelineWrap, 0)
	for _, w := range wraps {
		c := w.ev.Trace.CausationID
		if c != "" {
			if parent, ok := wrapByID[string(c)]; ok {
				parent.node.Children = append(parent.node.Children, w.node)
				continue
			}
		}
		roots = append(roots, w)
	}

	// 递归按时间戳排序（稳定，保证请求→响应顺序）。
	sortRoots(roots)

	// 拍平为可序列化节点。
	outRoots := make([]TimelineNode, 0, len(roots))
	for _, r := range roots {
		outRoots = append(outRoots, *r.node)
	}

	// 按 correlation_id 聚合对话视图。
	convCounts := map[string]int{}
	for _, ev := range events {
		if ev.Trace.CorrelationID != "" {
			convCounts[ev.Trace.CorrelationID]++
		}
	}
	convs := make([]ConversationView, 0, len(convCounts))
	for cid, n := range convCounts {
		convs = append(convs, ConversationView{CorrelationID: cid, EventCount: n})
	}
	sort.Slice(convs, func(i, j int) bool { return convs[i].EventCount > convs[j].EventCount })

	return &SessionTimeline{
		Plugin:        plugin,
		Status:        status,
		EventCount:    len(events),
		RootCount:     len(roots),
		Conversations: convs,
		Roots:         outRoots,
	}
}

type timelineWrap struct {
	node *TimelineNode
	ev   *event.Event
}

// sortRoots 递归按时间戳排序每个节点的子节点（稳定，保证请求→响应顺序）。
func sortRoots(wraps []*timelineWrap) {
	for _, w := range wraps {
		if len(w.node.Children) > 1 {
			sort.SliceStable(w.node.Children, func(i, j int) bool {
				return w.node.Children[i].Timestamp.Before(w.node.Children[j].Timestamp)
			})
		}
		sortRoots(childrenWraps(w))
	}
}

// childrenWraps 把节点的子指针映射回 wrap，以便继续递归排序。
func childrenWraps(w *timelineWrap) []*timelineWrap {
	out := make([]*timelineWrap, 0, len(w.node.Children))
	for _, c := range w.node.Children {
		out = append(out, &timelineWrap{node: c})
	}
	return out
}

// countDanglingCausation 统计 causation_id 指向窗口外事件的节点数量。
func countDanglingCausation(events []*event.Event) int {
	ids := make(map[string]struct{}, len(events))
	for _, ev := range events {
		ids[string(ev.Identity.ID)] = struct{}{}
	}
	n := 0
	for _, ev := range events {
		c := ev.Trace.CausationID
		if c == "" {
			continue
		}
		if _, ok := ids[string(c)]; !ok {
			n++
		}
	}
	return n
}

// truncateText 截断字符串到 maxRunes，超出追加省略号。
func truncateText(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}
