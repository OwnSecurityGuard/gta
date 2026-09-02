package store

import (
	"gta/pkg/event"
	"gta/pkg/schema"
)

// extractProjection 从 Event 按 schema 声明的 indexable_fields 提取投影值。
func extractProjection(ev *event.Event, reg *schema.Registry) map[string]any {
	decl, ok := reg.Lookup(ev.Payload.SchemaID)
	if !ok {
		return map[string]any{}
	}
	result := make(map[string]any, len(decl.IndexableFields))
	for _, f := range decl.IndexableFields {
		v, ok := ev.Payload.Value.GetByPath(f.Path)
		if !ok {
			continue
		}
		val, ok := convertValue(v, f.Type)
		if !ok {
			continue
		}
		result[f.Alias] = val
	}
	return result
}

// convertValue 按类型转换 Value。
func convertValue(v event.Value, typ string) (any, bool) {
	switch typ {
	case "string":
		return v.AsString()
	case "int":
		return v.AsInt()
	case "float":
		return v.AsFloat()
	case "bool":
		return v.AsBool()
	default:
		return v.ToAny(), true
	}
}
