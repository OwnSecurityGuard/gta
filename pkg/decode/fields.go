package decode

import (
	"encoding/json"
	"hash/fnv"
	"strings"
)

// FlowIDFromEndpoints 返回方向无关的 flow 标识（纯五元组 hash，不混入 session_id）。
// (src=A, dst=B) 与 (src=B, dst=A) 返回相同值，便于 request/response 配对。
// 决策：不混入 session_id，查询时 WHERE session_id=? AND flow_id=? 区分。
func FlowIDFromEndpoints(src, dst, protocol string) uint64 {
	a, b := src, dst
	if a > b { // 字典序规范化，保证方向无关
		a, b = b, a
	}
	h := fnv.New64a()
	h.Write([]byte(a))
	h.Write([]byte{0})
	h.Write([]byte(b))
	h.Write([]byte{0})
	h.Write([]byte(protocol))
	return h.Sum64()
}

// InferDirectionFromJSON 尝试从 JSON 的 data 子对象中推断方向（仅 HTTP 协议有效）。
// 无法判定时返回 ""（调用方应视为 unknown）。
// 约定：cleanJSON 形如 {"data":{"type":"request",...}}，type 从 data 内 peek。
// HTTP: type=request → client_to_server，type=response → server_to_client
// 风险：反向代理或服务端主动请求场景可能错误，因此调用方应设置 InferredDirection=true。
func InferDirectionFromJSON(jsonBytes []byte) string {
	if len(jsonBytes) == 0 {
		return ""
	}
	var wrapper struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(jsonBytes, &wrapper); err != nil {
		return ""
	}
	typeStr, _ := wrapper.Data["type"].(string)
	switch typeStr {
	case "request":
		return "client_to_server"
	case "response":
		return "server_to_client"
	}
	return ""
}

// InferMsgNameFromJSON 尝试从 JSON 的 data 子对象中推断消息名。
// 约定：cleanJSON 形如 {"data":{"type":"request",...}}，字段从 data 内 peek。
// HTTP request → "METHOD path"（如 "POST /api/login"）
// HTTP response → "resp STATUS"（如 "resp 200"）
// 其他协议返回 ""。
func InferMsgNameFromJSON(jsonBytes []byte) string {
	if len(jsonBytes) == 0 {
		return ""
	}
	var wrapper struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(jsonBytes, &wrapper); err != nil {
		return ""
	}
	typeStr, _ := wrapper.Data["type"].(string)
	method, _ := wrapper.Data["method"].(string)
	path, _ := wrapper.Data["path"].(string)
	status, _ := wrapper.Data["status"].(string)
	if typeStr == "request" && method != "" {
		return method + " " + path
	}
	if typeStr == "response" && status != "" {
		return "resp " + status
	}
	return ""
}

// AnnotateInferredDirection 在 JSON 中追加 _inferred_direction: true 标记，
// 供下游识别 direction 是 fallback 推断而非插件显式产出。
// 若 JSON 无效或已是对象，返回原 JSON。
func AnnotateInferredDirection(jsonBytes []byte) []byte {
	if len(jsonBytes) == 0 {
		return jsonBytes
	}
	var raw map[string]any
	if err := json.Unmarshal(jsonBytes, &raw); err != nil {
		return jsonBytes
	}
	raw["_inferred_direction"] = true
	out, err := json.Marshal(raw)
	if err != nil {
		return jsonBytes
	}
	return out
}

// getString 从 map 中安全取字符串值。
func getString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// StripReqRespSuffix 去掉消息名的 Req/Resp 后缀，返回基名。
// "BuildingUpgradeReq" → "BuildingUpgrade"
// "BuildingUpgradeResp" → "BuildingUpgrade"
// "LoginRequest" → "Login"（支持 Request/Response 全称）
// "BuildingUpgrade" → "BuildingUpgrade"（无后缀，原样返回）
// 用于 trace_protocol_flow 的 request/response 配对。
func StripReqRespSuffix(name string) string {
	for _, suffix := range []string{"Req", "Resp", "Request", "Response"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix)
		}
	}
	return name
}
