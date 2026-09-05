package plugin

import (
	"strings"
	"testing"

	"gametrace/pkg/schema"

	sdk "github.com/OwnSecurityGuard/gta-plugin-sdk"
	sdkschema "github.com/OwnSecurityGuard/gta-plugin-sdk/schema"
)

// Manifest 的解析与字段校验现由 SDK 单一持有，其单元测试也在 SDK
// （plugin_manifest_test.go）。本文件只测宿主特有行为：
//   - 类型别名等价性（别名一旦被改成新类型，跨模块赋值会静默断裂）
//   - 版本协商 CheckManifestVersion 与 ValidateManifest 的职责边界
//   - Manifest → schema.Registry 的投影

// 别名必须与 SDK 类型完全等价，否则插件侧传来的 sdk.Manifest 无法直接赋值。
func TestManifestIsSDKAlias(t *testing.T) {
	var m Manifest
	var s sdk.Manifest = m // 编译期即断言等价
	_ = s

	// SchemaDecl 已迁入 SDK schema 子包（sdkschema.Schema），不再作为 plugin 包别名。
	// 验证 Manifest.Schemas 类型与 SDK 一致。
	var schemas []sdkschema.Schema = m.Schemas
	if schemas != nil {
		t.Fatalf("uninitialized Schemas should be nil, got %+v", schemas)
	}
}

func TestManifestE2E_FullFlow(t *testing.T) {
	// 模拟插件传给 Register RPC 的 plugin.yaml 原文
	yamlContent := `api_version: gta.decoder/v2
name: lol-decoder
protocol: lol
type: decoder
protocol_version: game/patch-15.6
hints:
  - tcp
  - "port:7000"
schemas:
  - id: lol.login
    version: 1
    indexable_fields:
      - { path: cmd_id, type: number, alias: cmd }
meta:
  author: riot
  description: "League of Legends protocol decoder"
`

	m, err := ParseManifest([]byte(yamlContent))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if err := ValidateManifest(m); err != nil {
		t.Fatalf("ValidateManifest: %v", err)
	}
	if err := CheckManifestVersion(m); err != nil {
		t.Fatalf("CheckManifestVersion: %v", err)
	}

	if m.Name != "lol-decoder" || m.Protocol != "lol" {
		t.Errorf("Name/Protocol = %q/%q, want lol-decoder/lol", m.Name, m.Protocol)
	}
	if m.ProtocolVersion != "game/patch-15.6" {
		t.Errorf("ProtocolVersion = %q, want game/patch-15.6", m.ProtocolVersion)
	}
	if len(m.Hints) != 2 || m.Hints[1] != "port:7000" {
		t.Errorf("Hints = %v, want [tcp port:7000]", m.Hints)
	}
	if m.Meta.Author != "riot" {
		t.Errorf("Meta.Author = %q, want riot", m.Meta.Author)
	}
}

// 职责边界：api_version 形态合法但 major 不匹配时，
// ValidateManifest 必须放行，CheckManifestVersion 必须拦下。
func TestManifestE2E_VersionMismatch(t *testing.T) {
	yamlContent := `api_version: gta.decoder/v1
name: future-decoder
protocol: future
type: decoder
`
	m, err := ParseManifest([]byte(yamlContent))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if err := ValidateManifest(m); err != nil {
		t.Fatalf("ValidateManifest should pass for v1 format: %v", err)
	}
	err = CheckManifestVersion(m)
	if err == nil {
		t.Fatal("CheckManifestVersion should fail for v1 vs v2")
	}
	if !strings.Contains(err.Error(), "version mismatch") {
		t.Fatalf("error %q should contain 'version mismatch'", err.Error())
	}
}

