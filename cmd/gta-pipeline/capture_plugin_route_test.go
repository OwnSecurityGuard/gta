package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"gta/pkg/plugin"

	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
	"google.golang.org/grpc"
)

// fakeDecoderServerMain 是最小化的 Decoder gRPC 服务桩，仅用于让 Register 完成可达性拨号。
type fakeDecoderServerMain struct {
	pb.UnimplementedDecoderServer
}

func (fakeDecoderServerMain) DecodeV2(stream grpc.BidiStreamingServer[pb.DecodeRequest, pb.DecodeResponseV2]) error {
	return nil
}

// startFakeDecoderMain 启动一个监听 unix socket 的 Decoder 服务，返回 socket 路径与停止函数。
func startFakeDecoderMain(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "decoder.sock")
	_ = os.Remove(sock)
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	srv := grpc.NewServer()
	pb.RegisterDecoderServer(srv, fakeDecoderServerMain{})
	go func() { _ = srv.Serve(lis) }()
	stop := func() {
		srv.Stop()
		_ = os.Remove(sock)
	}
	return sock, stop
}

// registerFakePlugin 向注册表注册一个具名解码插件（protocol 固定 tcp，便于验证按名区分）。
func registerFakePlugin(t *testing.T, s *plugin.RegistryServer, name, sock string) {
	t.Helper()
	// api_version 必须是 v2：manager 的 CheckManifestVersion 要求 major 与
	// ProtocolVersion 一致，写 v1 会在 Register 阶段直接被拒。
	manifest := "api_version: gta.decoder/v2\nname: " + name + "\nprotocol: tcp\ntype: decoder\n"
	if _, err := s.Register(context.Background(), &pb.RegisterRequest{
		SocketPath: sock,
		Manifest:   []byte(manifest),
	}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
}

// TestCaptureTask_resolveDecoderClient 验证 GAP 2 修复：
// 会话按 t.plugin 名字精确路由到对应插件，多项目并行互不干扰；
// 未指定插件名时退化按 tcp 协议 hint；未知名返回 not-found。
func TestCaptureTask_resolveDecoderClient(t *testing.T) {
	sockA, stopA := startFakeDecoderMain(t)
	defer stopA()
	sockB, stopB := startFakeDecoderMain(t)
	defer stopB()

	s := plugin.NewRegistryServer(10)
	registerFakePlugin(t, s, "plugin-a", sockA)
	registerFakePlugin(t, s, "plugin-b", sockB)

	// 预取两个插件各自的 client 指针，用于断言"按名精确区分"
	ca, _, okA := s.FindByName("plugin-a")
	cb, _, okB := s.FindByName("plugin-b")
	if !okA || ca == nil || !okB || cb == nil {
		t.Fatal("precondition: plugin-a and plugin-b must be registered and resolvable")
	}

	// 1. 指定 plugin-a → 必须返回 A 的 client，且不能是 B 的 client
	taskA := &captureTask{registry: s, plugin: "plugin-a"}
	gotA, _, ok := taskA.resolveDecoderClient()
	if !ok || gotA != ca {
		t.Errorf("plugin-a routed incorrectly: got=%v want=%v ok=%v", gotA, ca, ok)
	}
	if gotA == cb {
		t.Errorf("plugin-a must NOT route to plugin-b's client")
	}

	// 2. 指定 plugin-b → 必须返回 B 的 client
	taskB := &captureTask{registry: s, plugin: "plugin-b"}
	gotB, _, ok := taskB.resolveDecoderClient()
	if !ok || gotB != cb {
		t.Errorf("plugin-b routed incorrectly: got=%v want=%v ok=%v", gotB, cb, ok)
	}

	// 3. 未指定插件名 → 退化按 tcp 协议 hint，返回一个非 nil 的在线解码器
	taskEmpty := &captureTask{registry: s, plugin: ""}
	if c, _, ok := taskEmpty.resolveDecoderClient(); !ok || c == nil {
		t.Errorf("empty plugin should fall back to tcp decoder, got=%v ok=%v", c, ok)
	}

	// 4. 未知名 → not-found
	taskZ := &captureTask{registry: s, plugin: "plugin-z"}
	if _, _, ok := taskZ.resolveDecoderClient(); ok {
		t.Errorf("unknown plugin name should not resolve")
	}
}
