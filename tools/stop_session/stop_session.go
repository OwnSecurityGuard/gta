package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "gametrace/pkg/internalipc/proto"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: stop_session <session-id>")
		os.Exit(1)
	}
	sessionID := os.Args[1]

	conn, err := grpc.NewClient("localhost:8088", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	client := pb.NewCaptureControlClient(conn)
	resp, err := client.StopCapture(context.Background(), &pb.StopCaptureRequest{SessionId: sessionID})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("stopped: raw=%d events=%d metrics=%d decode_errors=%d\n",
		resp.RawPackets, resp.Events, resp.Metrics, resp.DecodeErrors)
}
