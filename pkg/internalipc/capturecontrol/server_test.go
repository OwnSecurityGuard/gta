package capturecontrol

import (
	"context"
	"testing"

	pb "gta/pkg/internalipc/proto"
)

// fakeEngine 是测试用 CaptureEngine 桩实现。
type fakeEngine struct {
	startResult   StartSessionResult
	startErr      error
	startLastReq  StartSessionRequest
	stopResult    StopSessionResult
	stopErr       error
	statusResult  StatusResult
	statusErr     error
	listSessions  []SessionSummary
	listSessErr   error
	ifaceNames    []string
	ifaceErr      error
	decodeResult  DecodeRawPacketsResult
	decodeErr     error
	decodeLastReq DecodeRawPacketsRequest

	verifyResult  VerifyResult
	verifyErr     error
	verifyLastReq VerifyRequest
	sampleResult  SampleBytesResult
	sampleErr     error
	sampleLastReq SampleBytesRequest
}

func (f *fakeEngine) StartSession(ctx context.Context, req StartSessionRequest) (StartSessionResult, error) {
	f.startLastReq = req
	return f.startResult, f.startErr
}
func (f *fakeEngine) StopSession(ctx context.Context, sessionID string) (StopSessionResult, error) {
	return f.stopResult, f.stopErr
}
func (f *fakeEngine) GetStatus(ctx context.Context, sessionID string) (StatusResult, error) {
	return f.statusResult, f.statusErr
}
func (f *fakeEngine) ListSessions(ctx context.Context) ([]SessionSummary, error) {
	return f.listSessions, f.listSessErr
}
func (f *fakeEngine) ListInterfaces(ctx context.Context) ([]string, error) {
	return f.ifaceNames, f.ifaceErr
}
func (f *fakeEngine) DecodeRawPackets(ctx context.Context, req DecodeRawPacketsRequest) (DecodeRawPacketsResult, error) {
	f.decodeLastReq = req
	return f.decodeResult, f.decodeErr
}
func (f *fakeEngine) TestPlugin(ctx context.Context, req TestPluginRequest) (TestPluginResult, error) {
	return TestPluginResult{}, nil
}
func (f *fakeEngine) Verify(ctx context.Context, req VerifyRequest) (VerifyResult, error) {
	f.verifyLastReq = req
	return f.verifyResult, f.verifyErr
}
func (f *fakeEngine) SampleBytes(ctx context.Context, req SampleBytesRequest) (SampleBytesResult, error) {
	f.sampleLastReq = req
	return f.sampleResult, f.sampleErr
}
func (f *fakeEngine) SetSessionPlugin(ctx context.Context, sessionID, plugin string) (string, error) {
	return plugin, nil
}
func (f *fakeEngine) SubscribePlugins(ctx context.Context) (<-chan PluginEvent, error) {
	return nil, nil
}
func (f *fakeEngine) DeregisterPlugin(ctx context.Context, instanceID, name string) (string, error) {
	return instanceID, nil
}
func (f *fakeEngine) ListPlugins(ctx context.Context) ([]PluginSummary, error) {
	return nil, nil
}
func (f *fakeEngine) GetPluginManifest(ctx context.Context, name string) ([]byte, error) {
	return nil, nil
}
func (f *fakeEngine) GetRegistryAddr(ctx context.Context) (string, error) {
	return ":9091", nil
}

func (f *fakeEngine) GetProxyConfig(ctx context.Context) (ProxyConfigState, error) {
	return ProxyConfigState{}, nil
}

func (f *fakeEngine) UpdateProxyConfig(ctx context.Context, req ProxyConfigUpdate) (ProxyConfigState, error) {
	return ProxyConfigState{}, nil
}

