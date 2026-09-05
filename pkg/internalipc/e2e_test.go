package internalipc_test

import (
	"context"
	"net"
	"testing"

	"gametrace/pkg/internalipc"
	"gametrace/pkg/internalipc/capturecontrol"
	pb "gametrace/pkg/internalipc/proto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestCaptureControlE2E 验证 client 通过 transport 连接到 server 并调用 CaptureControl RPC。
func TestCaptureControlE2E(t *testing.T) {
	ctx := context.Background()
	workDir := t.TempDir()
	const sockName = "pipeline"

	// 1. server 侧：监听 transport
	listener, err := internalipc.Listen(workDir, sockName)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()

	// 2. server 侧：注册 CaptureControl server
	engine := &fakeCaptureEngine{}
	grpcSrv := grpc.NewServer()
	pb.RegisterCaptureControlServer(grpcSrv, capturecontrol.NewServer(engine))
	go grpcSrv.Serve(listener)
	defer grpcSrv.Stop()

	// 3. client 侧：用 grpc.NewClient + WithContextDialer 连接 transport
	// grpc.NewClient 默认用 dns resolver 解析无 scheme 的 target，会对 socket
	// 路径做 DNS 解析并返回零地址。这里显式用 passthrough scheme 让 resolver
	// 直接产出一个地址，真正的连接由 WithContextDialer 通过 internalipc.Dial
	// 完成（dialer 忽略传入的 addr）。
	conn, err := grpc.NewClient(
		"passthrough:///"+sockName,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return internalipc.Dial(workDir, sockName)
		}),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()
	client := pb.NewCaptureControlClient(conn)

	// 4. 调 ListInterfaces
	resp, err := client.ListInterfaces(ctx, &pb.ListInterfacesRequest{})
	if err != nil {
		t.Fatalf("ListInterfaces: %v", err)
	}
	if len(resp.GetNames()) != 2 {
		t.Errorf("names = %v, want 2", resp.GetNames())
	}

	// 5. 调 StartCapture（file source）
	startResp, err := client.StartCapture(ctx, &pb.StartCaptureRequest{
		SessionId: "e2e-test",
		Plugin:    "tcp",
		Port:      0,
		Source: &pb.StartCaptureRequest_File{
			File: &pb.PcapFileConfig{Path: "test.pcap"},
		},
	})
	if err != nil {
		t.Fatalf("StartCapture: %v", err)
	}
	if startResp.GetDbPath() == "" {
		t.Error("db_path should not be empty")
	}

	// 6. 调 GetCaptureStatus
	statusResp, err := client.GetCaptureStatus(ctx, &pb.GetCaptureStatusRequest{SessionId: "e2e-test"})
	if err != nil {
		t.Fatalf("GetCaptureStatus: %v", err)
	}
	if statusResp.GetState() != "Running" {
		t.Errorf("state = %q, want Running", statusResp.GetState())
	}

	// 7. 调 StopCapture
	stopResp, err := client.StopCapture(ctx, &pb.StopCaptureRequest{SessionId: "e2e-test"})
	if err != nil {
		t.Fatalf("StopCapture: %v", err)
	}
	if stopResp.GetRawPackets() != 10 {
		t.Errorf("raw_packets = %d, want 10", stopResp.GetRawPackets())
	}
}

// fakeCaptureEngine 实现 capturecontrol.CaptureEngine 接口。
type fakeCaptureEngine struct{}

