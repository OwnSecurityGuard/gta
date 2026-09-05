package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"gametrace/pkg/auth"
	"gametrace/pkg/store"
)

// newProjectMCP 构造带真实 projectStore + ControlStore + projectAuthorizer 的 mcpCapture。
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
	return &mcpCapture{projects: ps, controlStore: cs, authz: newProjectAuthorizer(ps)}, cs
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

	// bob（member）尝试 add_project_member → 拒绝。
	// 语义（2026-09-05）：不可见统一按 not found 处理，不泄露项目存在性。
	breq := mcp.CallToolRequest{}
	breq.Params.Arguments = map[string]any{"project_id": id, "user": "carol", "role": "member"}
	bres, err := m.handleAddProjectMember(ctxOwner("bob"), breq)
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(t, bres); !strings.Contains(text, "not found") {
		t.Errorf("bob add_project_member should be rejected: %s", text)
	}

	// bob 尝试 set_project_plugins → 拒绝。
	preq := mcp.CallToolRequest{}
	preq.Params.Arguments = map[string]any{"project_id": id, "plugins": `[{"id":"p1","name":"http"}]`}
	pres, err := m.handleSetProjectPlugins(ctxOwner("bob"), preq)
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(t, pres); !strings.Contains(text, "not found") {
		t.Errorf("bob set_project_plugins should be rejected: %s", text)
	}
}

// TestProjectSessionCollaborationBoundary 验证项目是协作边界（方案 D1-A）：
// 项目成员能看到项目内他人创建的会话；项目外用户不能。
func TestProjectSessionCollaborationBoundary(t *testing.T) {
	m, cs := newProjectMCP(t)
	ctx := context.Background()
	id := createProjectAs(t, m, "alice")

	// alice 添加 bob 为 member。
	areq := mcp.CallToolRequest{}
	areq.Params.Arguments = map[string]any{"project_id": id, "user": "bob", "role": "member"}
	if _, err := m.handleAddProjectMember(ctxOwner("alice"), areq); err != nil {
		t.Fatal(err)
	}

	// alice（owner）创建会话并绑到项目。
	sessionID := "s-collab-1"
	if err := cs.CreateSession(ctx, store.SessionMeta{
		Owner: "alice", SessionID: sessionID, Status: "running", Plugin: "http",
	}); err != nil {
		t.Fatal(err)
	}
	mreq := mcp.CallToolRequest{}
	mreq.Params.Arguments = map[string]any{"session_id": sessionID, "project_id": id}
	if _, err := m.handleMoveSessionToProject(ctxOwner("alice"), mreq); err != nil {
		t.Fatal(err)
	}

	// bob（member）get_project 的 recent_sessions 应包含 alice 的会话。
	greq := mcp.CallToolRequest{}
	greq.Params.Arguments = map[string]any{"id": id}
	gres, err := m.handleGetProject(ctxOwner("bob"), greq)
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(t, gres); !strings.Contains(text, sessionID) {
		t.Errorf("member bob must see project sessions (collab boundary): %s", text)
	}

	// tom（非成员）get_project → not found。
	tres, err := m.handleGetProject(ctxOwner("tom"), greq)
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(t, tres); !strings.Contains(text, "not found") {
		t.Errorf("tom must not see project: %s", text)
	}
}

