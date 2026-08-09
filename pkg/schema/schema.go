package schema

import (
	"fmt"
	"strings"
)

// Field 描述一个字段。
type Field struct {
	Type   string            `json:"type"` // string/number/object/array/boolean
	Fields map[string]*Field `json:"fields,omitempty"`
}

// Schema 描述插件输出 JSON 的结构。
type Schema struct {
	Fields map[string]*Field `json:"fields"`
}

// Lookup 按点号路径查找字段。
func (s *Schema) Lookup(path string) (*Field, error) {
	parts := strings.Split(path, ".")
	cur := &Field{Type: "object", Fields: s.Fields}
	for _, p := range parts {
		if cur.Type != "object" || cur.Fields == nil {
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
func (s *Schema) ValidateExprPaths(expr string, paths []string) error {
	for _, p := range paths {
		if _, err := s.Lookup(p); err != nil {
			return fmt.Errorf("validate %q: %w", expr, err)
		}
	}
	return nil
}
