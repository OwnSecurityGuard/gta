package event

import (
	"fmt"
	"strings"
)

// ValueKind 表示 Value 的类型
type ValueKind int

const (
	// Null 表示空值
	Null ValueKind = iota
	// Bool 表示布尔值
	Bool
	// Int 表示有符号整数
	Int
	// Uint 表示无符号整数
	Uint
	// Float 表示浮点数
	Float
	// String 表示字符串
	String
	// Bytes 表示字节数组
	Bytes
	// Array 表示数组
	Array
	// Object 表示对象（键值对集合）
	Object
)

// String 返回 ValueKind 的字符串表示
func (k ValueKind) String() string {
	switch k {
	case Null:
		return "null"
	case Bool:
		return "bool"
	case Int:
		return "int"
	case Uint:
		return "uint"
	case Float:
		return "float"
	case String:
		return "string"
	case Bytes:
		return "bytes"
	case Array:
		return "array"
	case Object:
		return "object"
	default:
		return fmt.Sprintf("unknown(%d)", k)
	}
}

// Value 是事件载荷中的值类型，支持多种数据类型
// 使用 tagged union 模式，通过 Kind 字段标识实际存储的类型
type Value struct {
	Kind   ValueKind
	Bool   bool
	Int    int64
	Uint   uint64
	Float  float64
	Str    string
	Bytes  []byte
	Array  []Value
	Object map[string]Value
}

// ValueNull 创建空值
func ValueNull() Value {
	return Value{Kind: Null}
}

// ValueBool 创建布尔值
func ValueBool(b bool) Value {
	return Value{Kind: Bool, Bool: b}
}

// ValueInt 创建有符号整数值
func ValueInt(i int64) Value {
	return Value{Kind: Int, Int: i}
}

// ValueUint 创建无符号整数值
func ValueUint(u uint64) Value {
	return Value{Kind: Uint, Uint: u}
}

// ValueFloat 创建浮点数值
func ValueFloat(f float64) Value {
	return Value{Kind: Float, Float: f}
}

// ValueString 创建字符串值
func ValueString(s string) Value {
	return Value{Kind: String, Str: s}
}

// ValueBytes 创建字节数组值
func ValueBytes(b []byte) Value {
	return Value{Kind: Bytes, Bytes: b}
}

// ValueArray 创建数组值
func ValueArray(a []Value) Value {
	return Value{Kind: Array, Array: a}
}

// ValueObject 创建对象值
func ValueObject(m map[string]Value) Value {
	return Value{Kind: Object, Object: m}
}

// IsNull 检查是否为空值
func (v Value) IsNull() bool {
	return v.Kind == Null
}

// AsBool 尝试将值转换为布尔值
func (v Value) AsBool() (bool, bool) {
	if v.Kind == Bool {
		return v.Bool, true
	}
	return false, false
}

// AsInt 尝试将值转换为有符号整数
func (v Value) AsInt() (int64, bool) {
	switch v.Kind {
	case Int:
		return v.Int, true
	case Uint:
		if v.Uint <= uint64(^uint64(0)>>1) {
			return int64(v.Uint), true
		}
	}
	return 0, false
}

// AsUint 尝试将值转换为无符号整数
func (v Value) AsUint() (uint64, bool) {
	switch v.Kind {
	case Uint:
		return v.Uint, true
	case Int:
		if v.Int >= 0 {
			return uint64(v.Int), true
		}
	}
	return 0, false
}

// AsFloat 尝试将值转换为浮点数
func (v Value) AsFloat() (float64, bool) {
	switch v.Kind {
	case Float:
		return v.Float, true
	case Int:
		return float64(v.Int), true
	case Uint:
		return float64(v.Uint), true
	}
	return 0, false
}

// AsString 尝试将值转换为字符串
func (v Value) AsString() (string, bool) {
	if v.Kind == String {
		return v.Str, true
	}
	return "", false
}

// AsBytes 尝试将值转换为字节数组
func (v Value) AsBytes() ([]byte, bool) {
	if v.Kind == Bytes {
		return v.Bytes, true
	}
	return nil, false
}

// AsArray 尝试将值转换为数组
func (v Value) AsArray() ([]Value, bool) {
	if v.Kind == Array {
		return v.Array, true
	}
	return nil, false
}

// AsObject 尝试将值转换为对象
func (v Value) AsObject() (map[string]Value, bool) {
	if v.Kind == Object {
		return v.Object, true
	}
	return nil, false
}

// Get 从对象中获取指定键的值
// 如果值不是对象或键不存在，返回 (ValueNull(), false)
func (v Value) Get(key string) (Value, bool) {
	if v.Kind != Object {
		return ValueNull(), false
	}
	val, ok := v.Object[key]
	return val, ok
}

// GetByPath 按 dot-separated path 从对象中取值。例如 "data.method"。
func (v Value) GetByPath(path string) (Value, bool) {
	parts := strings.Split(path, ".")
	cur := v
	for _, p := range parts {
		obj, ok := cur.AsObject()
		if !ok {
			return Value{}, false
		}
		next, ok := obj[p]
		if !ok {
			return Value{}, false
		}
		cur = next
	}
	return cur, true
}

// Index 从数组中获取指定索引的值
// 如果值不是数组或索引越界，返回 (ValueNull(), false)
func (v Value) Index(i int) (Value, bool) {
	if v.Kind != Array {
		return ValueNull(), false
	}
	if i < 0 || i >= len(v.Array) {
		return ValueNull(), false
	}
	return v.Array[i], true
}

// Len 返回值的长度
// 对于 Array 返回元素个数，对于 Object 返回键值对个数，对于 String/Bytes 返回字节长度
// 其他类型返回 0
func (v Value) Len() int {
	switch v.Kind {
	case Array:
		return len(v.Array)
	case Object:
		return len(v.Object)
	case String:
		return len(v.Str)
	case Bytes:
		return len(v.Bytes)
	}
	return 0
}

// Equal 检查两个值是否相等
func (v Value) Equal(other Value) bool {
	if v.Kind != other.Kind {
		return false
	}

	switch v.Kind {
	case Null:
		return true
	case Bool:
		return v.Bool == other.Bool
	case Int:
		return v.Int == other.Int
	case Uint:
		return v.Uint == other.Uint
	case Float:
		return v.Float == other.Float
	case String:
		return v.Str == other.Str
	case Bytes:
		if len(v.Bytes) != len(other.Bytes) {
			return false
		}
		for i := range v.Bytes {
			if v.Bytes[i] != other.Bytes[i] {
				return false
			}
		}
		return true
	case Array:
		if len(v.Array) != len(other.Array) {
			return false
		}
		for i := range v.Array {
			if !v.Array[i].Equal(other.Array[i]) {
				return false
			}
		}
		return true
	case Object:
		if len(v.Object) != len(other.Object) {
			return false
		}
		for k, val := range v.Object {
			otherVal, ok := other.Object[k]
			if !ok || !val.Equal(otherVal) {
				return false
			}
		}
		return true
	}
	return false
}
