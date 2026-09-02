package config

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Path 是 gjson 表达式（expr）的封装。所有字段提取一律使用 gjson，不引入复杂规则语言。
type Path struct {
	Expr string `yaml:"expr"`
}

// MessageDefinition 定义一条已知消息（value → name + role）。
type MessageDefinition struct {
	Value any    `yaml:"value"` // 允许 int 或 string；比较时规范化处理
	Name  string `yaml:"name"`
	Role  string `yaml:"role"` // request | response | push
}

// MessageConfig 是 message 段配置：用 message.id.expr 提取消息 ID，按 definitions 映射语义。
type MessageConfig struct {
	ID          Path                `yaml:"id"`
	Definitions []MessageDefinition `yaml:"definitions"`
}

// CorrelationRule 描述一类 Request/Response 配对规则。
// Request/Response 各自使用独立 expr，因为请求与响应的路径往往不同。
type CorrelationRule struct {
	Name     string `yaml:"name"`
	Request  Path   `yaml:"request"`
	Response Path   `yaml:"response"`
}

// CorrelationConfig 是 correlation 段配置。
type CorrelationConfig struct {
	Rules []CorrelationRule `yaml:"rules"`
}

// When 是一条 push 规则的触发条件：expr 提取值命中 equals 中任一值即命中。
// equals 允许标量或列表。
type When struct {
	Expr   string `yaml:"expr"`
	Equals any    `yaml:"equals"`
}

// EqualsList 将 Equals 归一化为列表，便于统一比较。
func (w When) EqualsList() []any {
	switch v := w.Equals.(type) {
	case nil:
		return nil
	case []any:
		return v
	default:
		return []any{v}
	}
}

// PushRule 定义一条 push（推送）识别规则。
type PushRule struct {
	Name string `yaml:"name"`
	When When   `yaml:"when"`
}

// PushConfig 是 push 段配置。
type PushConfig struct {
	Rules []PushRule `yaml:"rules"`
}

// ErrorConfig 是 error 段配置：用 code.expr 提取错误码，命中 success.values 视为成功。
type ErrorConfig struct {
	Code    Path `yaml:"code"`
	Success struct {
		Values []any `yaml:"values"`
	} `yaml:"success"`
}

// File 是完整 protocol.yaml 模型。
type File struct {
	Message     *MessageConfig     `yaml:"message"`
	Correlation *CorrelationConfig `yaml:"correlation"`
	Push        *PushConfig        `yaml:"push"`
	Error       *ErrorConfig       `yaml:"error"`
}

// Parse 从字节解析 protocol.yaml，并做结构校验。
func Parse(data []byte) (*File, error) {
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse protocol config: %w", err)
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// Load 从文件路径加载 protocol.yaml。
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read protocol config %q: %w", path, err)
	}
	return Parse(data)
}

// Validate 检查配置的最小结构性约束。
// 角色取值（request/response/push）在 resolver.New 构建时校验，避免 config 依赖协议层。
func (f *File) Validate() error {
	var errs []error

	if f.Message != nil {
		if f.Message.ID.Expr == "" {
			errs = append(errs, errors.New("message.id.expr is required"))
		}
		seen := make(map[string]bool)
		for _, d := range f.Message.Definitions {
			if d.Value == nil {
				errs = append(errs, errors.New("message.definitions[].value is required"))
			}
			if d.Name == "" {
				errs = append(errs, errors.New("message.definitions[].name is required"))
			}
			if d.Role != "" {
				if k := fmt.Sprintf("%v", d.Value); k != "<nil>" {
					if seen[k] {
						errs = append(errs, fmt.Errorf("duplicate message definition value %v", d.Value))
					}
					seen[k] = true
				}
			}
		}
	}

	if f.Correlation != nil {
		seen := make(map[string]bool)
		for _, r := range f.Correlation.Rules {
			if r.Request.Expr == "" || r.Response.Expr == "" {
				errs = append(errs, fmt.Errorf("correlation.rules[%q] requires request.expr and response.expr", r.Name))
			}
			if r.Name != "" {
				if seen[r.Name] {
					errs = append(errs, fmt.Errorf("duplicate correlation rule name %q", r.Name))
				}
				seen[r.Name] = true
			}
		}
	}

	if f.Push != nil {
		for _, r := range f.Push.Rules {
			if r.When.Expr == "" {
				errs = append(errs, fmt.Errorf("push.rules[%q].when.expr is required", r.Name))
			}
		}
	}

	if f.Error != nil {
		if f.Error.Code.Expr == "" {
			errs = append(errs, errors.New("error.code.expr is required when error is configured"))
		}
	}

	return errors.Join(errs...)
}