// TestToSchemaRegistry 校验两件事：
//
//  1. 注册键是**线上串形态**（id.version）。这是消费方约定的形态：dispatcher 拿插件
//     发出的 r.SchemaId、store/projection 拿 ev.Payload.SchemaID 来查 registry，
//     两者都是带版本的线上串。用裸 ID 注册会让两处永久 miss，所有事件被降级成 unknown.v1。
//  2. v1 遗留的 indexable_fields 块能被派生为可索引字段，且声明类型被映射成
//     投影层认得的 "int"/"string" 等（直接透传 "int64" 会在 convertValue 落
//     default 被丢弃）。
//
// 必须走 ParseManifest 而不是直接构造 Manifest：遗留块的升格发生在 ParseManifest
// 内部，生产上所有 Manifest 都出自这条路径，绕过它就测不到真实行为。
func TestToSchemaRegistry(t *testing.T) {
	yamlContent := `api_version: gta.decoder/v2
name: lol-decoder
protocol: lol
type: decoder
schemas:
  - id: lol.login
    version: 1
    indexable_fields:
      - { path: cmd_id, type: int64, alias: cmd }
      - { path: user.name, type: string }
  - id: lol.move
    version: 2
`
	m, err := ParseManifest([]byte(yamlContent))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}

	r := ToSchemaRegistry(m)
	if r == nil {
		t.Fatal("ToSchemaRegistry returned nil")
	}

	decl, ok := r.Lookup("lol.login.v1")
	if !ok {
		t.Fatal("schema lol.login.v1 not registered")
	}
	if decl.Version != 1 {
		t.Errorf("lol.login.v1 version = %d, want 1", decl.Version)
	}
	// 顺序随 map 遍历不定，按 path 取而不是按索引取。
	byPath := make(map[string]schema.IndexableField, len(decl.IndexableFields))
	for _, f := range decl.IndexableFields {
		byPath[f.Path] = f
	}
	if len(byPath) != 2 {
		t.Fatalf("lol.login.v1 indexable fields = %d (%+v), want 2", len(decl.IndexableFields), decl.IndexableFields)
	}
	if f := byPath["cmd_id"]; f.Type != "int" || f.Alias != "cmd" {
		t.Errorf("cmd_id = %+v, want {cmd_id int cmd}", f)
	}
	if f := byPath["user.name"]; f.Type != "string" || f.Alias != "user.name" {
		t.Errorf("user.name = %+v, want {user.name string user.name}（alias 回落到字段名）", f)
	}
	if _, ok := r.Lookup("lol.move.v2"); !ok {
		t.Error("schema lol.move.v2 not registered")
	}
	// 裸 ID 不应命中：消费方不会用裸 ID 查询。
	if _, ok := r.Lookup("lol.login"); ok {
		t.Error("bare id lol.login must not be a registry key (consumers look up the wire form)")
	}
}

// TestToSchemaRegistryQueryableFields 校验现代声明（fields 里 queryable:true）
// 会被派生为可索引字段；复合类型与 sensitive 字段必须被跳过。
func TestToSchemaRegistryQueryableFields(t *testing.T) {
	m := &Manifest{
		Schemas: []sdkschema.Schema{{
			Ref: sdkschema.Ref{ID: "ecs.state", Version: 1},
			Fields: map[string]*sdkschema.Field{
				"tick":   {Type: sdkschema.TypeUint64, Queryable: true},
				"ent_id": {Type: sdkschema.TypeInt64, Queryable: true, Alias: "eid"},
				"ratio":  {Type: sdkschema.TypeFloat64, Queryable: true},
				"moving": {Type: sdkschema.TypeBool, Queryable: true},
				"name":   {Type: sdkschema.TypeString, Queryable: true},
				"ents":   {Type: sdkschema.TypeArray, Queryable: true},                   // 复合类型，跳过
				"token":  {Type: sdkschema.TypeString, Queryable: true, Sensitive: true}, // PII，跳过
				"quiet":  {Type: sdkschema.TypeString},                                   // 未标 queryable，跳过
				// 嵌套在 array items 下的 queryable 同样不产出：GetByPath 下不了数组下标。
				"bag": {Type: sdkschema.TypeArray, Items: &sdkschema.Field{
					Type: sdkschema.TypeObject,
					Fields: map[string]*sdkschema.Field{
						"nested": {Type: sdkschema.TypeInt64, Queryable: true},
					},
				}},
			},
		}},
	}

	decl, ok := ToSchemaRegistry(m).Lookup("ecs.state.v1")
	if !ok {
		t.Fatal("schema ecs.state.v1 not registered")
	}
	want := map[string]string{
		"tick":   "int",
		"ent_id": "int",
		"ratio":  "float",
		"moving": "bool",
		"name":   "string",
	}
	if len(decl.IndexableFields) != len(want) {
		t.Fatalf("indexable fields = %d, want %d (%+v)", len(decl.IndexableFields), len(want), decl.IndexableFields)
	}
	for _, f := range decl.IndexableFields {
		wt, ok := want[f.Path]
		if !ok {
			t.Errorf("unexpected indexable field %q (type %s)", f.Path, f.Type)
			continue
		}
		if f.Type != wt {
			t.Errorf("field %q type = %s, want %s", f.Path, f.Type, wt)
		}
		if f.Path == "ent_id" && f.Alias != "eid" {
			t.Errorf("ent_id alias = %q, want eid", f.Alias)
		}
		if f.Path == "tick" && f.Alias != "tick" {
			t.Errorf("tick alias = %q, want tick (fallback to field name)", f.Alias)
		}
	}
}

// nil manifest 不应 panic：manager.SchemaRegistry 在插件尚未上报 manifest 时会走到这里。
func TestToSchemaRegistryNil(t *testing.T) {
	if r := ToSchemaRegistry(nil); r == nil {
		t.Fatal("ToSchemaRegistry(nil) should return an empty registry, not nil")
	}
}
