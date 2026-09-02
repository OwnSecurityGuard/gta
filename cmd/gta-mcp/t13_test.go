package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/grpc"

	"gta/pkg/auth"
	pb "gta/pkg/internalipc/proto"
	"gta/pkg/store"
)

// ---- fake 扩展：StartCapture / ListPlugins / GetPluginManifest ----

func (f *fakeCaptureClient) StartCapture(ctx context.Context, in *pb.StartCaptureRequest, _ ...grpc.CallOption) (*pb.StartCaptureResponse, error) {
	f.startReq = in
	return &pb.StartCaptureResponse{SessionId: "s-agent", State: "running", DbPath: filepath.Join(f.dbDir, "capture.sqlite")}, nil
}

func (f *fakeCaptureClient) ListPlugins(ctx context.Context, in *pb.ListPluginsRequest, _ ...grpc.CallOption) (*pb.ListPluginsResponse, error) {
	f.listPluginsReq = in
	return &pb.ListPluginsResponse{Plugins: []*pb.PluginSummary{
		{Name: "http", Online: true, Owner: "alice"},
	}}, nil
}

func (f *fakeCaptureClient) GetPluginManifest(ctx context.Context, in *pb.GetPluginManifestRequest, _ ...grpc.CallOption) (*pb.GetPluginManifestResponse, error) {
	f.manifestReq = in
	return &pb.GetPluginManifestResponse{Name: in.GetName(), Manifest: []byte("name: http")}, nil
}

// ---- T13: start_capture source=agent 参数流 ----

func TestStartCaptureAgentSource(t *testing.T) {
	fc := &fakeCaptureClient{dbDir: t.TempDir()}
	m := &mcpCapture{pipelineClient: fc, sessionMgr: newSessionManager(t.TempDir())}

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"source": "agent", "plugin": "http"}

	res, err := m.handleStartCapture(auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "alice"}), req)
	if err != nil {
		t.Fatal(err)
	}
	if fc.startReq == nil {
		t.Fatal("StartCapture not called")
	}
	if !fc.startReq.GetAgent() {
		t.Error("agent=true not set on StartCaptureRequest")
	}
	if fc.startReq.GetSource() != nil {
		t.Errorf("pure agent session should not carry a base source, got %T", fc.startReq.GetSource())
	}
	if fc.startReq.GetOwner() != "alice" {
		t.Errorf("owner not threaded to pipeline: %q", fc.startReq.GetOwner())
	}
	if text := res.Content[0].(mcp.TextContent).Text; !strings.Contains(text, `"source":"agent"`) {
		t.Errorf("result missing source=agent: %s", text)
	}
}

// source=nic（默认）行为不变：agent=false 且走 live source。
func TestStartCaptureNicSourceUnchanged(t *testing.T) {
	fc := &fakeCaptureClient{dbDir: t.TempDir()}
	m := &mcpCapture{pipelineClient: fc, sessionMgr: newSessionManager(t.TempDir())}

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"port": 8080}

	if _, err := m.handleStartCapture(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if fc.startReq == nil || fc.startReq.GetAgent() {
		t.Error("nic source must not set agent=true")
	}
	if fc.startReq.GetLive() == nil {
		t.Error("nic source must set live source")
	}
}

// 不支持的 source 依旧拒绝。
func TestStartCaptureInvalidSource(t *testing.T) {
	fc := &fakeCaptureClient{dbDir: t.TempDir()}
	m := &mcpCapture{pipelineClient: fc, sessionMgr: newSessionManager(t.TempDir())}

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"source": "bogus"}
	res, _ := m.handleStartCapture(context.Background(), req)
	if text := res.Content[0].(mcp.TextContent).Text; !strings.Contains(text, "unsupported source") {
		t.Errorf("want unsupported source error, got: %s", text)
	}
}

// ---- T13: list_registered_plugins / get_plugin_manifest owner 透传 ----

