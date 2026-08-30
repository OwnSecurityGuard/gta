// project.go — 轻量「项目」模型（Web First · P1）。
//
// 一个 project 只容纳名称 + 默认解码插件 + 默认抓包端口，供用户进入项目后
// 一键开始抓包（自动带上插件与端口，无需重复配置）。刻意不做 Workspace /
// Organization / RBAC 等推广结构（见产品评审 P1 克制原则）。
//
// 存储按 owner 分片到 workDir/projects.<owner>.json（匿名 owner 落 projects.json），
// 与 current 分片语义一致；owner 过滤 + admin 可见全部，复用既有会话归属逻辑。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"gta/pkg/auth"
)

// project 是一条可复用抓包配置（名称 / 默认插件 / 默认端口）。
type project struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Owner     string `json:"owner"`
	Plugin    string `json:"plugin,omitempty"` // 默认解码插件名（可为空）
	Port      int    `json:"port,omitempty"`   // 默认抓包端口（0=未设置，仅 source=nic/agent 用）
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// projectStore 管理 projects 文件的读写，跨 owner 分片，进程内互斥。
type projectStore struct {
	workDir string
	mu      sync.Mutex
}

func newProjectStore(workDir string) *projectStore {
	if workDir == "" {
		workDir = "."
	}
	return &projectStore{workDir: workDir}
}

// projectShardName 复用 current 分片的 owner 清洗规则，落成 projects.<owner>.json。
func projectShardName(owner string) string {
	if owner == "" {
		return "projects.json"
	}
	var b strings.Builder
	for _, r := range owner {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return "projects." + b.String() + ".json"
}

func (ps *projectStore) pathFor(owner string) string {
	return filepath.Join(ps.workDir, projectShardName(owner))
}

// ownerFromShardFile 从 projects.<owner>.json 反解 owner（带清洗副作用，仅用于 admin 汇总）。
func ownerFromProjectShardFile(path, prefix, suffix string) string {
	base := filepath.Base(path)
	if !strings.HasPrefix(base, prefix) || !strings.HasSuffix(base, suffix) {
		return ""
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(base, prefix), suffix)
	return mid
}

// load 读取某 owner 的全部项目（按 created_at 降序）。
func (ps *projectStore) load(owner string) ([]project, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	data, err := os.ReadFile(ps.pathFor(owner))
	if err != nil {
		if os.IsNotExist(err) {
			return []project{}, nil
		}
		return nil, err
	}
	var list []project
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// save 原子写盘（tmp + rename，与 session 元数据一致）。
func (ps *projectStore) save(owner string, list []project) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt > list[j].CreatedAt })
	data, err := json.Marshal(list)
	if err != nil {
		return err
	}
	tmp := ps.pathFor(owner) + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, ps.pathFor(owner))
}

func (ps *projectStore) List(owner string) ([]project, error) { return ps.load(owner) }

func (ps *projectStore) Get(owner, id string) (*project, error) {
	list, err := ps.load(owner)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == id {
			return &list[i], nil
		}
	}
	return nil, nil
}

func (ps *projectStore) Upsert(p *project) error {
	list, err := ps.load(p.Owner)
	if err != nil {
		return err
	}
	replaced := false
	for i := range list {
		if list[i].ID == p.ID {
			list[i] = *p
			replaced = true
			break
		}
	}
	if !replaced {
		list = append(list, *p)
	}
	return ps.save(p.Owner, list)
}

func (ps *projectStore) Delete(owner, id string) (bool, error) {
	list, err := ps.load(owner)
	if err != nil {
		return false, err
	}
	kept := list[:0]
	found := false
	for i := range list {
		if list[i].ID == id {
			found = true
			continue
		}
		kept = append(kept, list[i])
	}
	if !found {
		return false, nil
	}
	return true, ps.save(owner, kept)
}

func newProjectID() string {
	return time.Now().Format("20060102_150405.000")
}

// ownerScope 解析当前请求的 owner；返回是否 admin（admin 可见全部分片）。
func (m *mcpCapture) ownerScope(ctx context.Context) (owner string, all bool) {
	if p, ok := auth.PrincipalFrom(ctx); ok {
		return p.Owner, p.IsAdmin
	}
	return auth.OwnerFrom(ctx), false
}

