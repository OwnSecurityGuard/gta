package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "gta/pkg/internalipc/proto"
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

func main() {
	addr := "127.0.0.1:29888"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	must(err)
	defer conn.Close()
	cli := pb.NewCaptureControlClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. 创建租约（alice / pixel-7）—— 只建出口，不抓包
	r1, err := cli.CreateProxyLease(ctx, &pb.CreateProxyLeaseRequest{
		Owner:       "alice",
		AllOwners:   true,
		Device:      "pixel-7",
		NoAutoStart: true,
	})
	must(err)
	l1 := r1.GetLease()
	fmt.Printf("[1] lease=%s port=%d control=%d sticky=%v\n",
		l1.GetLeaseId(), l1.GetAgentListenPort(), l1.GetControlPort(), l1.GetStickyPort())

	// 2. 释放
	rr1, err := cli.ReleaseProxyLease(ctx, &pb.ReleaseProxyLeaseRequest{
		Owner: "alice", AllOwners: true, LeaseId: l1.GetLeaseId(),
	})
	must(err)
	fmt.Printf("[2] released ok=%v msg=%q\n", rr1.GetOk(), rr1.GetMessage())

	// 3. 同设备再建 → sticky 复用
	r2, err := cli.CreateProxyLease(ctx, &pb.CreateProxyLeaseRequest{
		Owner:       "alice",
		AllOwners:   true,
		Device:      "pixel-7",
		NoAutoStart: true,
	})
	must(err)
	l2 := r2.GetLease()
	fmt.Printf("[3] lease=%s port=%d control=%d sticky=%v\n",
		l2.GetLeaseId(), l2.GetAgentListenPort(), l2.GetControlPort(), l2.GetStickyPort())
	if l2.GetAgentListenPort() != l1.GetAgentListenPort() {
		fmt.Fprintf(os.Stderr, "FAIL: sticky port not reused: %d != %d\n",
			l1.GetAgentListenPort(), l2.GetAgentListenPort())
		os.Exit(1)
	}
	if !l2.GetStickyPort() {
		fmt.Fprintln(os.Stderr, "FAIL: StickyPort=false on reused port")
		os.Exit(1)
	}

	// 4. start capture → 新 session_id / mobile_grpc
	rs, err := cli.StartLeaseCapture(ctx, &pb.StartLeaseCaptureRequest{
		Owner: "alice", AllOwners: true, LeaseId: l2.GetLeaseId(),
	})
	must(err)
	ls := rs.GetLease()
	fmt.Printf("[4] start session=%s mobile_grpc=%d capture_running=%v\n",
		rs.GetSessionId(), ls.GetMobileGrpcPort(), ls.GetCaptureRunning())

	// 5. 立即 stop → 出口端口保留
	rst, err := cli.StopLeaseCapture(ctx, &pb.StopLeaseCaptureRequest{
		Owner: "alice", AllOwners: true, LeaseId: l2.GetLeaseId(),
	})
	must(err)
	fmt.Printf("[5] stop session=%s raw=%d events=%d dur=%.3fs\n",
		rst.GetSessionId(), rst.GetRawPackets(), rst.GetEvents(), rst.GetDurationSec())

	// 6. 再次 start → 新 session_id（与停止的不同）
	rs2, err := cli.StartLeaseCapture(ctx, &pb.StartLeaseCaptureRequest{
		Owner: "alice", AllOwners: true, LeaseId: l2.GetLeaseId(),
	})
	must(err)
	if rs2.GetSessionId() == rs.GetSessionId() {
		fmt.Fprintln(os.Stderr, "FAIL: restarted capture reused stopped session_id")
		os.Exit(1)
	}
	fmt.Printf("[6] restart session=%s (new)\n", rs2.GetSessionId())

	// 7. 二次 stop + release
	_, err = cli.StopLeaseCapture(ctx, &pb.StopLeaseCaptureRequest{
		Owner: "alice", AllOwners: true, LeaseId: l2.GetLeaseId(),
	})
	must(err)
	_, err = cli.ReleaseProxyLease(ctx, &pb.ReleaseProxyLeaseRequest{
		Owner: "alice", AllOwners: true, LeaseId: l2.GetLeaseId(),
	})
	must(err)
	fmt.Println("[7] lease fully released")

	// 8. list 应为空
	lst, err := cli.ListProxyLeases(ctx, &pb.ListProxyLeasesRequest{
		Owner: "alice", AllOwners: true,
	})
	must(err)
	if len(lst.GetLeases()) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: expected 0 leases after cleanup, got %d\n",
			len(lst.GetLeases()))
		os.Exit(1)
	}
	fmt.Println("[8] list empty (clean)")

	fmt.Println("SMOKE OK")
}