func TestListRegisteredPluginsOwnerThreaded(t *testing.T) {
	fc := &fakeCaptureClient{dbDir: t.TempDir()}
	m := &mcpCapture{pipelineClient: fc}

	// 非 admin：透传 owner
	res, _ := m.handleListRegisteredPlugins(
		auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "alice"}), mcp.CallToolRequest{})
	if fc.listPluginsReq == nil || fc.listPluginsReq.GetOwner() != "alice" || fc.listPluginsReq.GetAllOwners() {
		t.Fatalf("owner not threaded: %+v", fc.listPluginsReq)
	}
	if text := res.Content[0].(mcp.TextContent).Text; !strings.Contains(text, `"owner":"alice"`) {
		t.Errorf("output missing owner field: %s", text)
	}

	// admin：all_owners=true
	fc.listPluginsReq = nil
	if _, err := m.handleListRegisteredPlugins(
		auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "root", IsAdmin: true}), mcp.CallToolRequest{}); err != nil {
		t.Fatal(err)
	}
	if !fc.listPluginsReq.GetAllOwners() {
		t.Error("admin should set all_owners")
	}

	// 匿名（无身份）：空 owner / all_owners=false
	fc.listPluginsReq = nil
	if _, err := m.handleListRegisteredPlugins(context.Background(), mcp.CallToolRequest{}); err != nil {
		t.Fatal(err)
	}
	if fc.listPluginsReq.GetOwner() != "" || fc.listPluginsReq.GetAllOwners() {
		t.Errorf("anonymous should send empty owner: %+v", fc.listPluginsReq)
	}
}

func TestGetPluginManifestOwnerThreaded(t *testing.T) {
	fc := &fakeCaptureClient{dbDir: t.TempDir()}
	m := &mcpCapture{pipelineClient: fc}

	if _, err := m.handleGetPluginManifest(
		auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "bob"}), mcp.CallToolRequest{}); err != nil {
		t.Fatal(err)
	}
	if fc.manifestReq == nil || fc.manifestReq.GetOwner() != "bob" {
		t.Fatalf("manifest owner not threaded: %+v", fc.manifestReq)
	}
}

// ---- T13: getDBPath owner 校验（metadata.json 路径） ----

func newTestCaptureWithSession(t *testing.T, owner, sessionID string) (*mcpCapture, string) {
	t.Helper()
	workDir := t.TempDir()
	m := &mcpCapture{sessionMgr: newSessionManager(workDir)}
	dbPath := filepath.Join(workDir, "sessions", sessionID, "capture.sqlite")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.sessionMgr.writeSessionMetadata(sessionID, sessionMetadata{
		Owner: owner, SessionID: sessionID, Status: "stopped", DBPath: dbPath,
	}); err != nil {
		t.Fatal(err)
	}
	return m, dbPath
}

func TestGetDBPathOwnerAllowed(t *testing.T) {
	m, dbPath := newTestCaptureWithSession(t, "alice", "s1")
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "alice"})
	got, err := m.getDBPath(ctx, "s1")
	if err != nil || got != dbPath {
		t.Fatalf("getDBPath(alice) = %q, %v; want %q", got, err, dbPath)
	}
}

func TestGetDBPathOwnerDenied(t *testing.T) {
	m, _ := newTestCaptureWithSession(t, "alice", "s1")
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "bob"})
	got, err := m.getDBPath(ctx, "s1")
	if err == nil && got != "" {
		t.Fatalf("getDBPath(bob on alice's session) = %q, want denied/empty", got)
	}
}

func TestGetDBPathAdminBypass(t *testing.T) {
	m, dbPath := newTestCaptureWithSession(t, "alice", "s1")
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "root", IsAdmin: true})
	if got, err := m.getDBPath(ctx, "s1"); err != nil || got != dbPath {
		t.Fatalf("admin getDBPath = %q, %v; want %q", got, err, dbPath)
	}
}

