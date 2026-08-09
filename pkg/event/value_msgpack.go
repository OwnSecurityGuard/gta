package event

import (
	"fmt"

	"github.com/vmihailenco/msgpack/v5"
)

// msgpackValue 是 MsgPack 序列化时的中间结构
// 使用 tagged union 模式，通过 type 字段标识实际值
type msgpackValue struct {
	Type   string          `msgpack:"type"`
	Bool   *bool           `msgpack:"bool,omitempty"`
	Int    *int64          `msgpack:"int,omitempty"`
	Uint   *uint64         `msgpack:"uint,omitempty"`
	Float  *float64        `msgpack:"float,omitempty"`
	String *string         `msgpack:"string,omitempty"`
	Bytes  []byte          `msgpack:"bytes,omitempty"`
	Array  []msgpackValue  `msgpack:"array,omitempty"`
	Object *map[string]msgpackValue `msgpack:"object,omitempty"`
}

// MarshalMsgpack 将 Value 编码为 MsgPack 格式
func (v Value) MarshalMsgpack() ([]byte, error) {
	mv := valueToMsgpack(v)
	return msgpack.Marshal(mv)
}

// UnmarshalValueMsgpack 从 MsgPack 格式解码 Value
func UnmarshalValueMsgpack(data []byte) (Value, error) {
	var mv msgpackValue
	if err := msgpack.Unmarshal(data, &mv); err != nil {
		return ValueNull(), fmt.Errorf("unmarshal msgpack: %w", err)
	}
	return msgpackToValue(mv)
}

// valueToMsgpack 将 Value 转换为 msgpackValue
func valueToMsgpack(v Value) msgpackValue {
	switch v.Kind {
	case Null:
		return msgpackValue{Type: "null"}
	case Bool:
		b := v.Bool
		return msgpackValue{Type: "bool", Bool: &b}
	case Int:
		i := v.Int
		return msgpackValue{Type: "int", Int: &i}
	case Uint:
		u := v.Uint
		return msgpackValue{Type: "uint", Uint: &u}
	case Float:
		f := v.Float
		return msgpackValue{Type: "float", Float: &f}
	case String:
		s := v.Str
		return msgpackValue{Type: "string", String: &s}
	case Bytes:
		return msgpackValue{Type: "bytes", Bytes: v.Bytes}
	case Array:
		arr := make([]msgpackValue, len(v.Array))
		for i, item := range v.Array {
			arr[i] = valueToMsgpack(item)
		}
		return msgpackValue{Type: "array", Array: arr}
	case Object:
		obj := make(map[string]msgpackValue, len(v.Object))
		for k, val := range v.Object {
			obj[k] = valueToMsgpack(val)
		}
		return msgpackValue{Type: "object", Object: &obj}
	default:
		return msgpackValue{Type: "null"}
	}
}

// msgpackToValue 将 msgpackValue 转换为 Value
func msgpackToValue(mv msgpackValue) (Value, error) {
	switch mv.Type {
	case "null":
		return ValueNull(), nil
	case "bool":
		if mv.Bool == nil {
			return ValueNull(), fmt.Errorf("bool field is nil")
		}
		return ValueBool(*mv.Bool), nil
	case "int":
		if mv.Int == nil {
			return ValueNull(), fmt.Errorf("int field is nil")
		}
		return ValueInt(*mv.Int), nil
	case "uint":
		if mv.Uint == nil {
			return ValueNull(), fmt.Errorf("uint field is nil")
		}
		return ValueUint(*mv.Uint), nil
	case "float":
		if mv.Float == nil {
			return ValueNull(), fmt.Errorf("float field is nil")
		}
		return ValueFloat(*mv.Float), nil
	case "string":
		if mv.String == nil {
			return ValueNull(), fmt.Errorf("string field is nil")
		}
		return ValueString(*mv.String), nil
	case "bytes":
		return ValueBytes(mv.Bytes), nil
	case "array":
		arr := make([]Value, len(mv.Array))
		for i, item := range mv.Array {
			v, err := msgpackToValue(item)
			if err != nil {
				return ValueNull(), fmt.Errorf("array[%d]: %w", i, err)
			}
			arr[i] = v
		}
		return ValueArray(arr), nil
	case "object":
		if mv.Object == nil {
			return ValueNull(), fmt.Errorf("object field is nil")
		}
		obj := make(map[string]Value, len(*mv.Object))
		for k, val := range *mv.Object {
			v, err := msgpackToValue(val)
			if err != nil {
				return ValueNull(), fmt.Errorf("object[%s]: %w", k, err)
			}
			obj[k] = v
		}
		return ValueObject(obj), nil
	default:
		return ValueNull(), fmt.Errorf("unknown type: %s", mv.Type)
	}
}
