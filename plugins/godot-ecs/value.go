package main

import (
	"github.com/OwnSecurityGuard/gta-plugin-sdk/event"
)

// num 标记一个"必须保持浮点"的数值。
//
// 背景：event.ValueFromAny 会把整数值的 float64（0、1、-2 之类）折叠成
// event.Int，而 schema 中声明为 float64 的字段要求 wire kind 为 Float，
// kindMatches 判定不匹配后宿主会报 payload-conforms ERROR。世界同步报文里
// 坐标 / 朝向 / 相位恰好经常是整数值（y 恒为 0、静止时 x/z 取整），
// 因此必须显式保浮点，不能依赖默认转换。
type num float64

// valueFromMap 把业务字段 + _meta 转成事件载荷（根必须是 object）。
// 与 event.ValueFromMap 的唯一差别是识别 num 并保持 Float。
func valueFromMap(m map[string]any) event.Value {
	obj := make(map[string]event.Value, len(m))
	for k, v := range m {
		obj[k] = valueFromAny(v)
	}
	return event.ValueObject(obj)
}

func valueFromAny(v any) event.Value {
	switch x := v.(type) {
	case num:
		return event.ValueFloat(float64(x))
	case map[string]any:
		return valueFromMap(x)
	case []any:
		arr := make([]event.Value, len(x))
		for i, e := range x {
			arr[i] = valueFromAny(e)
		}
		return event.ValueArray(arr)
	default:
		return event.ValueFromAny(v)
	}
}