// handleProjectArgPort 读取可选的 port 参数（0=未设置）。
func handleProjectArgPort(req mcp.CallToolRequest) int {
	if args := req.GetArguments(); args != nil {
		if v, ok := args["port"]; ok && v != nil {
			var p int
			if err := json.Unmarshal([]byte(fmt.Sprintf("%v", v)), &p); err == nil {
				return p
			}
		}
	}
	return 0
}

// handleCreateProject 新建项目。
func (m *mcpCapture) handleCreateProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	owner, _ := m.ownerScope(ctx)
	name := strings.TrimSpace(req.GetString("name", ""))
	if name == "" {
		return errorResult(fmt.Errorf("name is required")), nil
	}
	plugin := strings.TrimSpace(req.GetString("plugin", ""))
	port := handleProjectArgPort(req)

	now := time.Now().Format(time.RFC3339)
	p := project{
		ID:        newProjectID(),
		Name:      name,
		Owner:     owner,
		Plugin:    plugin,
		Port:      port,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := m.projects.Upsert(&p); err != nil {
		return nil, fmt.Errorf("create project: %w", err)
	}
	slog.Info("project created", "owner", owner, "project_id", p.ID, "name", name)
	return successResult(p), nil
}

// handleListProjects 列出当前可见项目（admin 可见全部；普通用户只见自己的）。
func (m *mcpCapture) handleListProjects(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	owner, all := m.ownerScope(ctx)
	out := []project{}
	if all {
		// admin：汇总当前 owner + 各 projects.*.json 分片。
		seen := map[string]struct{}{}
		roots := []string{owner}
		if files, err := filepath.Glob(filepath.Join(m.projects.workDir, "projects.*.json")); err == nil {
			for _, f := range files {
				if o := ownerFromProjectShardFile(f, "projects.", ".json"); o != "" {
					roots = append(roots, o)
				}
			}
		}
		for _, o := range roots {
			list, err := m.projects.List(o)
			if err != nil {
				continue
			}
			for _, p := range list {
				if _, dup := seen[p.ID]; dup {
					continue
				}
				seen[p.ID] = struct{}{}
				out = append(out, p)
			}
		}
	} else {
		var err error
		out, err = m.projects.List(owner)
		if err != nil {
			return nil, fmt.Errorf("list projects: %w", err)
		}
	}
	return successResult(map[string]any{"projects": out}), nil
}

// handleUpdateProject 更新项目（仅更新显式提供的字段）。
func (m *mcpCapture) handleUpdateProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	owner, _ := m.ownerScope(ctx)
	id := strings.TrimSpace(req.GetString("id", ""))
	if id == "" {
		return errorResult(fmt.Errorf("id is required")), nil
	}
	existing, err := m.projects.Get(owner, id)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	if existing == nil {
		return errorResult(fmt.Errorf("project not found")), nil
	}
	if n := strings.TrimSpace(req.GetString("name", "")); n != "" {
		existing.Name = n
	}
	if args := req.GetArguments(); args != nil {
		if _, ok := args["plugin"]; ok {
			existing.Plugin = strings.TrimSpace(req.GetString("plugin", ""))
		}
		if _, ok := args["port"]; ok {
			existing.Port = handleProjectArgPort(req)
		}
	}
	existing.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := m.projects.Upsert(existing); err != nil {
		return nil, fmt.Errorf("update project: %w", err)
	}
	slog.Info("project updated", "owner", owner, "project_id", id)
	return successResult(existing), nil
}

// handleDeleteProject 删除项目。
func (m *mcpCapture) handleDeleteProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	owner, _ := m.ownerScope(ctx)
	id := strings.TrimSpace(req.GetString("id", ""))
	if id == "" {
		return errorResult(fmt.Errorf("id is required")), nil
	}
	ok, err := m.projects.Delete(owner, id)
	if err != nil {
		return nil, fmt.Errorf("delete project: %w", err)
	}
	if !ok {
		return errorResult(fmt.Errorf("project not found")), nil
	}
	slog.Info("project deleted", "owner", owner, "project_id", id)
	return successResult(map[string]any{"id": id}), nil
}