package plugin

import (
	"gta/pkg/schema"

	sdk "github.com/OwnSecurityGuard/gta-plugin-sdk"
	sdkschema "github.com/OwnSecurityGuard/gta-plugin-sdk/schema"
)

// schema 包名与宿主 pkg/schema 冲突，schema 声明类型在此用局部别名引用。
type (
	sdkSchema  = sdkschema.Schema
	schemaType = sdkschema.Type
)

// Manifest 及其附属类型的定义已迁入 SDK（github.com/OwnSecurityGuard/gta-plugin-sdk）。
//
// 迁移前 gta 与 SDK 各持有一份逐字段相同的定义，两边独立演进必然漂移：
// 插件按 SDK 的结构产出 plugin.yaml，宿主按自己那份解析，字段一旦不同步就是
// 注册期才暴露的静默错误。现在契约单向流动——SDK 定义，gta 消费。
//
// 这里用类型别名而非新类型，保证 plugin.Manifest 与 sdk.Manifest 完全等价：
// 宿主代码继续写 plugin.Manifest，插件侧传来的 sdk.Manifest 可直接赋值，无需转换。
type (
	Manifest     = sdk.Manifest
	EventSpec    = sdk.EventSpec
	FieldDecl    = sdk.FieldDecl
	DataSpec     = sdk.DataSpec
	DataSchema   = sdk.DataSchema
	ManifestMeta = sdk.ManifestMeta
)

// ParseManifest 解析 plugin.yaml 原文返回 Manifest。
// 不做字段校验，校验由 ValidateManifest 完成。
func ParseManifest(data []byte) (*Manifest, error) {
	return sdk.ParseManifest(data)
}

// ValidateManifest 校验 manifest 必填字段与格式约束。
//
// 只校验形态（api_version 匹配 gta.decoder/v<digit>、name 为 kebab-case 等），
// 不判定版本兼容性——major 是否与宿主一致由 CheckManifestVersion 负责。
func ValidateManifest(m *Manifest) error {
	return sdk.ValidateManifest(m)
}

// ToSchemaRegistry 将 Manifest 中的 schema 声明注册到内存 registry。
//
// Manifest 现为 SDK 类型的别名，Go 不允许为非本地类型定义方法，
// 因此由原先的 (*Manifest).ToSchemaRegistry 方法改为本包函数。
//
// 注册键必须是 **线上串形态**（Ref.Wire()，如 "game.player.v1"）而非裸 ID：
// 两个消费方都拿事件的完整 schema_id 来查——
//   - pkg/decode/dispatcher.go 用 r.SchemaId（插件发出的线上串）校验是否已声明，
//     未命中就把事件降级成 "unknown.v1"；
//   - pkg/store/projection.go 用 ev.Payload.SchemaID 取 indexable_fields 做投影。
//
// 用裸 ID 注册会让上述两处永久 miss（所有插件的所有事件都变 unknown.v1）。
func ToSchemaRegistry(m *Manifest) *schema.Registry {
	r := schema.NewRegistry()
	if m == nil {
		return r
	}
	for _, s := range m.Schemas {
		decl := &schema.SchemaDecl{
			ID:              s.Ref.Wire(),
			Version:         s.Ref.Version,
			IndexableFields: indexableFieldsOf(&s),
		}
		_ = r.Register(decl)
	}
	return r
}

// indexableFieldsOf 汇总一个 schema 的可索引字段：顶层 fields 里
// queryable:true 的标量子字段（spec v3 §8.2）。
//
// 只扫 Fields 一个来源即可覆盖新旧两种写法：v1 遗留的 schema 级
// indexable_fields 块已由 ParseManifest 的 normalizeSchemas 升格为
// Fields 里的 queryable 字段（同名 key 已存在时以显式声明为准），
// 这里再单独走一遍只会复活那些被显式声明覆盖掉的遗留项。
//
// 复合类型（array/object/bytes）按契约不能 queryable，即使标了也跳过，
// 因为投影层 GetByPath 只能取标量。
//
// ponytail: 不下钻嵌套层。嵌套 object 的点路径（pos.x）GetByPath 取得到，
// 真需要时加一个带路径前缀的递归即可；数组内的 queryable 即使推导出路径也
// 永远取不到值（GetByPath 不支持下标，见 pkg/event/value.go），
// 那属于投影模型的设计问题，不是这里的补丁。
func indexableFieldsOf(s *sdkSchema) []schema.IndexableField {
	out := make([]schema.IndexableField, 0, len(s.Fields))
	for name, f := range s.Fields {
		if f == nil || !f.Queryable || f.Sensitive {
			continue
		}
		typ, ok := indexableType(f.Type)
		if !ok {
			continue
		}
		alias := f.Alias
		if alias == "" {
			alias = name
		}
		out = append(out, schema.IndexableField{Path: name, Type: typ, Alias: alias})
	}
	return out
}

// indexableType 把 schema 声明类型映射到投影层支持的类型字符串。
// 非标量（array/object/bytes）返回 false 由调用方跳过。
func indexableType(t schemaType) (string, bool) {
	switch {
	case t.IsSigned() || t.IsUnsigned():
		return "int", true
	case t.IsFloat():
		return "float", true
	case t == sdkschema.TypeBool:
		return "bool", true
	case t == sdkschema.TypeString:
		return "string", true
	default:
		return "", false
	}
}
