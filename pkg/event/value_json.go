package event

import (
	"encoding/json"
	"fmt"
)

// MarshalJSON 将 Value 编码为 JSON 格式
func (v Value) MarshalJSON() ([]byte, error) {
	switch v.Kind {
	case Null:
		return json.Marshal(nil)
	case Bool:
		return json.Marshal(v.Bool)
	case Int:
		return json.Marshal(v.Int)
	case Uint:
		return json.Marshal(v.Uint)
	case Float:
		return json.Marshal(v.Float)
	case String:
		return json.Marshal(v.Str)
	case Bytes:
		// 字节数组编码为 base64 字符串
		return json.Marshal(v.Bytes)
	case Array:
		return json.Marshal(v.Array)
	case Object:
		return json.Marshal(v.Object)
	default:
		return nil, fmt.Errorf("unknown value kind: %v", v.Kind)
	}
}

// UnmarshalJSON 从 JSON 格式解码 Value
func (v *Value) UnmarshalJSON(data []byte) error {
	// 尝试解析为通用接口
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	// 转换为 Value
	*v = anyToValue(raw)
	return nil
}

// anyToValue 将 JSON 解码后的 any 类型转换为 Value。
// 复用 ValueFromAny，保持类型映射一致。
func anyToValue(raw any) Value {
	return ValueFromAny(raw)
}

// ToJSON 将 Value 编码为 JSON 字节数组
func (v Value) ToJSON() ([]byte, error) {
	return json.Marshal(v)
}

// ToJSONIndent 将 Value 编码为格式化的 JSON 字节数组
func (v Value) ToJSONIndent() ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

// String 返回 Value 的 JSON 字符串表示
func (v Value) String() string {
	data, err := v.ToJSON()
	if err != nil {
		return fmt.Sprintf("<error: %v>", err)
	}
	return string(data)
}
