package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "gta/pkg/internalipc/proto"
	"gta/pkg/store"
)

// claimFakePipeline 仅实现 claim 需要的方法（StartCapture + GetRegistryAddr），
// 避免依赖共享 fakeCaptureClient（其后端接口为 nil，调用会 panic）。
type claimFakePipeline struct {
	pb.CaptureControlClient
	sessionID string
}

func (f *claimFakePipeline) StartCapture(_ context.Context, _ *pb.StartCaptureRequest, _ ...grpc.CallOption) (*pb.StartCaptureResponse, error) {
	return &pb.StartCaptureResponse{SessionId: f.sessionID, State: "running", DbPath: "s-claim/capture.sqlite"}, nil
}

func (f *claimFakePipeline) GetRegistryAddr(_ context.Context, _ *pb.GetRegistryAddrRequest, _ ...grpc.CallOption) (*pb.GetRegistryAddrResponse, error) {
	return &pb.GetRegistryAddrResponse{RegistryAddr: "192.168.1.10:9091"}, nil
}

func newClaimCapture(t *testing.T) (*mcpCapture, *accessCodeStore) {
	t.Helper()
	workDir := t.TempDir()
	cs, err := store.NewControlStore(filepath.Join(workDir, "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	store := newAccessCodeStore(cs.DB())
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	m := &mcpCapture{
		sessionMgr:     newSessionManager(workDir),
		accessCodes:    store,
		pipelineClient: &claimFakePipeline{sessionID: "s-claim"},
		tokensByOwner:  map[string]string{"alice": "tok_alice"},
	}
	return m, store
}

func TestAccessClaimReturnsConfig(t *testing.T) {
	m, store := newClaimCapture(t)
	if err := store.Create(context.Background(), &accessCode{
		Code: "GTA-ABC1-DEF2", Owner: "alice", Port: 8080, Plugin: "http",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/access/claim?code=GTA-ABC1-DEF2", nil)
	rec := httptest.NewRecorder()
	m.handleAccessClaim(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var cfg map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["server"] != "192.168.1.10:9091" {
		t.Fatalf("server = %v", cfg["server"])
	}
	if cfg["session"] != "s-claim" {
		t.Fatalf("session = %v", cfg["session"])
	}
	if cfg["token"] != "tok_alice" {
		t.Fatalf("token = %v", cfg["token"])
	}
	if cfg["bpf"] != "tcp port 8080 or udp port 8080" {
		t.Fatalf("bpf = %v", cfg["bpf"])
	}

	// 认领后标记 claimed，返回头带 session。
	if rec.Header().Get("X-Session-Id") != "s-claim" {
		t.Fatalf("X-Session-Id = %q", rec.Header().Get("X-Session-Id"))
	}
	got, _ := store.Get(context.Background(), "GTA-ABC1-DEF2")
	if !got.Claimed || got.SessionID != "s-claim" {
		t.Fatalf("code not marked claimed: %+v", got)
	}
}

func TestAccessClaimRejectsInvalidAndExpired(t *testing.T) {
	m, store := newClaimCapture(t)
	// 过期码
	if err := store.Create(context.Background(), &accessCode{
		Code: "GTA-OLD1-XXXX", Owner: "bob", ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	expReq := httptest.NewRequest(http.MethodGet, "/access/claim?code=GTA-OLD1-XXXX", nil)
	expRec := httptest.NewRecorder()
	m.handleAccessClaim(expRec, expReq)
	if expRec.Code != http.StatusGone {
		t.Fatalf("expired code: expected 410, got %d", expRec.Code)
	}

	// 未知码
	badReq := httptest.NewRequest(http.MethodGet, "/access/claim?code=GTA-NOPE-0000", nil)
	badRec := httptest.NewRecorder()
	m.handleAccessClaim(badRec, badReq)
	if badRec.Code != http.StatusNotFound {
		t.Fatalf("unknown code: expected 404, got %d", badRec.Code)
	}
}

func TestSetupScriptSnippet(t *testing.T) {
	m, _ := newClaimCapture(t)
	req := httptest.NewRequest(http.MethodGet, "/setup.sh?code=GTA-ABC1-DEF2", nil)
	rec := httptest.NewRecorder()
	m.handleSetupScript(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup.sh: expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !bytes.Contains([]byte(body), []byte(`CODE="GTA-ABC1-DEF2"`)) && !bytes.Contains([]byte(body), []byte("GTA-ABC1-DEF2")) {
		t.Fatalf("setup.sh missing code, got:\n%s", body)
	}
	if !bytes.Contains([]byte(body), []byte("/access/claim?code=")) {
		t.Fatalf("setup.sh missing claim url, got:\n%s", body)
	}
}

func TestSetupPS1ScriptSnippet(t *testing.T) {
	m, _ := newClaimCapture(t)
	req := httptest.NewRequest(http.MethodGet, "/setup.ps1?code=GTA-ABC1-DEF2&platform=windows/amd64", nil)
	rec := httptest.NewRecorder()
	m.handleSetupScriptPS1(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup.ps1: expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !bytes.Contains([]byte(body), []byte("GTA-ABC1-DEF2")) {
		t.Fatalf("setup.ps1 missing code, got:\n%s", body)
	}
	if !bytes.Contains([]byte(body), []byte("/access/claim?code=")) {
		t.Fatalf("setup.ps1 missing claim url, got:\n%s", body)
	}
	if !bytes.Contains([]byte(body), []byte("Invoke-RestMethod")) {
		t.Fatalf("setup.ps1 missing Invoke-RestMethod, got:\n%s", body)
	}
	if !bytes.Contains([]byte(body), []byte("Expand-Archive")) {
		t.Fatalf("setup.ps1 missing Expand-Archive, got:\n%s", body)
	}
}

func TestSetupPS1RequiresCode(t *testing.T) {
	m, _ := newClaimCapture(t)
	req := httptest.NewRequest(http.MethodGet, "/setup.ps1", nil)
	rec := httptest.NewRecorder()
	m.handleSetupScriptPS1(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("setup.ps1 without code: expected 400, got %d", rec.Code)
	}
}
