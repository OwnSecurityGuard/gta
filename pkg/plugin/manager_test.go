package plugin

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
)

// fakeDecoderServer 是一个最小化的 Decoder gRPC 服务桩，仅用于让 RegistryServer.Register
// 完成可达性拨号验证（Register 只建立连接，不实际调用 Decode）。
type fakeDecoderServer struct {
	pb.UnimplementedDecoderServer
}

func (fakeDecoderServer) DecodeV2(stream grpc.BidiStreamingServer[pb.DecodeRequest, pb.DecodeResponseV2]) error {
	return nil
}

// startFakeDecoder 启动一个监听 unix socket 的 Decoder 服务，返回 socket 路径与停止函数。
func startFakeDecoder(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "decoder.sock")
	_ = os.Remove(sock)
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	srv := grpc.NewServer()
	pb.RegisterDecoderServer(srv, fakeDecoderServer{})
	go func() { _ = srv.Serve(lis) }()
	stop := func() {
		srv.Stop()
		_ = os.Remove(sock)
	}
	return sock, stop
}

const testManifest = `api_version: gta.decoder/v2
name: test-decoder
protocol: test_proto
type: decoder
hints:
  - tcp
  - "port:7000"
`

// TestRegister_RejectsBadSemanticDeclaration 验证注册期六层语义校验：
// schema 声明非法（未知类型）必须被 PluginChecker.Check 拦下，
// 且发生在拨号验证之前（无需真实 decoder socket 即可断言）。
func TestRegister_RejectsBadSemanticDeclaration(t *testing.T) {
	bad := `api_version: gta.decoder/v2
name: bad-schema-decoder
protocol: test_proto
type: decoder
capabilities:
  decode: true
  schema: true
schemas:
  - id: test.player.v1
    version: 1
    fields:
      hp: { type: not_a_real_type }
`
	s := NewRegistryServer(10)
	_, err := s.Register(context.Background(), &pb.RegisterRequest{
		SocketPath: "unix:/nonexistent/decoder.sock",
		Manifest:   []byte(bad),
	})
	if err == nil {
		t.Fatal("Register should reject a manifest with an invalid schema field type")
	}
	if !strings.Contains(err.Error(), "semantic contract check failed") {
		t.Fatalf("error %q should be a semantic contract rejection", err.Error())
	}
}

// TestRegister_AcceptsSemanticDeclaration 验证合法的四层声明能通过注册校验：
// 语义层不拦截（grpc.Dial 为惰性连接，socket 不存在不影响注册结果），
// 注册成功并返回 instance_id。
func TestRegister_AcceptsSemanticDeclaration(t *testing.T) {
	good := `api_version: gta.decoder/v2
name: good-schema-decoder
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
      hp: { type: uint32, semantic: health, unit: hp, aggregatable: true }
`
	s := NewRegistryServer(10)
	resp, err := s.Register(context.Background(), &pb.RegisterRequest{
		SocketPath: "unix:/nonexistent/decoder.sock",
		Manifest:   []byte(good),
	})
	if err != nil {
		t.Fatalf("valid declaration should pass the semantic check, got: %v", err)
	}
	if resp.GetInstanceId() == "" {
		t.Fatal("expected non-empty instance_id")
	}
}

