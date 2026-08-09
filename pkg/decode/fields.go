package decode

import (
	"encoding/json"
	"fmt"
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

// ExtractedFields 是从 JSON _fields 子对象抽取的结构化字段。
type ExtractedFields struct {
	Direction       string                  // "client_to_server" | "server_to_client" | ""
	MsgName         string                  // 业务消息名
	IsPush          bool                    // 是否为服务器推送
	TCPFlags        string                  // TCP 控制位字符串（如 "FIN", "RST", "FIN|ACK"），非空表示 tcp_close 事件
	HasDirection    bool                    // Direction 是否由插件显式产出
	HasMsgName      bool                    // MsgName 是否由插件显式产出
}

// ExtractFields 从 JSON 字节中抽取 _fields 子对象（若存在），并返回剩余的纯净 JSON。
// 约定：插件在 JSON 顶层追加 "_fields" 子对象，形如：
//   {"type":"request","method":"POST",...,"_fields":{"direction":"client_to_server","msg_name":"POST /api/login","is_push":false}}
// 抽取后 _fields 键会从 JSON 中删除，避免污染下游 data.* 表达式。
//
// 校验策略：对 _fields 内的已知字段做类型/取值校验。校验失败时返回 error，
// 但仍会填充已成功解析的字段（向后兼容，不阻断解码流程）。
// 调用方应记录该 error 但不必中断处理。
func ExtractFields(jsonBytes []byte) (ExtractedFields, []byte, error) {
	var result ExtractedFields
	if len(jsonBytes) == 0 {
		return result, jsonBytes, nil
	}

	var raw map[string]any
	if err := json.Unmarshal(jsonBytes, &raw); err != nil {
		return result, jsonBytes, fmt.Errorf("unmarshal json for _fields extraction: %w", err)
	}

	fieldsRaw, ok := raw["_fields"]
	if !ok {
		// 无 _fields，返回原 JSON
		return result, jsonBytes, nil
	}

	// 删除 _fields 键，重新序列化得到纯净 JSON
	delete(raw, "_fields")
	cleanJSON, err := json.Marshal(raw)
	if err != nil {
		return result, jsonBytes, fmt.Errorf("re-marshal json after _fields removal: %w", err)
	}

	// 解析 _fields
	fieldsMap, ok := fieldsRaw.(map[string]any)
	if !ok {
		return result, cleanJSON, fmt.Errorf("_fields is not an object: %T", fieldsRaw)
	}

	// 收集校验错误（不阻断，最后统一返回）
	var validationErrors []string

	// direction: 必须是字符串，取值限定
	if v, exists := fieldsMap["direction"]; exists {
		if s, ok := v.(string); ok {
			switch s {
			case "client_to_server", "server_to_client", "unknown", "":
				result.Direction = s
				result.HasDirection = true
			default:
				validationErrors = append(validationErrors,
					fmt.Sprintf("direction has invalid value %q (allowed: client_to_server|server_to_client|unknown)", s))
				// 仍记录方向，但标记为非显式产出
			}
		} else {
			validationErrors = append(validationErrors,
				fmt.Sprintf("direction must be string, got %T", v))
		}
	}

	// msg_name: 必须是字符串
	if v, exists := fieldsMap["msg_name"]; exists {
		if s, ok := v.(string); ok {
			result.MsgName = s
			result.HasMsgName = true
		} else {
			validationErrors = append(validationErrors,
				fmt.Sprintf("msg_name must be string, got %T", v))
		}
	}

	// is_push: 必须是 bool
	if v, exists := fieldsMap["is_push"]; exists {
		if b, ok := v.(bool); ok {
			result.IsPush = b
		} else {
			validationErrors = append(validationErrors,
				fmt.Sprintf("is_push must be bool, got %T", v))
		}
	}

	// tcp_flags: 必须是字符串
	if v, exists := fieldsMap["tcp_flags"]; exists {
		if s, ok := v.(string); ok {
			result.TCPFlags = s
		} else {
			validationErrors = append(validationErrors,
				fmt.Sprintf("tcp_flags must be string, got %T", v))
		}
	}

	if len(validationErrors) > 0 {
		return result, cleanJSON, fmt.Errorf("_fields validation: %s", strings.Join(validationErrors, "; "))
	}
	return result, cleanJSON, nil
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
