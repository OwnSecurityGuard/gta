package main

import (
	"context"
	"fmt"
	"log"

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

	sessions, err := client.ListCaptureSessions(context.Background(), &pb.ListCaptureSessionsRequest{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("active sessions: %d\n", len(sessions.Sessions))
	for _, s := range sessions.Sessions {
		fmt.Printf("  session=%s state=%s plugin=%s port=%d\n", s.SessionId, s.State, s.Plugin, s.Port)
	}

	for _, s := range sessions.Sessions {
		status, err := client.GetCaptureStatus(context.Background(), &pb.GetCaptureStatusRequest{SessionId: s.SessionId})
		if err != nil {
			fmt.Printf("status for %s: error=%v\n", s.SessionId, err)
			continue
		}
		fmt.Printf("status for %s: state=%s raw=%d events=%d metrics=%d decode_errors=%d err=%q\n",
			s.SessionId, status.State, status.RawCount, status.EventCount, status.MetricCount, status.DecodeErrors, status.Err)
	}
}
