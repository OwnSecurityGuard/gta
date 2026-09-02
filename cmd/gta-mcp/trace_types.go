package main

import (
	"time"
)

// Message 是从 events 表查询出的消息表示（适配 Event）。
type Message struct {
	MsgID     int64
	FlowID    string
	Timestamp time.Time
	Direction string
	MsgName   string
	IsPush    bool
	Src       string
	Dst       string
	RawLen    int
	JSON      []byte
	TCPFlags  string // TCP 控制位字符串（如 "FIN", "RST"），非空表示 tcp_close 事件
	Index     int    // 在 messages 切片中的索引，用于配对
}

// RequestResponsePair 是配对的 request/response。
type RequestResponsePair struct {
	Request  Message
	Response *Message
	PairRule string // "msg_name_suffix" | "direction_temporal" | "unpaired"
}

// TraceStep 是执行链路的一步。
type TraceStep struct {
	StepID        string           `json:"step_id"`
	RequestMsgID  int64            `json:"request_msg_id"`
	Request       RequestSummary   `json:"request"`
	Response      *ResponseSummary `json:"response,omitempty"`
	Pushes        []PushSummary    `json:"pushes,omitempty"`
	EntityDiffs   []EntityDiff     `json:"entity_diffs,omitempty"`
	WhyRelated    string           `json:"why_related"`
}

// RequestSummary / ResponseSummary / PushSummary
type RequestSummary struct {
	Name      string         `json:"name"`
	Direction string         `json:"direction"`
	KeyFields map[string]any `json:"key_fields,omitempty"`
}

type ResponseSummary struct {
	MsgID     int64          `json:"msg_id"`
	Name      string          `json:"name"`
	KeyFields map[string]any `json:"key_fields,omitempty"`
}

type PushSummary struct {
	MsgID   int64  `json:"msg_id"`
	Name    string `json:"name"`
	Summary string `json:"summary"`
}

// EntityDiff 描述一个实体的字段变更。
type EntityDiff struct {
	URI    string   `json:"uri"`    // 如 "Building"
	Key    string   `json:"key"`    // 如 "1001"
	Fields []string `json:"fields"` // 如 ["level", "cost"]
}

// TraceResult 是 trace_protocol_flow 的完整输出。
type TraceResult struct {
	RunID         string      `json:"run_id"`
	FlowID        string      `json:"flow_id"`
	FeatureName   string      `json:"feature_name"`
	TimeWindow    TimeWindow  `json:"time_window"`
	Steps         []TraceStep `json:"steps"`
	CloseInfo     *CloseInfo  `json:"close_info,omitempty"`
	Uncertainties []string    `json:"uncertainties,omitempty"`
	FilePath      string      `json:"file_path,omitempty"`
}

// CloseInfo 描述 TCP 连接关闭信息。
// 通过 flow 内的 tcp_close 事件（FIN/RST）推断哪一侧主动关闭连接。
type CloseInfo struct {
	Closer    string    `json:"closer"`     // "client" | "server" | "unknown"
	Method    string    `json:"method"`     // "FIN" | "RST" | "FIN|ACK"
	Timestamp time.Time `json:"timestamp"`  // 关闭事件时间
	MsgID     int64     `json:"msg_id"`     // 对应事件 ID
	Src       string    `json:"src"`        // 关闭包源地址
	Dst       string    `json:"dst"`        // 关闭包目的地址
	Note      string    `json:"note,omitempty"` // 额外说明
}

// TimeWindow 是 trace 的时间范围。
type TimeWindow struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// NoiseFilter 是噪声过滤配置。
type NoiseFilter struct {
	DropNames      []string
	DropHeartbeats bool
}

// EntityDiffConfig 是 entity diff 配置。
type EntityDiffConfig struct {
	Enabled  bool
	WindowMs int
}
