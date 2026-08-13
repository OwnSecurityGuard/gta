package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "gta/pkg/internalipc/proto"
)

// hotreload is a dev harness that proves a plugin can be attached mid-capture:
// it starts a live capture while the plugin is OFFLINE, then launches the plugin
// binary and checks that events start flowing without stopping the capture.
//
// It is generic: pass the plugin name, its directory, and the traffic port via
// flags. It does NOT hardcode any specific plugin or path.
func main() {
	var (
		plugin    = flag.String("plugin", "", "plugin name; the binary <plugin>.exe must exist in -plugindir")
		plugindir = flag.String("plugindir", ".", "directory containing the plugin binary")
		port      = flag.Int("port", 8984, "capture port for live traffic")
		device    = flag.String("device", "\\Device\\NPF_Loopback", "pcap capture device")
		registry  = flag.String("registry", "", "registry address; defaults to $GTA_REGISTRY_ADDR then :9091")
		prefix    = flag.String("prefix", "hotreload", "session id prefix")
	)
	flag.Parse()
	if *plugin == "" {
		log.Fatal("-plugin is required")
	}

	regAddr := *registry
	if regAddr == "" {
		regAddr = os.Getenv("GTA_REGISTRY_ADDR")
	}
	if regAddr == "" {
		regAddr = ":9091"
	}

	conn, err := grpc.NewClient("localhost:8088", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	client := pb.NewCaptureControlClient(conn)
	sessionID := fmt.Sprintf("%s-%d", *prefix, time.Now().Unix())
	bpf := fmt.Sprintf("tcp port %d", *port)

	// Phase 1: start capture while plugin is offline.
	startResp, err := client.StartCapture(context.Background(), &pb.StartCaptureRequest{
		SessionId: sessionID,
		Plugin:    *plugin,
		Port:      int32(*port),
		Source: &pb.StartCaptureRequest_Live{
			Live: &pb.PcapLiveConfig{
				Device: *device,
				Bpf:    bpf,
			},
		},
	})
	if err != nil {
		log.Fatalf("start capture: %v", err)
	}
	fmt.Printf("[phase 1] capture started: %s db=%s\n", startResp.SessionId, startResp.DbPath)

	time.Sleep(5 * time.Second)

	status1, err := client.GetCaptureStatus(context.Background(), &pb.GetCaptureStatusRequest{SessionId: sessionID})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("[phase 1] plugin offline status: raw=%d events=%d decode_errors=%d\n",
		status1.RawCount, status1.EventCount, status1.DecodeErrors)

	fmt.Printf("[phase 2] starting plugin %q from %s ...\n", *plugin, *plugindir)
	binPath := filepath.Join(*plugindir, *plugin+".exe")
	cmd := exec.Command(binPath)
	cmd.Dir = *plugindir
	cmd.Env = append(os.Environ(), "GTA_REGISTRY_ADDR="+regAddr)
	if err := cmd.Start(); err != nil {
		log.Fatalf("start plugin: %v", err)
	}
	defer cmd.Process.Kill()

	// Wait for the plugin to register and attach.
	time.Sleep(8 * time.Second)

	status2, err := client.GetCaptureStatus(context.Background(), &pb.GetCaptureStatusRequest{SessionId: sessionID})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("[phase 2] plugin online status: raw=%d events=%d decode_errors=%d\n",
		status2.RawCount, status2.EventCount, status2.DecodeErrors)

	stopResp, err := client.StopCapture(context.Background(), &pb.StopCaptureRequest{SessionId: sessionID})
	if err != nil {
		log.Fatalf("stop capture: %v", err)
	}
	fmt.Printf("[phase 3] stopped: raw=%d events=%d decode_errors=%d\n",
		stopResp.RawPackets, stopResp.Events, stopResp.DecodeErrors)
}
