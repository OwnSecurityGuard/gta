package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "gta/pkg/internalipc/proto"
)

func main() {
	conn, err := grpc.NewClient("localhost:8088", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	client := pb.NewCaptureControlClient(conn)

	sessionID := fmt.Sprintf("http-hotreload-%d", time.Now().Unix())

	// Phase 1: start capture while plugin is offline.
	startResp, err := client.StartCapture(context.Background(), &pb.StartCaptureRequest{
		SessionId: sessionID,
		Plugin:    "http",
		Port:      8984,
		Source: &pb.StartCaptureRequest_Live{
			Live: &pb.PcapLiveConfig{
				Device: "\\Device\\NPF_Loopback",
				Bpf:    "tcp port 8984",
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

	fmt.Println("[phase 2] restarting http plugin...")
	cmd := exec.Command(".\\http-plugin.exe")
	cmd.Dir = "E:\\gta\\plugins\\http"
	cmd.Env = append(os.Environ(), "GTA_REGISTRY_ADDR=:9091")
	if err := cmd.Start(); err != nil {
		log.Fatalf("start plugin: %v", err)
	}
	defer cmd.Process.Kill()

	// Wait for plugin to register and attach.
	time.Sleep(8 * time.Second)

	status2, err := client.GetCaptureStatus(context.Background(), &pb.GetCaptureStatusRequest{SessionId: sessionID})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("[phase 2] plugin online status: raw=%d events=%d decode_errors=%d\n",
		status2.RawCount, status2.EventCount, status2.DecodeErrors)

	stopResp, err := client.StopCapture(context.Background(), &pb.StopCaptureRequest{SessionId: sessionID})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("[phase 3] stopped: raw=%d events=%d decode_errors=%d\n",
		stopResp.RawPackets, stopResp.Events, stopResp.DecodeErrors)
}