// TestRegistryServer_RegisterAndLifecycle 覆盖 Register 后的完整生命周期：
// Find（按 protocol / hint / 未知）、Heartbeat、List、CheckOffline、GetPluginManifest、Deregister。
func TestRegistryServer_RegisterAndLifecycle(t *testing.T) {
	sock, stop := startFakeDecoder(t)
	defer stop()

	s := NewRegistryServer(10)

	resp, err := s.Register(context.Background(), &pb.RegisterRequest{
		SocketPath: sock,
		Manifest:   []byte(testManifest),
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.InstanceId == "" {
		t.Fatalf("expected non-empty instance_id")
	}
	if resp.HeartbeatIntervalSec != 10 {
		t.Errorf("heartbeat interval = %d, want 10", resp.HeartbeatIntervalSec)
	}

	// Find by protocol
	if client, _, ok := s.Find("test_proto"); !ok || client == nil {
		t.Errorf("Find by protocol failed: ok=%v client=%v", ok, client)
	}
	// Find by hint
	if _, _, ok := s.Find("port:7000"); !ok {
		t.Errorf("Find by hint port:7000 failed")
	}
	// Find by unknown must not match
	if _, _, ok := s.Find("nope"); ok {
		t.Errorf("Find by unknown should not match")
	}

	// Heartbeat 刷新 LastHeartbeat
	if _, err := s.Heartbeat(context.Background(), &pb.HeartbeatRequest{InstanceId: resp.InstanceId}); err != nil {
		t.Errorf("Heartbeat: %v", err)
	}

	// List 应含 1 个在线插件
	if list := s.List(); len(list) != 1 || !list[0].Online {
		t.Errorf("List = %+v, want 1 online", list)
	}

	// 回填 LastHeartbeat 到过去，CheckOffline 应将其标记下线
	// （Windows 下 time.Now() 分辨率约 15ms，不能用亚毫秒超时断言）
	for _, rp := range s.plugins {
		rp.LastHeartbeat = time.Now().Add(-time.Minute)
	}
	s.CheckOffline(time.Second)
	if list := s.List(); len(list) != 1 || list[0].Online {
		t.Errorf("CheckOffline should mark offline, got %+v", list)
	}

	// GetPluginManifest 返回非空 bytes
	mani, err := s.GetPluginManifest("test-decoder")
	if err != nil {
		t.Fatalf("GetPluginManifest: %v", err)
	}
	if len(mani) == 0 {
		t.Errorf("GetPluginManifest returned empty")
	}

	// Deregister 后不再可被 Find 命中
	if _, err := s.Deregister(context.Background(), &pb.DeregisterRequest{InstanceId: resp.InstanceId}); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	if _, _, ok := s.Find("test_proto"); ok {
		t.Errorf("after Deregister, Find should not match")
	}
}

// TestRegistryServer_RegisterInvalidManifest 验证非法 manifest 在 parse/validate/version 阶段即报错。
func TestRegistryServer_RegisterInvalidManifest(t *testing.T) {
	s := NewRegistryServer(10)
	// api_version 格式非法
	_, err := s.Register(context.Background(), &pb.RegisterRequest{
		SocketPath: "/nonexistent.sock",
		Manifest:   []byte("api_version: bad\nname: x\nprotocol: p\ntype: decoder\n"),
	})
	if err == nil {
		t.Fatal("expected error for invalid api_version, got nil")
	}
}

// TestRegistryServer_RegisterVersionMismatch 验证 major 版本不匹配时注册被拒绝。
func TestRegistryServer_RegisterVersionMismatch(t *testing.T) {
	s := NewRegistryServer(10)
	mani := "api_version: gta.decoder/v1\nname: x\nprotocol: p\ntype: decoder\n"
	_, err := s.Register(context.Background(), &pb.RegisterRequest{
		SocketPath: "/nonexistent.sock",
		Manifest:   []byte(mani),
	})
	if err == nil {
		t.Fatal("expected version-mismatch error, got nil")
	}
}

// TestRegistryServer_FindByName 验证按插件名精确路由（GAP 2 核心能力）：
// 同协议（都声明 tcp）的两个插件可被按名区分，A 会话只命中 A、B 会话只命中 B，
// 未知名报 not-found，注销后名查失效，协议 hint 退化路径仍可用。
func TestRegistryServer_FindByName(t *testing.T) {
	sockA, stopA := startFakeDecoder(t)
	defer stopA()
	sockB, stopB := startFakeDecoder(t)
	defer stopB()

	const (
		maniA = "api_version: gta.decoder/v2\nname: a-decoder\nprotocol: tcp\ntype: decoder\n"
		maniB = "api_version: gta.decoder/v2\nname: b-decoder\nprotocol: tcp\ntype: decoder\n"
	)

	s := NewRegistryServer(10)
	if _, err := s.Register(context.Background(), &pb.RegisterRequest{SocketPath: sockA, Manifest: []byte(maniA)}); err != nil {
		t.Fatalf("register a-decoder: %v", err)
	}
	if _, err := s.Register(context.Background(), &pb.RegisterRequest{SocketPath: sockB, Manifest: []byte(maniB)}); err != nil {
		t.Fatalf("register b-decoder: %v", err)
	}

	ca, _, okA := s.FindByName("a-decoder")
	if !okA || ca == nil {
		t.Fatalf("FindByName a-decoder failed: ok=%v client=%v", okA, ca)
	}
	cb, _, okB := s.FindByName("b-decoder")
	if !okB || cb == nil {
		t.Fatalf("FindByName b-decoder failed: ok=%v client=%v", okB, cb)
	}
	// 同名解析必须稳定返回同一 client，且与另一插件不同
	if ca2, _, ok2 := s.FindByName("a-decoder"); !ok2 || ca2 != ca {
		t.Errorf("FindByName a-decoder should return stable client: ok=%v client=%v", ok2, ca2)
	}
	if ca == cb {
		t.Error("a-decoder and b-decoder must resolve to distinct clients")
	}

	// 未知名应返回 not-found
	if _, _, ok := s.FindByName("missing"); ok {
		t.Error("FindByName missing should return not-found")
	}

	// 协议 hint 退化路径：Find("tcp") 仍能命中（两个插件之一）
	if c, _, ok := s.Find("tcp"); !ok || c == nil {
		t.Errorf("Find tcp fallback failed: ok=%v client=%v", ok, c)
	}

	// 注销 b-decoder 后：名查失效，a-decoder 不受影响
	var bInstance string
	for _, p := range s.List() {
		if p.Name == "b-decoder" {
			bInstance = p.InstanceID
		}
	}
	if bInstance == "" {
		t.Fatal("b-decoder instance id not found in List")
	}
	if _, err := s.Deregister(context.Background(), &pb.DeregisterRequest{InstanceId: bInstance}); err != nil {
		t.Fatalf("deregister b-decoder: %v", err)
	}
	if _, _, ok := s.FindByName("b-decoder"); ok {
		t.Error("FindByName b-decoder should fail after deregister")
	}
	if _, _, ok := s.FindByName("a-decoder"); !ok {
		t.Error("a-decoder should still resolve after b-decoder deregistered")
	}
}