// TestTransferProjectOwnerFlow 验证 transfer_project_owner 工具流：
// owner 可转移给项目内成员；新 owner 生效、旧 owner 降为 admin 成员；
// member 无权发起转移；目标必须是项目内成员。
func TestTransferProjectOwnerFlow(t *testing.T) {
	m, _ := newProjectMCP(t)
	id := createProjectAs(t, m, "alice")

	// alice 添加 bob（admin）、carol（member）。
	for u, r := range map[string]string{"bob": "admin", "carol": "member"} {
		req := mcp.CallToolRequest{}
		req.Params.Arguments = map[string]any{"project_id": id, "user": u, "role": r}
		if _, err := m.handleAddProjectMember(ctxOwner("alice"), req); err != nil {
			t.Fatal(err)
		}
	}

	// carol（member）无权发起转移。
	creq := mcp.CallToolRequest{}
	creq.Params.Arguments = map[string]any{"project_id": id, "new_owner": "carol"}
	res, err := m.handleTransferProjectOwner(ctxOwner("carol"), creq)
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(t, res); !strings.Contains(text, "not found") {
		t.Errorf("member transfer must be rejected: %s", text)
	}

	// 目标是项目外用户（tom）→ 拒绝。
	treq := mcp.CallToolRequest{}
	treq.Params.Arguments = map[string]any{"project_id": id, "new_owner": "tom"}
	res, err = m.handleTransferProjectOwner(ctxOwner("alice"), treq)
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(t, res); !strings.Contains(text, "must be an existing member") {
		t.Errorf("outsider cannot become owner: %s", text)
	}

	// alice → bob 转移成功。
	breq := mcp.CallToolRequest{}
	breq.Params.Arguments = map[string]any{"project_id": id, "new_owner": "bob"}
	res, err = m.handleTransferProjectOwner(ctxOwner("alice"), breq)
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(t, res); strings.Contains(text, `"ok":false`) {
		t.Fatalf("transfer failed: %s", text)
	}
	p, err := m.projects.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if p.Owner != "bob" {
		t.Errorf("owner = %q, want bob", p.Owner)
	}
	// bob 现在可以删除项目（owner 全权），alice 降为 admin 仍可管成员但不能删。
	dreq := mcp.CallToolRequest{}
	dreq.Params.Arguments = map[string]any{"id": "nonexistent"}
	_ = dreq // 删除路径在 TestProjectDeleteByOwnerOnly 中验证
	foundAlice := false
	foundBob := false
	for _, mm := range p.Members {
		if mm.User == "alice" && mm.Role == roleAdmin {
			foundAlice = true
		}
		if mm.User == "bob" {
			foundBob = true
		}
	}
	if !foundAlice {
		t.Errorf("previous owner alice should be demoted to admin member: %+v", p.Members)
	}
	if foundBob {
		t.Errorf("new owner bob should not sit in members table: %+v", p.Members)
	}
}

// TestProjectDeleteByOwnerOnly 验证删除项目仅 Owner / global admin：
// project admin 成员可管成员，但不能删除项目。
func TestProjectDeleteByOwnerOnly(t *testing.T) {
	m, _ := newProjectMCP(t)
	id := createProjectAs(t, m, "alice")

	// bob 为 admin 成员。
	areq := mcp.CallToolRequest{}
	areq.Params.Arguments = map[string]any{"project_id": id, "user": "bob", "role": "admin"}
	if _, err := m.handleAddProjectMember(ctxOwner("alice"), areq); err != nil {
		t.Fatal(err)
	}

	// bob（admin）可加成员，但删除项目被拒。
	dreq := mcp.CallToolRequest{}
	dreq.Params.Arguments = map[string]any{"project_id": id, "user": "carol", "role": "member"}
	if _, err := m.handleAddProjectMember(ctxOwner("bob"), dreq); err != nil {
		t.Fatal(err)
	}
	delreq := mcp.CallToolRequest{}
	delreq.Params.Arguments = map[string]any{"id": id}
	res, err := m.handleDeleteProject(ctxOwner("bob"), delreq)
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(t, res); !strings.Contains(text, "not found") {
		t.Errorf("project admin must not delete project: %s", text)
	}

	// alice（owner）删除成功。
	res, err = m.handleDeleteProject(ctxOwner("alice"), delreq)
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(t, res); strings.Contains(text, `"ok":false`) {
		t.Fatalf("owner delete failed: %s", text)
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
	} else if text := resultText(t, res); !strings.Contains(text, "not found") {
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