func TestServer_StartCapture(t *testing.T) {
	engine := &fakeEngine{
		startResult: StartSessionResult{SessionID: "s1", State: "Running", DBPath: "/tmp/s1.db"},
	}
	srv := NewServer(engine)
	resp, err := srv.StartCapture(context.Background(), &pb.StartCaptureRequest{
		SessionId: "s1",
		Plugin:    "tcp",
		Port:      8080,
		Source: &pb.StartCaptureRequest_File{
			File: &pb.PcapFileConfig{Path: "test.pcap"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetSessionId() != "s1" || resp.GetState() != "Running" || resp.GetDbPath() != "/tmp/s1.db" {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestServer_StopCapture(t *testing.T) {
	engine := &fakeEngine{
		stopResult: StopSessionResult{State: "Closed", RawPackets: 10, Events: 8},
	}
	srv := NewServer(engine)
	resp, err := srv.StopCapture(context.Background(), &pb.StopCaptureRequest{SessionId: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetState() != "Closed" || resp.GetRawPackets() != 10 || resp.GetEvents() != 8 {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestServer_GetCaptureStatus(t *testing.T) {
	engine := &fakeEngine{
		statusResult: StatusResult{State: "Running", RawCount: 5, EventCount: 3},
	}
	srv := NewServer(engine)
	resp, err := srv.GetCaptureStatus(context.Background(), &pb.GetCaptureStatusRequest{SessionId: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetState() != "Running" || resp.GetRawCount() != 5 || resp.GetEventCount() != 3 {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestServer_ListInterfaces(t *testing.T) {
	engine := &fakeEngine{ifaceNames: []string{"eth0", "lo"}}
	srv := NewServer(engine)
	resp, err := srv.ListInterfaces(context.Background(), &pb.ListInterfacesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetNames()) != 2 || resp.GetNames()[0] != "eth0" {
		t.Errorf("unexpected names: %v", resp.GetNames())
	}
}

func TestServer_ListCaptureSessions(t *testing.T) {
	engine := &fakeEngine{
		listSessions: []SessionSummary{
			{SessionID: "s1", State: "running", SourceName: "pcap-live", Port: 8080, Plugin: "tcp"},
			{SessionID: "s2", State: "running", SourceName: "pcap-file", Port: 9090, Plugin: "http"},
		},
	}
	srv := NewServer(engine)
	resp, err := srv.ListCaptureSessions(context.Background(), &pb.ListCaptureSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetSessions()) != 2 {
		t.Fatalf("got %d sessions, want 2", len(resp.GetSessions()))
	}
	first := resp.GetSessions()[0]
	if first.GetSessionId() != "s1" || first.GetState() != "running" || first.GetPort() != 8080 {
		t.Errorf("unexpected first session: %+v", first)
	}
}

func TestServer_DecodeRawPackets(t *testing.T) {
	engine := &fakeEngine{
		decodeResult: DecodeRawPacketsResult{TotalRaw: 100, Decoded: 80, DecodeErrors: 20},
	}
	srv := NewServer(engine)
	resp, err := srv.DecodeRawPackets(context.Background(), &pb.DecodeRawPacketsRequest{
		SessionId:     "s1",
		Plugin:        "http",
		Protocol:      "tcp",
		Src:           "1.2.3.4",
		Dst:           "5.6.7.8",
		Limit:         50,
		ClearExisting: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetTotalRaw() != 100 || resp.GetDecoded() != 80 || resp.GetDecodeErrors() != 20 {
		t.Errorf("unexpected response: %+v", resp)
	}
	// 验证请求参数被正确转换到 CaptureEngine 调用
	if engine.decodeLastReq.SessionID != "s1" || engine.decodeLastReq.Plugin != "http" {
		t.Errorf("unexpected session/plugin: %+v", engine.decodeLastReq)
	}
	if engine.decodeLastReq.Protocol != "tcp" || engine.decodeLastReq.Src != "1.2.3.4" || engine.decodeLastReq.Dst != "5.6.7.8" {
		t.Errorf("unexpected filters: %+v", engine.decodeLastReq)
	}
	if engine.decodeLastReq.Limit != 50 || !engine.decodeLastReq.ClearExisting {
		t.Errorf("unexpected limit/clear_existing: %+v", engine.decodeLastReq)
	}
}

// TestServer_StartCaptureMobile 验证 mobile source 配置正确映射到 CaptureEngine。
func TestServer_StartCaptureMobile(t *testing.T) {
	engine := &fakeEngine{
		startResult: StartSessionResult{SessionID: "m1", State: "Running", DBPath: "/tmp/m1.db"},
	}
	srv := NewServer(engine)
	resp, err := srv.StartCapture(context.Background(), &pb.StartCaptureRequest{
		SessionId: "m1",
		Plugin:    "game",
		Source: &pb.StartCaptureRequest_Mobile{
			Mobile: &pb.MobileSourceConfig{
				ListenAddr:   "127.0.0.1:9090",
				FrameStyle:   "length_prefix",
				PrefixLen:    4,
				LittleEndian: true,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetSessionId() != "m1" {
		t.Errorf("unexpected response: %+v", resp)
	}
	m := engine.startLastReq.Mobile
	if m == nil {
		t.Fatalf("expected mobile config, got nil")
	}
	if m.ListenAddr != "127.0.0.1:9090" || m.FrameStyle != "length_prefix" ||
		m.PrefixLen != 4 || !m.LittleEndian {
		t.Errorf("unexpected mobile config: %+v", m)
	}
}
