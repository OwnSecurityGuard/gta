package main

// invite_flow_test.go — 邀请制身份发放的端到端测试（2026-09-05）：
// create_access_code(new_owner) → /access/claim → users 表新身份 + 独立 token 即时可用。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/grpc"

	"gametrace/pkg/auth"
	pb "gametrace/pkg/internalipc/proto"
	"gametrace/pkg/store"
)

// fakeInviteClient 覆写 StartCapture / GetRegistryAddr，其余走 nil 嵌入（不会调用）。
type fakeInviteClient struct {
	pb.CaptureControlClient
	startReq   *pb.StartCaptureRequest
	registryQA string
}

func (f *fakeInviteClient) StartCapture(_ context.Context, in *pb.StartCaptureRequest, _ ...grpc.CallOption) (*pb.StartCaptureResponse, error) {
	f.startReq = in
	return &pb.StartCaptureResponse{SessionId: "s-invite-1", DbPath: "E:/tmp/capture.sqlite"}, nil
}

func (f *fakeInviteClient) GetRegistryAddr(_ context.Context, _ *pb.GetRegistryAddrRequest, _ ...grpc.CallOption) (*pb.GetRegistryAddrResponse, error) {
	return &pb.GetRegistryAddrResponse{RegistryAddr: f.registryQA}, nil
}

func ctxOwnerAdmin(owner string) context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{Owner: owner, IsAdmin: true})
}

func reqWith(kv ...string) mcp.CallToolRequest {
	req := mcp.CallToolRequest{}
	args := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		args[kv[i]] = kv[i+1]
	}
	req.Params.Arguments = args
	return req
}

// newInviteMCP 构造带 users/accessCodes/projectStore/pipeline 桩的完整 mcpCapture。
func newInviteMCP(t *testing.T) (*mcpCapture, *userStore, *fakeInviteClient) {
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
	us := newUserStore(cs.DB())
	if err := us.Init(); err != nil {
		t.Fatal(err)
	}
	ac := newAccessCodeStore(cs.DB())
	if err := ac.Init(); err != nil {
		t.Fatal(err)
	}
	fc := &fakeInviteClient{}
	m := &mcpCapture{
		projects: ps, users: us, authz: newProjectAuthorizer(ps),
		accessCodes: ac, tokensByOwner: map[string]string{"bob": "tok-bob"},
		pipelineClient: fc, sessionMgr: newSessionManager(t.TempDir()),
	}
	return m, us, fc
}

