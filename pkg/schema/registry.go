package schema

import (
	"fmt"
	"sync"
)

// IndexableField 描述插件声明的一个可索引字段。
type IndexableField struct {
	Path  string
	Type  string // string | int | float | bool
	Alias string
}

// SchemaDecl 描述插件注册的一个 schema。
type SchemaDecl struct {
	ID              string
	Version         int
	IndexableFields []IndexableField
}

// Registry 内存级 schema 注册表。
type Registry struct {
	mu      sync.RWMutex
	schemas map[string]*SchemaDecl
}

// NewRegistry 创建空的 Registry。
func NewRegistry() *Registry {
	return &Registry{schemas: make(map[string]*SchemaDecl)}
}

// Register 注册一个 schema 声明。ID 为空时返回错误。
func (r *Registry) Register(decl *SchemaDecl) error {
	if decl == nil || decl.ID == "" {
		return fmt.Errorf("schema id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.schemas[decl.ID] = decl
	return nil
}

// Lookup 查找 schema，返回声明和是否命中。
func (r *Registry) Lookup(schemaID string) (*SchemaDecl, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	decl, ok := r.schemas[schemaID]
	return decl, ok
}