// 匿名（历史/单机）会话对匿名调用方可见——回归底线；
// 对带身份的调用方按 store.SessionOwnerFilter.Matches 语义不可见（owner 不匹配）。
func TestGetDBPathAnonymousSessionVisible(t *testing.T) {
	m, dbPath := newTestCaptureWithSession(t, "", "s0")
	if got, err := m.getDBPath(context.Background(), "s0"); err != nil || got != dbPath {
		t.Fatalf("anonymous getDBPath = %q, %v; want %q", got, err, dbPath)
	}
	alice := auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "alice"})
	if got, err := m.getDBPath(alice, "s0"); err == nil && got != "" {
		t.Fatalf("alice getDBPath on anonymous session = %q, want not visible", got)
	}
}

// ---- T13: getDBPath owner 校验（controlStore 路径） ----

func TestGetDBPathControlStoreOwnerFilter(t *testing.T) {
	workDir := t.TempDir()
	cs, err := store.NewControlStore(filepath.Join(workDir, "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	m := &mcpCapture{sessionMgr: newSessionManager(workDir), controlStore: cs}

	dbPath := filepath.Join(workDir, "sessions", "sc1", "capture.sqlite")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := cs.CreateSession(ctx, store.SessionMeta{
		Owner: "alice", SessionID: "sc1", Status: "stopped", DBPath: dbPath,
	}); err != nil {
		t.Fatal(err)
	}

	alice := auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "alice"})
	if got, err := m.getDBPath(alice, "sc1"); err != nil || got != dbPath {
		t.Fatalf("alice getDBPath = %q, %v; want %q", got, err, dbPath)
	}

	bob := auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "bob"})
	// controlStore 中存在但不可见 → 明确拒绝（不泄露 db_path，语义同
	// TestGetDBPathControlStoreRecordNoFSFallback）。
	if got, err := m.getDBPath(bob, "sc1"); err == nil {
		t.Fatalf("bob getDBPath on alice's session = %q, want deny error", got)
	}
}

// ---- T12: CORS + 鉴权中间件链 ----

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestCORSMiddlewareNoOriginsConfigured(t *testing.T) {
	h := buildHTTPHandler([]string{""}, nil, okHandler())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/mcp", nil))
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("no origins configured: must not emit Access-Control-Allow-Origin")
	}
	if rec.Header().Get("Access-Control-Expose-Headers") != "" {
		t.Error("no origins configured: must not emit Access-Control-Expose-Headers")
	}
}

func TestCORSMiddlewareAllowlist(t *testing.T) {
	h := buildHTTPHandler([]string{"http://good.example.com"}, nil, okHandler())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/mcp", nil)
	req.Header.Set("Origin", "http://good.example.com")
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://good.example.com" {
		t.Errorf("allowed origin ACAO = %q, want echoed origin", got)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/mcp", nil)
	req.Header.Set("Origin", "http://evil.example.com")
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("non-allowlisted origin must not get ACAO header")
	}
	if rec.Header().Get("Access-Control-Expose-Headers") != "" {
		t.Error("non-allowlisted origin must not get Access-Control-Expose-Headers")
	}
}

// TestCORSExposeIdentityHeaders 验证命中 allowlist 的跨域响应暴露身份回显头，
// 否则前端 JS 在跨域直连场景下读不到 X-GTA-Owner / X-GTA-Admin。
func TestCORSExposeIdentityHeaders(t *testing.T) {
	h := buildHTTPHandler([]string{"http://good.example.com"}, nil, okHandler())
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Origin", "http://good.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	expose := rec.Header().Get("Access-Control-Expose-Headers")
	if expose != auth.HeaderOwner+", "+auth.HeaderAdmin {
		t.Fatalf("应恰好暴露身份回显头，实际 %q", expose)
	}
}