// TestInviteFlowCreatesIndependentIdentity 全链路：bob 发邀请 → carol claim →
// carol 获得独立 token（users 表精确匹配）、会话归属 carol 而非 bob。
func TestInviteFlowCreatesIndependentIdentity(t *testing.T) {
	m, us, fc := newInviteMCP(t)
	ctx := ctxOwner("bob")

	// bob 为 carol 发邀请码。
	creq := reqWith("new_owner", "carol")
	res, err := m.handleCreateAccessCode(ctx, creq)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Ok     bool   `json:"ok"`
		Code   string `json:"code"`
		Invite bool   `json:"invite"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &created); err != nil || !created.Ok || !created.Invite {
		t.Fatalf("create invite failed: %v %s", err, resultText(t, res))
	}

	// carol 的目标机 claim。
	req := httptest.NewRequest(http.MethodGet, "/access/claim?code="+created.Code, nil)
	w := httptest.NewRecorder()
	m.handleAccessClaim(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("claim failed: %d %s", w.Code, w.Body.String())
	}
	var cfg struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil || cfg.Token == "" {
		t.Fatalf("claim response missing token: %v %s", err, w.Body.String())
	}
	if cfg.Token == "tok-bob" {
		t.Fatal("invite must not hand out the inviter's token")
	}

	// 会话归属新身份。
	if fc.startReq == nil || fc.startReq.GetOwner() != "carol" {
		t.Fatalf("capture owner = %v, want carol", fc.startReq)
	}

	// users 表有 carol（created_by=bob），新 token 精确匹配（锁住 auth.DBResolver 的 SQL 契约）。
	users, err := us.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Owner != "carol" || users[0].CreatedBy != "bob" || users[0].IsAdmin {
		t.Fatalf("users after claim: %+v", users)
	}
	var owner string
	var admin int
	if err := us.db.QueryRow(`SELECT owner, is_admin FROM users WHERE token=?`, cfg.Token).
		Scan(&owner, &admin); err != nil || owner != "carol" {
		t.Fatalf("new token not resolvable: %v owner=%q", err, owner)
	}

	// 身份已存在 → 再发同名邀请被拒。
	res2, err := m.handleCreateAccessCode(ctx, creq)
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(t, res2); !strings.Contains(text, "already exists") {
		t.Errorf("duplicate invite must be rejected: %s", text)
	}
}

// TestRevokeUserHandlers 验证 list_users / revoke_user 的 admin 门禁与撤销语义。
func TestRevokeUserHandlers(t *testing.T) {
	m, us, _ := newInviteMCP(t)
	ctx := context.Background()
	if _, _, err := us.CreateUser(ctx, "dave", "bob"); err != nil {
		t.Fatal(err)
	}

	// 非 admin 调 list_users / revoke_user → 拒绝。
	for _, tc := range []struct {
		name string
		call func() (any, error)
	}{
		{"list_users", func() (any, error) { return m.handleListUsers(ctxOwner("bob"), mcp.CallToolRequest{}) }},
		{"revoke_user", func() (any, error) { return m.handleRevokeUser(ctxOwner("bob"), reqWith("owner", "dave")) }},
	} {
		res, err := tc.call()
		if err != nil {
			t.Fatal(err)
		}
		if text := resultText(t, res.(*mcp.CallToolResult)); !strings.Contains(text, "global admin") {
			t.Errorf("%s non-admin must be forbidden: %s", tc.name, text)
		}
	}

	// admin 撤销 dave → ok；再撤 → not found。
	res, err := m.handleRevokeUser(ctxOwnerAdmin("root"), reqWith("owner", "dave"))
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(t, res); strings.Contains(text, `"ok":false`) {
		t.Fatalf("admin revoke failed: %s", text)
	}
	if n := countUsers(t, us, "dave"); n != 0 {
		t.Fatalf("dave rows after revoke: %d", n)
	}
	res2, _ := m.handleRevokeUser(ctxOwnerAdmin("root"), reqWith("owner", "dave"))
	if text := resultText(t, res2); !strings.Contains(text, "not found") {
		t.Errorf("revoke twice must be not found: %s", text)
	}

	// admin 不能撤销自己。
	res3, _ := m.handleRevokeUser(ctxOwnerAdmin("root"), reqWith("owner", "root"))
	if text := resultText(t, res3); !strings.Contains(text, "cannot revoke yourself") {
		t.Errorf("self revoke must be rejected: %s", text)
	}
}

func countUsers(t *testing.T, us *userStore, owner string) int {
	t.Helper()
	var n int
	if err := us.db.QueryRow(`SELECT COUNT(*) FROM users WHERE owner=?`, owner).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestInviteNameValidation 验证 new_owner 格式约束（防命名空间歧义）。
func TestInviteNameValidation(t *testing.T) {
	m, _, _ := newInviteMCP(t)
	for _, name := range []string{"-bad", "has space", "a/b"} {
		res, err := m.handleCreateAccessCode(ctxOwner("bob"), reqWith("new_owner", name))
		if err != nil {
			t.Fatal(err)
		}
		if text := resultText(t, res); !strings.Contains(text, `"ok":false`) {
			t.Errorf("invalid new_owner %q must be rejected: %s", name, text)
		}
	}
	if !validOwnerName("alice.forest-x_2") {
		t.Error("reasonable name rejected")
	}
}
