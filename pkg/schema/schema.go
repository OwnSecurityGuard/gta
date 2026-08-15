package schema

import (
	"fmt"
	"strings"

	sdkschema "github.com/OwnSecurityGuard/gta-plugin-sdk/schema"
)

// ─────────────────────────────────────────────────────────────────────────────
// SDK 类型别名 — 契约单向流动：SDK 定义，gta 消费
//
// 以下类型别名将 SDK 的 Semantic Contract v1 四层类型（schema/state/evidence/rule）
// 桥接到 gta 内部，消除两套定义间的漂移。新增代码应直接使用 SDK 类型，旧代码通过
// 别名兼容。
// ─────────────────────────────────────────────────────────────────────────────

// Field 描述一个字段。v1 起桥接到 SDK 的 schema.Field，兼容旧 API。
// 调用方仍可通过 .Type 和 .Fields 访问，但字段语义更丰富（Semantic/Unit/Entity 等）。
type Field = sdkschema.Field

// Schema 描述插件输出 JSON 的结构。v1 起桥接到 SDK 的 schema.Schema。
type Schema = sdkschema.Schema

// SchemaType 是 SDK 声明的类型枚举（int8/int16/int32/int64/uint8/uint16/uint32/uint64/
// float32/float64/bool/string/bytes/array/object/null）。
type SchemaType = sdkschema.Type

// Semantic 是字段的语义标注（builtin/x- experimental/gta. reserved）。
type Semantic = sdkschema.Semantic

// ─────────────────────────────────────────────────────────────────────────────
// 兼容层 — 保持现有调用方代码零修改
// ─────────────────────────────────────────────────────────────────────────────

// Lookup 按点号路径查找字段。兼容 SDK Schema.Fields 结构。
func Lookup(s *Schema, path string) (*Field, error) {
	if s == nil || s.Fields == nil {
		return nil, fmt.Errorf("path %q: schema has no fields", path)
	}
	parts := strings.Split(path, ".")
	cur := &Field{Type: sdkschema.TypeObject, Fields: s.Fields}
	for _, p := range parts {
		if cur.Type != sdkschema.TypeObject || cur.Fields == nil {
			return nil, fmt.Errorf("path %q is not an object", path)
		}
		f, ok := cur.Fields[p]
		if !ok {
			return nil, fmt.Errorf("field %q not found in path %q", p, path)
		}
		cur = f
	}
	return cur, nil
}

// ValidateExprPaths 检查表达式中引用的字段路径是否都存在。
// 简化版：扫描字符串中的点号路径（真实实现可解析 expr AST）。
func ValidateExprPaths(s *Schema, expr string, paths []string) error {
	for _, p := range paths {
		if _, err := Lookup(s, p); err != nil {
			return fmt.Errorf("validate %q: %w", expr, err)
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 向后兼容 — 旧 API 的包装函数
// 以下方法保留旧代码的调用模式，内部转发到新的包级函数。
// 后续可逐步迁移调用方到直接使用 SDK 类型。
// ─────────────────────────────────────────────────────────────────────────────

// DeprecatedLookup 是旧 Schema.Lookup 的兼容包装。
// 新代码请直接使用包级 Lookup 函数或 SDK 的 schema.Schema。
func DeprecatedLookup(s *Schema, path string) (*Field, error) {
	return Lookup(s, path)
}
