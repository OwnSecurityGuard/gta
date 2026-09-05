package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	"gopkg.in/yaml.v3"
)

// KeyFieldExtractor 预编译 key_field 表达式，供多次调用复用。
type KeyFieldExtractor struct {
	specs map[string]string
	progs map[string]*vm.Program
}

// keyFieldSpec 是 key_fields.yaml 的单项配置。
type keyFieldSpec struct {
	Name string `yaml:"name"`
	Expr string `yaml:"expr"`
}

// NewKeyFieldExtractor 编译 key_field 表达式。
func NewKeyFieldExtractor(specs map[string]string) (*KeyFieldExtractor, error) {
	e := &KeyFieldExtractor{specs: specs, progs: make(map[string]*vm.Program)}
	env := map[string]any{
		"data":  map[string]any(nil),
		"event": (*interface{})(nil),
	}
	for name, exprStr := range specs {
		prog, err := expr.Compile(exprStr, expr.Env(env))
		if err != nil {
			return nil, fmt.Errorf("compile key_field %s: %w", name, err)
		}
		e.progs[name] = prog
	}
	return e, nil
}

// Extract 从 JSON 字节中提取关键业务字段。
func (e *KeyFieldExtractor) Extract(jsonBytes []byte) (map[string]any, error) {
	if len(jsonBytes) == 0 || len(e.progs) == 0 {
		return nil, nil
	}

	var data map[string]any
	if err := json.Unmarshal(jsonBytes, &data); err != nil {
		return nil, err
	}

	env := map[string]any{
		"data":  data,
		"event": nil,
	}

	result := make(map[string]any, len(e.progs))
	for name, prog := range e.progs {
		out, err := expr.Run(prog, env)
		if err != nil {
			result[name] = "<extract_error: " + err.Error() + ">"
			continue
		}
		result[name] = out
	}
	return result, nil
}

// loadKeyFieldExtractor 从 sessionDir/key_fields.yaml 加载配置并创建提取器。
func (m *mcpCapture) loadKeyFieldExtractor(sessionID string) (*KeyFieldExtractor, error) {
	sessionDir := m.sessionMgr.sessionDir(sessionID)
	path := filepath.Join(sessionDir, "key_fields.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 无配置文件，不提取
		}
		return nil, err
	}

	var specs []keyFieldSpec
	if err := yaml.Unmarshal(data, &specs); err != nil {
		return nil, err
	}

	specMap := make(map[string]string, len(specs))
	for _, s := range specs {
		specMap[s.Name] = s.Expr
	}
	if len(specMap) == 0 {
		return nil, nil
	}
	return NewKeyFieldExtractor(specMap)
}

// writeTraceFile 将 trace 结果写入文件，返回路径。
func (m *mcpCapture) writeTraceFile(runID string, result TraceResult) (string, error) {
	runDir := filepath.Join(m.workDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		return "", err
	}
	path := filepath.Join(runDir, "trace.json")
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", err
	}
	return path, nil
}
