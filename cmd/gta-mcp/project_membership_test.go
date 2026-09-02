package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"gta/pkg/auth"
	"gta/pkg/store"
)

// newProjectMCP 构造带真实 projectStore + ControlStore 的 mcpCapture。
func newProjectMCP(t *testing.T) (*mcpCapture, *store.ControlStore) {
	t.Helper()
	cs, err := store.NewControlStore(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	ps := newProjectStoreDB(cs.DB())
	if err := ps.Init(); err != nil {
		t.Fatal(err)
	}
	return &mcpCapture{projects: ps, controlStore: cs}, cs
}

func ctxOwner(owner string) context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{Owner: owner})
}

// resultText 提取 CallToolResult 的文本内容。
func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatalf("empty result: %+v", res)
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("content is not text: %T", res.Content[0])
	}
	return tc.Text
}

// createProjectAs 以指定 owner 创建一个项目并返回其 id。
func createProjectAs(t *testing.T, m *mcpCapture, owner string) string {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"name": "Proj-" + owner}
	res, err := m.handleCreateProject(ctxOwner(owner), req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Ok bool   `json:"ok"`
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &out); err != nil || !out.Ok {
		t.Fatalf("create_project failed: %v text=%s", err, resultText(t, res))
	}
	return out.ID
}

// TestProjectCreateListGet 验证 create/list/get 基本流。
func TestProjectCreateListGet(t *testing.T) {
	m, _ := newProjectMCP(t)
	id := createProjectAs(t, m, "alice")

	// list_projects（alice 视角）应含该项目。
	lreq := mcp.CallToolRequest{}
	lres, err := m.handleListProjects(ctxOwner("alice"), lreq)
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(t, lres); !strings.Contains(text, id) {
		t.Errorf("list_projects missing project %s: %s", id, text)
	}

	// get_project（alice 视角）成功且回显 id。
	greq := mcp.CallToolRequest{}
	greq.Params.Arguments = map[string]any{"id": id}
	gres, err := m.handleGetProject(ctxOwner("alice"), greq)
	if err != nil {
		t.Fatal(err)
	}
	var gout struct {
		Ok bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(resultText(t, gres)), &gout); err != nil || !gout.Ok {
		t.Fatalf("get_project failed: %v text=%s", err, resultText(t, gres))
	}

	// tom（非成员）get_project 应按未找到处理。
	tres, err := m.handleGetProject(ctxOwner("tom"), greq)
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(t, tres); !strings.Contains(text, "project not found") {
		t.Errorf("tom get_project should be not found: %s", text)
	}
}

// TestProjectMemberCannotManage 验证普通成员无法 add_project_member / set_project_plugins。
func TestProjectMemberCannotManage(t *testing.T) {
	m, _ := newProjectMCP(t)
	id := createProjectAs(t, m, "alice")

	// alice 添加 bob 为成员。
	areq := mcp.CallToolRequest{}
	areq.Params.Arguments = map[string]any{"project_id": id, "user": "bob", "role": "member"}
	if res, err := m.handleAddProjectMember(ctxOwner("alice"), areq); err != nil {
		t.Fatal(err)
	} else if text := resultText(t, res); strings.Contains(text, `"ok":false`) {
		t.Fatalf("add member failed: %s", text)
	}

	// bob 尝试 add_project_member → forbidden。
	breq := mcp.CallToolRequest{}
	breq.Params.Arguments = map[string]any{"project_id": id, "user": "carol", "role": "member"}
	bres, err := m.handleAddProjectMember(ctxOwner("bob"), breq)
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(t, bres); !strings.Contains(text, "forbidden") {
		t.Errorf("bob add_project_member should be forbidden: %s", text)
	}

	// bob 尝试 set_project_plugins → forbidden。
	preq := mcp.CallToolRequest{}
	preq.Params.Arguments = map[string]any{"project_id": id, "plugins": `[{"id":"p1","name":"http"}]`}
	pres, err := m.handleSetProjectPlugins(ctxOwner("bob"), preq)
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(t, pres); !strings.Contains(text, "forbidden") {
		t.Errorf("bob set_project_plugins should be forbidden: %s", text)
	}
}

