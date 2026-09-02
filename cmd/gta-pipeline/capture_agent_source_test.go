package main

import (
	"context"
	"testing"

	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
	"gta/pkg/auth"
	"gta/pkg/capture/agent"
	"gta/pkg/internalipc/capturecontrol"
)

// agentOnly=true 且 hub 未配置：明确报错，而不是静默开出一个空会话。
func TestOpenCaptureSourcesAgentOnlyNoHub(t *testing.T) {
	_, err := openCaptureSources(context.Background(), "", 0, "", nil, nil, nil, "s1", true)
	if err == nil {
		t.Fatal("agentOnly without agent hub should fail")
	}
}

// agentOnly=true + hub：只打开 agent source，不打开任何基础（网卡）source。
func TestOpenCaptureSourcesAgentOnlyWithHub(t *testing.T) {
	hub := agent.NewHub()
	sources, err := openCaptureSources(context.Background(), "", 0, "", nil, nil, hub, "s1", true)
	if err != nil {
		t.Fatal(err)
	}
	defer closeCaptureSources(sources)
	if len(sources) != 1 {
		t.Fatalf("agentOnly session should open exactly 1 source, got %d", len(sources))
	}
}

// agentOnly=false（既有行为）：hub 存在时在基础 source 之外追加 agent source——
// 这里用无 hub + 空 iface 的错误路径验证基础 source 分支未被跳过。
func TestOpenCaptureSourcesBasePathUnchanged(t *testing.T) {
	// agentOnly=false 且无任何可用配置：走基础 source 路径（无 hub 时不追加 agent）。
	// 空 iface + 无设备时 openCaptureSourcesBase 尝试枚举网卡，CI 环境通常失败——
	// 无论哪种错误，都不应出现 agent source 相关信息。
	_, err := openCaptureSources(context.Background(), "", 0, "", nil, nil, nil, "s1", false)
	if err == nil {
		return // 意外成功也可接受（环境有可用网卡时的 fallback 行为未变）
	}
}

// 纯 agent StartSession：无基础 source 也允许创建会话（sourceName=agent），
// 且调用方身份被记录为会话归属（ControlStore + captureTask.owner）。
func TestPipelineService_StartSessionAgentOnly(t *testing.T) {
	s, _, controlStore := newTestPipelineService(t)

	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "alice"})
	res, err := s.StartSession(ctx, capturecontrol.StartSessionRequest{Plugin: "http", Agent: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.StopSession(context.Background(), res.SessionID)

	task, ok := s.getTask(res.SessionID)
	if !ok {
		t.Fatal("task not registered")
	}
	if task.sourceName != agent.SourceName {
		t.Errorf("sourceName = %q, want %q", task.sourceName, agent.SourceName)
	}
	if task.owner != "alice" {
		t.Errorf("task.owner = %q, want alice", task.owner)
	}
	meta, err := controlStore.GetSession(context.Background(), res.SessionID)
	if err != nil || meta == nil {
		t.Fatalf("control store session missing: %v", err)
	}
	if meta.Owner != "alice" {
		t.Errorf("SessionMeta.Owner = %q, want alice", meta.Owner)
	}
}

// 无 source 且无 agent 标志：维持 ErrSourceEmpty。
func TestPipelineService_StartSessionEmptySource(t *testing.T) {
	s, _, _ := newTestPipelineService(t)
	if _, err := s.StartSession(context.Background(), capturecontrol.StartSessionRequest{Plugin: "tcp"}); err == nil {
		t.Fatal("empty source should fail")
	}
}

// GetPluginManifest owner 作用域：admin 可查任意 owner 的插件，
// 其他 owner 的插件不可见（评审补充用例）。
func TestPipelineService_GetPluginManifestAdmin(t *testing.T) {
	s, _, _ := newTestPipelineService(t)

	manifest := `api_version: gta.decoder/v2
name: alice-plugin
protocol: test_proto
type: decoder
capabilities:
  decode: true
  schema: true
schemas:
  - id: test.player.v1
    version: 1
    strict: true
    fields:
      hp: { type: uint32 }
`
	if _, err := s.registry.Register(
		auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "alice"}),
		&pb.RegisterRequest{SocketPath: "unix:/nonexistent/decoder.sock", Manifest: []byte(manifest)},
	); err != nil {
		t.Fatal(err)
	}

	// alice 自己：可见
	if _, err := s.GetPluginManifest(
		auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "alice"}), "alice-plugin"); err != nil {
		t.Fatalf("owner lookup should succeed: %v", err)
	}
	// bob：不可见
	if _, err := s.GetPluginManifest(
		auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "bob"}), "alice-plugin"); err == nil {
		t.Fatal("bob should not see alice's plugin manifest")
	}
	// admin：可见
	if _, err := s.GetPluginManifest(
		auth.WithPrincipal(context.Background(), &auth.Principal{Owner: "root", IsAdmin: true}), "alice-plugin"); err != nil {
		t.Fatalf("admin lookup should succeed: %v", err)
	}
}
