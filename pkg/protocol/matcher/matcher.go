// Package matcher 提供基于 gjson 的固定语义槽位匹配能力。
//
// 设计约束（对应 MVP 范围）：
//   - 不做复杂表达式：不引入 CEL / expr-lang，gjson 足够
//   - 不做自动推断：所有语义槽位均由 protocol.yaml 显式配置
package matcher

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// JSONOf 将已解码的 payload 序列化为 JSON，供 gjson 匹配。
// 调用方负责保证 payload 可序列化；失败时返回可读错误。
func JSONOf(v interface{ ToJSON() ([]byte, error) }) (string, error) {
	b, err := v.ToJSON()
	if err != nil {
		return "", fmt.Errorf("marshal payload to json for protocol matching: %w", err)
	}
	return string(b), nil
}

// Get 按 gjson 表达式提取字符串值。
// 不存在或值为 null 时返回 ("", false)。
func Get(json, expr string) (string, bool) {
	r, ok := Raw(json, expr)
	if !ok {
		return "", false
	}
	return r.String(), true
}

// Raw 按 gjson 表达式返回原始结果。
// 不存在或值为 null 时返回 (zero, false)。
func Raw(json, expr string) (gjson.Result, bool) {
	if expr == "" {
		return gjson.Result{}, false
	}
	r := gjson.Get(json, expr)
	if !r.Exists() || r.Type == gjson.Null {
		return gjson.Result{}, false
	}
	return r, true
}

// KeyOf 返回 expr 的最后一段路径（如 "body.seq" → "seq"），用于关联字段名。
func KeyOf(expr string) string {
	i := strings.LastIndex(expr, ".")
	if i < 0 {
		return expr
	}
	return expr[i+1:]
}

// Equals 比较 gjson 提取值 raw 与配置的期望值 expected 是否相等。
//
// 比较规则（规范化）：
//   - 数值 10 与字符串 "10" 视为相等（协议字段多为 int，配置常写字符串）
//   - 浮点 10.0 与 int 10 视为相等
//   - bool 仅与 bool 比较
func Equals(raw gjson.Result, expected any) bool {
	if !raw.Exists() || raw.Type == gjson.Null {
		return false
	}
	switch exp := expected.(type) {
	case bool:
		return raw.Type == gjson.True && raw.Bool() == exp || raw.Type == gjson.False && raw.Bool() == exp
	case string:
		return raw.String() == exp || numEquals(raw.String(), exp)
	case int:
		return numEquals(raw.String(), strconv.Itoa(exp))
	case int64:
		return numEquals(raw.String(), strconv.FormatInt(exp, 10))
	case float64:
		return floatEquals(raw, exp)
	default:
		// 其他类型（map/slice 等）不支持作为 equals 期望值。
		return false
	}
}

// numEquals 判断规范化数字字符串是否相等（如 "10" == "10"、raw="10.0" vs exp="10"）。
func numEquals(raw, exp string) bool {
	if raw == exp {
		return true
	}
	if !isNumeric(raw) || !isNumeric(exp) {
		return false
	}
	rf, err1 := strconv.ParseFloat(raw, 64)
	ef, err2 := strconv.ParseFloat(exp, 64)
	if err1 != nil || err2 != nil {
		return false
	}
	return rf == ef
}

// floatEquals 处理 float64 期望值（如 equals: 0 被 YAML 解析为 int，这里覆盖显式浮点）。
func floatEquals(raw gjson.Result, exp float64) bool {
	if raw.Type == gjson.Number {
		return raw.Float() == exp
	}
	if raw.Type == gjson.String {
		return numEquals(raw.String(), strconv.FormatFloat(exp, 'f', -1, 64))
	}
	return false
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}