func (f *fakeCaptureEngine) StartSession(ctx context.Context, req capturecontrol.StartSessionRequest) (capturecontrol.StartSessionResult, error) {
	return capturecontrol.StartSessionResult{SessionID: req.SessionID, State: "Running", DBPath: "/tmp/test.db"}, nil
}
func (f *fakeCaptureEngine) StopSession(ctx context.Context, sessionID string) (capturecontrol.StopSessionResult, error) {
	return capturecontrol.StopSessionResult{State: "Closed", RawPackets: 10, Events: 8}, nil
}
func (f *fakeCaptureEngine) GetStatus(ctx context.Context, sessionID string) (capturecontrol.StatusResult, error) {
	return capturecontrol.StatusResult{State: "Running", RawCount: 5, EventCount: 3}, nil
}
func (f *fakeCaptureEngine) ListSessions(ctx context.Context) ([]capturecontrol.SessionSummary, error) {
	return nil, nil
}
func (f *fakeCaptureEngine) ListInterfaces(ctx context.Context) ([]string, error) {
	return []string{"eth0", "lo"}, nil
}
func (f *fakeCaptureEngine) DecodeRawPackets(ctx context.Context, req capturecontrol.DecodeRawPacketsRequest) (capturecontrol.DecodeRawPacketsResult, error) {
	return capturecontrol.DecodeRawPacketsResult{}, nil
}
func (f *fakeCaptureEngine) DeregisterPlugin(ctx context.Context, instanceID, name string) (string, error) {
	return instanceID, nil
}
func (f *fakeCaptureEngine) ListPlugins(ctx context.Context) ([]capturecontrol.PluginSummary, error) {
	return nil, nil
}
func (f *fakeCaptureEngine) GetPluginManifest(ctx context.Context, name string) ([]byte, error) {
	return nil, nil
}
func (f *fakeCaptureEngine) GetRegistryAddr(ctx context.Context) (string, error) {
	return ":9091", nil
}
func (f *fakeCaptureEngine) SetSessionPlugin(ctx context.Context, sessionID, plugin string, pluginOwners []string) (string, error) {
	return plugin, nil
}
func (f *fakeCaptureEngine) SubscribePlugins(ctx context.Context) (<-chan capturecontrol.PluginEvent, error) {
	ch := make(chan capturecontrol.PluginEvent)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}
func (f *fakeCaptureEngine) TestPlugin(ctx context.Context, req capturecontrol.TestPluginRequest) (capturecontrol.TestPluginResult, error) {
	return capturecontrol.TestPluginResult{}, nil
}
func (f *fakeCaptureEngine) Verify(ctx context.Context, req capturecontrol.VerifyRequest) (capturecontrol.VerifyResult, error) {
	return capturecontrol.VerifyResult{Verdict: "pass"}, nil
}
func (f *fakeCaptureEngine) SampleBytes(ctx context.Context, req capturecontrol.SampleBytesRequest) (capturecontrol.SampleBytesResult, error) {
	return capturecontrol.SampleBytesResult{}, nil
}
func (f *fakeCaptureEngine) CreateProxyLease(ctx context.Context, req capturecontrol.CreateProxyLeaseRequest) (capturecontrol.ProxyLease, error) {
	return capturecontrol.ProxyLease{LeaseID: "lease-1", SessionID: "lease-1"}, nil
}
func (f *fakeCaptureEngine) ListProxyLeases(ctx context.Context) ([]capturecontrol.ProxyLease, error) {
	return nil, nil
}
func (f *fakeCaptureEngine) GetProxyLease(ctx context.Context, leaseID string) (capturecontrol.ProxyLease, error) {
	return capturecontrol.ProxyLease{}, nil
}
func (f *fakeCaptureEngine) ReleaseProxyLease(ctx context.Context, leaseID string) (capturecontrol.ReleaseProxyLeaseResult, error) {
	return capturecontrol.ReleaseProxyLeaseResult{OK: true}, nil
}
func (f *fakeCaptureEngine) StartLeaseCapture(ctx context.Context, req capturecontrol.StartLeaseCaptureRequest) (capturecontrol.StartLeaseCaptureResult, error) {
	return capturecontrol.StartLeaseCaptureResult{OK: true, SessionID: "cap-1"}, nil
}
func (f *fakeCaptureEngine) StopLeaseCapture(ctx context.Context, leaseID string) (capturecontrol.StopLeaseCaptureResult, error) {
	return capturecontrol.StopLeaseCaptureResult{OK: true, SessionID: "cap-1"}, nil
}
