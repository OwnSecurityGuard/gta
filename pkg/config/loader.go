package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"gametrace/pkg/analyze"
	"gametrace/pkg/schema"

	"gopkg.in/yaml.v3"
)

// File 是配置文件的顶层结构。
type File struct {
	Rules []analyze.RawRule `yaml:"rules"`
}

// LoadRules 从 YAML 文件加载并编译规则。
func LoadRules(path string) ([]*analyze.CompiledRule, error) {
	slog.Info("loading rules", "path", path)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	baseDir := filepath.Dir(path)
	schemas := map[string]*schema.Schema{}
	var out []*analyze.CompiledRule
	for _, r := range f.Rules {
		s, err := loadSchema(baseDir, r.Schema, schemas)
		if err != nil {
			return nil, fmt.Errorf("load schema for rule %s: %w", r.Name, err)
		}
		cr, err := analyze.CompileRule(r, s)
		if err != nil {
			return nil, fmt.Errorf("compile rule %s: %w", r.Name, err)
		}
		out = append(out, cr)
	}
	slog.Info("rules loaded", "count", len(out))
	return out, nil
}

func loadSchema(baseDir, schemaPath string, cache map[string]*schema.Schema) (*schema.Schema, error) {
	if schemaPath == "" {
		return nil, nil
	}
	if s, ok := cache[schemaPath]; ok {
		return s, nil
	}
	if !filepath.IsAbs(schemaPath) {
		schemaPath = filepath.Join(baseDir, schemaPath)
	}
	b, err := os.ReadFile(schemaPath)
	if err != nil {
		return nil, err
	}
	var s schema.Schema
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	cache[schemaPath] = &s
	return &s, nil
}
