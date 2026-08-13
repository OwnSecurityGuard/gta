package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	_ "modernc.org/sqlite"

	pb "gta/pkg/internalipc/proto"
)

// verify_plugin is a dev harness that captures live traffic for a given plugin
// and inspects the decoded events written to the session SQLite database.
//
// It is generic: pass the plugin name and traffic port via flags. Generate the
// traffic against -port with your own client/server (for an HTTP decoder,
// examples/http/server is a ready-made generator — but that is unrelated to any
// specific plugin binary).
func main() {
	var (
		plugin = flag.String("plugin", "", "plugin name to verify (must be registered/active)")
		port   = flag.Int("port", 8984, "capture port for live traffic")
		device = flag.String("device", "\\Device\\NPF_Loopback", "pcap capture device")
		prefix = flag.String("prefix", "verify", "session id prefix")
	)
	flag.Parse()
	if *plugin == "" {
		log.Fatal("-plugin is required")
	}

	conn, err := grpc.NewClient("localhost:8088", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	client := pb.NewCaptureControlClient(conn)

	sessionID := fmt.Sprintf("%s-%d", *prefix, time.Now().Unix())
	startResp, err := client.StartCapture(context.Background(), &pb.StartCaptureRequest{
		SessionId: sessionID,
		Plugin:    *plugin,
		Port:      int32(*port),
		Source: &pb.StartCaptureRequest_Live{
			Live: &pb.PcapLiveConfig{
				Device: *device,
				Bpf:    fmt.Sprintf("tcp port %d", *port),
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
