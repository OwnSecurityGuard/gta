package plugin

import (
	"strings"
	"testing"

	sdk "github.com/OwnSecurityGuard/gta-plugin-sdk"
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

	var decl SchemaDecl = sdk.SchemaDecl{ID: "x", Version: 1}
	if decl.ID != "x" {
		t.Fatalf("SchemaDecl alias broken: %+v", decl)
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

func TestToSchemaRegistry(t *testing.T) {
	m := &Manifest{
		Schemas: []SchemaDecl{
			{
				ID:      "lol.login",
				Version: 1,
				IndexableFields: []IndexableField{
					{Path: "cmd_id", Type: "number", Alias: "cmd"},
					{Path: "user.name", Type: "string"},
				},
			},
			{ID: "lol.move", Version: 2},
		},
	}

	r := ToSchemaRegistry(m)
	if r == nil {
		t.Fatal("ToSchemaRegistry returned nil")
	}

	decl, ok := r.Lookup("lol.login")
	if !ok {
		t.Fatal("schema lol.login not registered")
	}
	if decl.Version != 1 {
		t.Errorf("lol.login version = %d, want 1", decl.Version)
	}
	if len(decl.IndexableFields) != 2 {
		t.Fatalf("lol.login indexable fields = %d, want 2", len(decl.IndexableFields))
	}
	if f := decl.IndexableFields[0]; f.Path != "cmd_id" || f.Type != "number" || f.Alias != "cmd" {
		t.Errorf("indexable field[0] = %+v, want {cmd_id number cmd}", f)
	}
	if _, ok := r.Lookup("lol.move"); !ok {
		t.Error("schema lol.move not registered")
	}
}

// nil manifest 不应 panic：manager.SchemaRegistry 在插件尚未上报 manifest 时会走到这里。
func TestToSchemaRegistryNil(t *testing.T) {
	if r := ToSchemaRegistry(nil); r == nil {
		t.Fatal("ToSchemaRegistry(nil) should return an empty registry, not nil")
	}
}
