package event

import (
	"encoding/json"
	"fmt"
)

// ValueFromAny 将 any 类型转换为 Value
// 支持的类型：nil, bool, int/int8/int16/int32/int64, uint/uint8/uint16/uint32/uint64,
// float32/float64, string, []byte, []any, map[string]any
// 未知类型退化为字符串表示，避免静默丢失数据。
func ValueFromAny(v any) Value {
	if v == nil {
		return ValueNull()
	}

	switch val := v.(type) {
	case bool:
		return ValueBool(val)
	case int:
		return ValueInt(int64(val))
	case int8:
		return ValueInt(int64(val))
	case int16:
		return ValueInt(int64(val))
	case int32:
		return ValueInt(int64(val))
	case int64:
		return ValueInt(val)
	case uint:
		return ValueUint(uint64(val))
	case uint8:
		return ValueUint(uint64(val))
	case uint16:
		return ValueUint(uint64(val))
	case uint32:
		return ValueUint(uint64(val))
	case uint64:
		return ValueUint(val)
	case float32:
		return ValueFloat(float64(val))
	case float64:
		// JSON 数字默认解析为 float64；若其值在整数范围内则存为 Int，保持精度。
		if val == float64(int64(val)) {
			return ValueInt(int64(val))
		}
		return ValueFloat(val)
	case string:
		return ValueString(val)
	case []byte:
		return ValueBytes(val)
	case []any:
		arr := make([]Value, len(val))
		for i, item := range val {
			arr[i] = ValueFromAny(item)
		}
		return ValueArray(arr)
	case map[string]any:
		obj := make(map[string]Value, len(val))
		for k, v := range val {
			obj[k] = ValueFromAny(v)
		}
		return ValueObject(obj)
	default:
		return ValueString(fmt.Sprintf("%v", val))
	}
}

// ToAny 将 Value 转换为 Go 原生类型
// Null -> nil
// Bool -> bool
// Int -> int64
// Uint -> uint64
// Float -> float64
// String -> string
// Bytes -> []byte
// Array -> []any
// Object -> map[string]any
func (v Value) ToAny() any {
	switch v.Kind {
	case Null:
		return nil
	case Bool:
		return v.Bool
	case Int:
		return v.Int
	case Uint:
		return v.Uint
	case Float:
		return v.Float
	case String:
		return v.Str
	case Bytes:
		return v.Bytes
	case Array:
		arr := make([]any, len(v.Array))
		for i, item := range v.Array {
			arr[i] = item.ToAny()
		}
		return arr
	case Object:
		obj := make(map[string]any, len(v.Object))
		for k, val := range v.Object {
			obj[k] = val.ToAny()
		}
		return obj
	default:
		return nil
	}
}

// ValueFromJSON 从 JSON 字节数组解析 Value
func ValueFromJSON(data []byte) (Value, error) {
	var v Value
	if err := v.UnmarshalJSON(data); err != nil {
		return ValueNull(), fmt.Errorf("parse json: %w", err)
	}
	return v, nil
}

// ValueFromMap 从 map[string]any 创建 Value
func ValueFromMap(m map[string]any) Value {
	obj := make(map[string]Value, len(m))
	for k, v := range m {
		obj[k] = ValueFromAny(v)
	}
	return ValueObject(obj)
}

// ValueFromSlice 从 []any 创建 Value
func ValueFromSlice(s []any) Value {
	arr := make([]Value, len(s))
	for i, v := range s {
		arr[i] = ValueFromAny(v)
	}
	return ValueArray(arr)
}

// Merge 合并两个对象类型的 Value
// 如果 v 或 other 不是对象，返回错误
// other 中的键会覆盖 v 中的同名键
func (v Value) Merge(other Value) (Value, error) {
	if v.Kind != Object {
		return ValueNull(), fmt.Errorf("cannot merge: left value is not object")
	}
	if other.Kind != Object {
		return ValueNull(), fmt.Errorf("cannot merge: right value is not object")
	}

	result := make(map[string]Value, len(v.Object)+len(other.Object))
	for k, val := range v.Object {
		result[k] = val
	}
	for k, val := range other.Object {
		result[k] = val
	}
	return ValueObject(result), nil
}

// Set 在对象中设置键值对
// 如果 v 不是对象，返回错误
func (v Value) Set(key string, val Value) (Value, error) {
	if v.Kind != Object {
		return ValueNull(), fmt.Errorf("cannot set: value is not object")
	}

	result := make(map[string]Value, len(v.Object)+1)
	for k, v := range v.Object {
		result[k] = v
	}
	result[key] = val
	return ValueObject(result), nil
}

// Delete 从对象中删除键
// 如果 v 不是对象，返回错误
func (v Value) Delete(key string) (Value, error) {
	if v.Kind != Object {
		return ValueNull(), fmt.Errorf("cannot delete: value is not object")
	}

	result := make(map[string]Value, len(v.Object))
	for k, val := range v.Object {
		if k != key {
			result[k] = val
		}
	}
	return ValueObject(result), nil
}

// Append 向数组追加元素
// 如果 v 不是数组，返回错误
func (v Value) Append(item Value) (Value, error) {
	if v.Kind != Array {
		return ValueNull(), fmt.Errorf("cannot append: value is not array")
	}

	result := make([]Value, len(v.Array)+1)
	copy(result, v.Array)
	result[len(v.Array)] = item
	return ValueArray(result), nil
}

// ValueFromJSONMap 从 JSON 字节数组解析为 map[string]any，然后转换为 Value。
func ValueFromJSONMap(data []byte) (Value, error) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return ValueNull(), fmt.Errorf("unmarshal json map: %w", err)
	}
	return ValueFromMap(m), nil
}
