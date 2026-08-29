package capturecontrol

import (
	"context"
	"testing"

	"gta/pkg/auth"
	pb "gta/pkg/internalipc/proto"
)

// ownerCapturingEngine 记录 engine 侧收到的调用方身份（ctx 注入结果）。
type ownerCapturingEngine struct {
	fakeEngine
	owner      string
	allOwners  bool
	agentFlag  bool
	agentSeen  bool
	pluginName string
}

func (f *ownerCapturingEngine) StartSession(ctx context.Context, req StartSessionRequest) (StartSessionResult, error) {
	if p, ok := auth.PrincipalFrom(ctx); ok {
		f.owner, f.allOwners = p.Owner, p.IsAdmin
	}
	f.agentSeen = req.Agent
	f.pluginName = req.Plugin
	return StartSessionResult{SessionID: "s1"}, nil
}

func (f *ownerCapturingEngine) ListPlugins(ctx context.Context) ([]PluginSummary, error) {
	if p, ok := auth.PrincipalFrom(ctx); ok {
		f.owner, f.allOwners = p.Owner, p.IsAdmin
	}
	return []PluginSummary{{Name: "http", Owner: "alice", Online: true}}, nil
}

func (f *ownerCapturingEngine) GetPluginManifest(ctx context.Context, name string) ([]byte, error) {
	if p, ok := auth.PrincipalFrom(ctx); ok {
		f.owner, f.allOwners = p.Owner, p.IsAdmin
	}
	return []byte("name: http"), nil
}

// StartCapture 透传 owner 身份 + agent 标志，engine 侧经 auth.PrincipalFrom 取回。
func TestServer_StartCaptureThreadsOwnerAndAgent(t *testing.T) {
	engine := &ownerCapturingEngine{}
	srv := NewServer(engine)
	if _, err := srv.StartCapture(context.Background(), &pb.StartCaptureRequest{
		Plugin: "http",
		Agent:  true,
		Owner:  "alice",
	}); err != nil {
		t.Fatal(err)
	}
	if engine.owner != "alice" || engine.allOwners {
		t.Errorf("owner not injected into engine ctx: owner=%q admin=%v", engine.owner, engine.allOwners)
	}
	if !engine.agentSeen {
		t.Error("agent flag not threaded to StartSessionRequest")
	}

	// 匿名（空 owner / 非 admin）：不注入身份
	engine2 := &ownerCapturingEngine{}
	if _, err := NewServer(engine2).StartCapture(context.Background(), &pb.StartCaptureRequest{Plugin: "tcp"}); err != nil {
		t.Fatal(err)
	}
	if engine2.owner != "" || engine2.allOwners {
		t.Errorf("anonymous request should not inject principal: owner=%q admin=%v", engine2.owner, engine2.allOwners)
	}
}

// ListPlugins 透传 owner 身份，并在响应中带回 Owner 字段。
func TestServer_ListPluginsThreadsOwner(t *testing.T) {
	engine := &ownerCapturingEngine{}
	srv := NewServer(engine)
	resp, err := srv.ListPlugins(context.Background(), &pb.ListPluginsRequest{Owner: "alice", AllOwners: true})
	if err != nil {
		t.Fatal(err)
	}
	if engine.owner != "alice" || !engine.allOwners {
		t.Errorf("owner not injected into engine ctx: owner=%q admin=%v", engine.owner, engine.allOwners)
	}
	if len(resp.GetPlugins()) != 1 || resp.GetPlugins()[0].GetOwner() != "alice" {
		t.Errorf("owner missing in response: %+v", resp.GetPlugins())
	}
}

// GetPluginManifest 透传 owner 身份。
func TestServer_GetPluginManifestThreadsOwner(t *testing.T) {
	engine := &ownerCapturingEngine{}
	srv := NewServer(engine)
	if _, err := srv.GetPluginManifest(context.Background(), &pb.GetPluginManifestRequest{Name: "http", Owner: "bob"}); err != nil {
		t.Fatal(err)
	}
	if engine.owner != "bob" {
		t.Errorf("owner not injected into engine ctx: %q", engine.owner)
	}
}