func TestCORSMiddlewarePreflight(t *testing.T) {
	h := buildHTTPHandler([]string{"http://good.example.com"}, nil, okHandler())

	req := httptest.NewRequest("OPTIONS", "/mcp", nil)
	req.Header.Set("Origin", "http://good.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Errorf("preflight = %d, ACAO=%q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestAuthMiddlewareTokenMode(t *testing.T) {
	resolver := auth.NewStaticResolver(map[string]auth.Principal{
		"gta_alice": {Owner: "alice"},
	})
	var gotOwner string
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOwner = auth.OwnerFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := buildHTTPHandler(nil, resolver, base)

	// 无凭证 → 401
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/mcp", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing bearer = %d, want 401", rec.Code)
	}

	// 有效凭证 → 200 且身份注入 ctx
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer gta_alice")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || gotOwner != "alice" {
		t.Errorf("valid bearer = %d, owner=%q; want 200/alice", rec.Code, gotOwner)
	}
}

func TestAuthMiddlewareAnonymousModeUnchanged(t *testing.T) {
	resolver := auth.NewStaticResolver(nil) // 匿名模式
	var sawPrincipal bool
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawPrincipal = auth.PrincipalFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := buildHTTPHandler(nil, resolver, base)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/mcp", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous mode blocked request: %d", rec.Code)
	}
	if sawPrincipal {
		t.Error("anonymous mode must not inject a Principal (T12 前行为回归底线)")
	}
}

// admin token（":admin" 后缀）经完整 HTTP 中间件链后，handler 收到 IsAdmin 身份。
func TestAuthMiddlewareAdminToken(t *testing.T) {
	resolver, err := auth.ParseTokens("root=gta_root:admin,alice=gta_alice")
	if err != nil {
		t.Fatal(err)
	}
	var gotAdmin bool
	var gotOwner string
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := auth.PrincipalFrom(r.Context()); ok {
			gotAdmin, gotOwner = p.IsAdmin, p.Owner
		}
		w.WriteHeader(http.StatusOK)
	})
	h := buildHTTPHandler(nil, resolver, base)

	for _, tc := range []struct {
		token     string
		wantAdmin bool
		wantOwner string
	}{
		{"gta_root", true, "root"},
		{"gta_alice", false, "alice"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+tc.token)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || gotOwner != tc.wantOwner || gotAdmin != tc.wantAdmin {
			t.Errorf("token %q = %d owner=%q admin=%v; want 200/%s/%v",
				tc.token, rec.Code, gotOwner, gotAdmin, tc.wantOwner, tc.wantAdmin)
		}
	}
}

// 评审修复回归：controlStore 有他人会话记录、本地无 metadata.json（os.Stat
// 合成路径）时，匿名调用方不得通过 getDBPath 拿到他人会话的 db_path。
func TestGetDBPathControlStoreRecordNoFSFallback(t *testing.T) {
	workDir := t.TempDir()
	cs, err := store.NewControlStore(filepath.Join(workDir, "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	m := &mcpCapture{sessionMgr: newSessionManager(workDir), controlStore: cs}

	dbPath := filepath.Join(workDir, "sessions", "alice-s1", "capture.sqlite")
	// 伪造 alice 会话的 capture.sqlite 文件存在（无 metadata.json），
	// 复现 os.Stat 合成 Owner="" 元数据的泄露路径。
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := cs.CreateSession(ctx, store.SessionMeta{
		Owner: "alice", SessionID: "alice-s1", Status: "stopped", DBPath: dbPath,
	}); err != nil {
		t.Fatal(err)
	}

	// 匿名调用方：拒绝（不得经合成路径拿到 db_path）
	if got, err := m.getDBPath(context.Background(), "alice-s1"); err == nil && got != "" {
		t.Fatalf("anonymous getDBPath leaked db_path %q", got)
	}
	// bob：拒绝
	bob := auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "bob"})
	if got, err := m.getDBPath(bob, "alice-s1"); err == nil && got != "" {
		t.Fatalf("bob getDBPath leaked db_path %q", got)
	}
	// alice 本人：可见
	alice := auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "alice"})
	if got, err := m.getDBPath(alice, "alice-s1"); err != nil || got != dbPath {
		t.Fatalf("alice getDBPath = %q, %v; want %q", got, err, dbPath)
	}
}
