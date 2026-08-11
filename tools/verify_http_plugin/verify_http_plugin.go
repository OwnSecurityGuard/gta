package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	_ "modernc.org/sqlite"

	pb "gta/pkg/internalipc/proto"
)

func main() {
	conn, err := grpc.NewClient("localhost:8088", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	client := pb.NewCaptureControlClient(conn)

	// Start capture on port 8984 (examples/http/server).
	sessionID := fmt.Sprintf("http-verify-%d", time.Now().Unix())
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
	dbPath := startResp.DbPath
	fmt.Printf("capture started: session=%s db_path=%s\n", startResp.SessionId, dbPath)

	time.Sleep(2 * time.Second)

	statusResp, err := client.GetCaptureStatus(context.Background(), &pb.GetCaptureStatusRequest{SessionId: sessionID})
	if err != nil {
		log.Fatalf("get status: %v", err)
	}
	fmt.Printf("status: state=%s err=%q raw=%d events=%d\n",
		statusResp.State, statusResp.Err, statusResp.RawCount, statusResp.EventCount)

	stopResp, err := client.StopCapture(context.Background(), &pb.StopCaptureRequest{SessionId: sessionID})
	if err != nil {
		log.Fatalf("stop capture: %v", err)
	}
	fmt.Printf("capture stopped: raw=%d events=%d metrics=%d decode_errors=%d\n",
		stopResp.RawPackets, stopResp.Events, stopResp.Metrics, stopResp.DecodeErrors)

	if dbPath == "" {
		log.Fatal("db_path empty")
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	var eventCount int
	row := db.QueryRow("SELECT COUNT(*) FROM events")
	if err := row.Scan(&eventCount); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("events in db: %d\n", eventCount)

	rows, err := db.Query("SELECT type, source, substr(payload, 1, 200) FROM events LIMIT 3")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var typ, source, payload string
		if err := rows.Scan(&typ, &source, &payload); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("event: type=%s source=%s payload=%s\n", typ, source, payload)
	}
}
