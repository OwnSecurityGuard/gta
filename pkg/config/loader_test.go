package config

import (
	"path/filepath"
	"testing"
)

func TestLoadRules(t *testing.T) {
	path := filepath.Join("testdata", "rules.yaml")
	rules, err := LoadRules(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].Name != "test_count" {
		t.Fatalf("unexpected rule name: %s", rules[0].Name)
	}
}
