// 反复 start/stop 探针：重点在「stop 后立刻再 start」「多次反复」「通过 get_proxy_lease 看的中间状态」。
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "gametrace/pkg/internalipc/proto"
)

func must(err error, ctx string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL [%s]: %v\n", ctx, err)
		os.Exit(1)
	}
}

func main() {
	addr := "127.0.0.1:9888"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	must(err, "dial")
	defer conn.Close()
	cli := pb.NewCaptureControlClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 1. 建租约（NoAutoStart=true，避免第一轮就被我们自己 start 干扰）
	r1, err := cli.CreateProxyLease(ctx, &pb.CreateProxyLeaseRequest{
		Owner: "alice", AllOwners: true, Device: "smoke-dev",
		NoAutoStart: true,
	})
	must(err, "create")
	leaseID := r1.GetLease().GetLeaseId()
	fmt.Printf("[0] lease=%s agent_listen=%d control=%d sticky=%v\n",
		leaseID, r1.GetLease().GetAgentListenPort(), r1.GetLease().GetControlPort(), r1.GetLease().GetStickyPort())
	defer func() {
		_, _ = cli.ReleaseProxyLease(ctx, &pb.ReleaseProxyLeaseRequest{
			Owner: "alice", AllOwners: true, LeaseId: leaseID,
		})
	}()

	// 2. 3 轮 start/stop，每次 stop 后立刻 get 看 idle 状态
	for i := 1; i <= 3; i++ {
		fmt.Printf("\n--- round %d ---\n", i)

		// 2a. stop 时应当 not capturing（除第一轮，第一次前无 capture 无所谓）
		if i > 1 {
			rst, err := cli.StopLeaseCapture(ctx, &pb.StopLeaseCaptureRequest{
				Owner: "alice", AllOwners: true, LeaseId: leaseID,
			})
			must(err, fmt.Sprintf("round %d stop", i))
			fmt.Printf("  stop ok: session=%s raw=%d events=%d\n",
				rst.GetSessionId(), rst.GetRawPackets(), rst.GetEvents())
		}

		// 2b. 看 idle 状态：session_id 必须 == ""、capture_running=false、session_running=false
		got, err := cli.GetProxyLease(ctx, &pb.GetProxyLeaseRequest{
			Owner: "alice", AllOwners: true, LeaseId: leaseID,
		})
		must(err, fmt.Sprintf("round %d get(before start)", i))
		lv := got.GetLease()
		fmt.Printf("  idle state: session_id=%q capture_running=%v session_running=%v capture_count=%d\n",
			lv.GetSessionId(), lv.GetCaptureRunning(), lv.GetSessionRunning(), lv.GetCaptureCount())
		if lv.GetSessionId() != "" {
			fmt.Fprintf(os.Stderr, "FAIL: idle but session_id=%q (lease not cleared)\n", lv.GetSessionId())
			os.Exit(1)
		}
		if lv.GetCaptureRunning() || lv.GetSessionRunning() {
			fmt.Fprintf(os.Stderr, "FAIL: idle but still running (capture=%v session=%v)\n",
				lv.GetCaptureRunning(), lv.GetSessionRunning())
			os.Exit(1)
		}

		// 2c. start
		rs, err := cli.StartLeaseCapture(ctx, &pb.StartLeaseCaptureRequest{
			Owner: "alice", AllOwners: true, LeaseId: leaseID,
		})
		must(err, fmt.Sprintf("round %d start", i))
		fmt.Printf("  start ok: session=%s mobile_grpc=%d capture_running=%v\n",
			rs.GetSessionId(), rs.GetLease().GetMobileGrpcPort(), rs.GetLease().GetCaptureRunning())
	}
	fmt.Println("\nALL ROUNDS OK")
}