// TestProjectAdminRoleCanManage 验证 admin 角色成员具备管理权（P0 权限语义修复）：
// alice 把 bob 加为 admin 角色成员后，bob 可以设置插件、添加成员；
// member 角色成员（carol）仍不可管理。
func TestProjectAdminRoleCanManage(t *testing.T) {
	m, _ := newProjectMCP(t)
	id := createProjectAs(t, m, "alice")

	// alice 添加 bob 为 admin 角色成员、carol 为 member 角色成员。
	areq := mcp.CallToolRequest{}
	areq.Params.Arguments = map[string]any{"project_id": id, "user": "bob", "role": "admin"}
	if res, err := m.handleAddProjectMember(ctxOwner("alice"), areq); err != nil {
		t.Fatal(err)
	} else if text := resultText(t, res); strings.Contains(text, `"ok":false`) {
		t.Fatalf("add member failed: %s", text)
	}
	creq := mcp.CallToolRequest{}
	creq.Params.Arguments = map[string]any{"project_id": id, "user": "carol", "role": "member"}
	if _, err := m.handleAddProjectMember(ctxOwner("alice"), creq); err != nil {
		t.Fatal(err)
	}

	// bob（admin 角色）可以 set_project_plugins。
	preq := mcp.CallToolRequest{}
	preq.Params.Arguments = map[string]any{"project_id": id, "plugins": `[{"id":"http","name":"http"}]`}
	if res, err := m.handleSetProjectPlugins(ctxOwner("bob"), preq); err != nil {
		t.Fatal(err)
	} else if text := resultText(t, res); strings.Contains(text, "forbidden") {
		t.Errorf("bob (project admin role) should manage plugins: %s", text)
	}

	// bob（admin 角色）可以添加成员。
	dreq := mcp.CallToolRequest{}
	dreq.Params.Arguments = map[string]any{"project_id": id, "user": "dave", "role": "member"}
	if res, err := m.handleAddProjectMember(ctxOwner("bob"), dreq); err != nil {
		t.Fatal(err)
	} else if text := resultText(t, res); strings.Contains(text, "forbidden") {
		t.Errorf("bob (project admin role) should add members: %s", text)
	}

	// carol（member 角色）不可管理。
	erreq := mcp.CallToolRequest{}
	erreq.Params.Arguments = map[string]any{"project_id": id, "plugins": `[]`}
	if res, err := m.handleSetProjectPlugins(ctxOwner("carol"), erreq); err != nil {
		t.Fatal(err)
	} else if text := resultText(t, res); !strings.Contains(text, "forbidden") {
		t.Errorf("carol (member role) must not manage: %s", text)
	}
}

// TestProjectCreatorCanManage 验证创建者可添加成员、设置插件。
func TestProjectCreatorCanManage(t *testing.T) {
	m, _ := newProjectMCP(t)
	id := createProjectAs(t, m, "alice")

	// 创建者添加成员。
	areq := mcp.CallToolRequest{}
	areq.Params.Arguments = map[string]any{"project_id": id, "user": "bob", "role": "admin"}
	if res, err := m.handleAddProjectMember(ctxOwner("alice"), areq); err != nil {
		t.Fatal(err)
	} else if text := resultText(t, res); strings.Contains(text, `"ok":false`) {
		t.Fatalf("add member failed: %s", text)
	}

	// 创建者设置插件。
	preq := mcp.CallToolRequest{}
	preq.Params.Arguments = map[string]any{"project_id": id, "plugins": `[{"id":"pl1","name":"godot_gateway"}]`}
	if res, err := m.handleSetProjectPlugins(ctxOwner("alice"), preq); err != nil {
		t.Fatal(err)
	} else if text := resultText(t, res); strings.Contains(text, `"ok":false`) {
		t.Fatalf("set plugins failed: %s", text)
	}

	// 回读验证 plugins 已保存。
	p, err := m.projects.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Plugins) != 1 || p.Plugins[0].ID != "pl1" {
		t.Errorf("plugins not persisted: %+v", p.Plugins)
	}
	if len(p.Members) != 1 || p.Members[0].User != "bob" || p.Members[0].Role != roleAdmin {
		t.Errorf("members not persisted: %+v", p.Members)
	}
}

// TestSetSessionProjectRoundTrip 验证 set_session_project 更新会话绑定的往返。
func TestSetSessionProjectRoundTrip(t *testing.T) {
	m, cs := newProjectMCP(t)
	ctx := context.Background()
	sessionID := "s-proj-1"
	if err := cs.CreateSession(ctx, store.SessionMeta{
		Owner: "alice", SessionID: sessionID, Status: "running", Plugin: "http",
	}); err != nil {
		t.Fatal(err)
	}
	id := createProjectAs(t, m, "alice")

	sreq := mcp.CallToolRequest{}
	sreq.Params.Arguments = map[string]any{"session_id": sessionID, "project_id": id}
	if res, err := m.handleSetSessionProject(ctxOwner("alice"), sreq); err != nil {
		t.Fatal(err)
	} else if text := resultText(t, res); strings.Contains(text, `"ok":false`) {
		t.Fatalf("set_session_project failed: %s", text)
	}

	meta, err := cs.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ProjectID != id {
		t.Errorf("session project_id = %q, want %q", meta.ProjectID, id)
	}

	// get_project 的 recent_sessions 应包含该会话。
	greq := mcp.CallToolRequest{}
	greq.Params.Arguments = map[string]any{"id": id}
	gres, err := m.handleGetProject(ctxOwner("alice"), greq)
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(t, gres); !strings.Contains(text, sessionID) {
		t.Errorf("get_project recent_sessions missing session %s: %s", sessionID, text)
	}
}
