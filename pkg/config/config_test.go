package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRulesWithSchema(t *testing.T) {
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.json")
	rulesPath := filepath.Join(dir, "rules.yaml")

	schema := `{
  "fields": {
    "name": {"type": "string"},
    "damage": {"type": "number"}
  }
}`
	rules := `rules:
  - name: hit
    filter: 'data.name == "hit" && data.damage > 0'
    schema: schema.json
    aggregate:
      type: count
      window: 1s
      group_by: []
      output: hits
`
	if err := os.WriteFile(schemaPath, []byte(schema), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rulesPath, []byte(rules), 0644); err != nil {
		t.Fatal(err)
	}

	compiled, err := LoadRules(rulesPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(compiled))
	}
}

func TestLoadRulesWithBadSchema(t *testing.T) {
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.json")
	rulesPath := filepath.Join(dir, "rules.yaml")

	schema := `{"fields":{"name":{"type":"string"}}}`
	rules := `rules:
  - name: bad
    filter: 'data.missing == 1'
    schema: schema.json
    aggregate:
      type: count
      window: 1s
      group_by: []
      output: bad
`
	if err := os.WriteFile(schemaPath, []byte(schema), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rulesPath, []byte(rules), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadRules(rulesPath); err == nil {
		t.Fatal("expected schema validation error")
	}
}
