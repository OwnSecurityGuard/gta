package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "gametrace/pkg/internalipc/proto"
)

func main() {
	conn, err := grpc.NewClient("localhost:8088", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	client := pb.NewCaptureControlClient(conn)

	sessionID := fmt.Sprintf("test-%d", time.Now().Unix())
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
	fmt.Printf("started: session=%s db=%s\n", startResp.SessionId, startResp.DbPath)

	time.Sleep(10 * time.Second)

	status, err := client.GetCaptureStatus(context.Background(), &pb.GetCaptureStatusRequest{SessionId: sessionID})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("status: state=%s raw=%d events=%d decode_errors=%d err=%q\n",
		status.State, status.RawCount, status.EventCount, status.DecodeErrors, status.Err)
}
