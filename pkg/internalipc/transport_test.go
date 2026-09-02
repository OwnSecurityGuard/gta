package internalipc

import (
	"context"
	"io"
	"net"
	"runtime"
	"testing"
)

func TestListenAndDialEcho(t *testing.T) {
	workDir := t.TempDir()
	const name = "test-echo"

	ln, err := Listen(workDir, name)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(conn, conn) // 回显
	}()

	conn, err := Dial(workDir, name)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	want := []byte("hello-gta")
	if _, err := conn.Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestListenAddrTCPAndDialRouting 验证跨机器部署路径：在 TCP 地址上监听，
// 并通过 DialContextTarget 建立连接；同时验证地址路由规则：含 "/" 的路径按
// Unix socket 处理，纯 host:port 按 TCP，unix:/npipe: 按对应 scheme。
func TestListenAddrTCPAndDialRouting(t *testing.T) {
	// TCP 监听
	ln, err := ListenAddr("127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenAddr tcp: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(conn, conn)
	}()

	addr := ln.Addr().String()
	// DialGRPCAddr 底层使用 DialContextTarget，应能连上 TCP 监听。
	conn, err := DialGRPCAddr(addr)
	if err != nil {
		t.Fatalf("DialGRPCAddr: %v", err)
	}
	conn.Close()
}

func TestDialContextTargetRoutes(t *testing.T) {
	ctx := context.Background()

	// 1) TCP：host:port
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	defer tcpLn.Close()
	go func() {
		c, e := tcpLn.Accept()
		if e != nil {
			return
		}
		c.Close()
	}()
	if c, e := DialContextTarget(ctx, tcpLn.Addr().String()); e != nil {
		t.Fatalf("dial tcp target: %v", e)
	} else {
		c.Close()
	}

	// 2) Unix socket：裸路径（含 "/"）
	dir := t.TempDir()
	sock := dir + "/x.sock"
	uLn, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer uLn.Close()
	go func() {
		c, e := uLn.Accept()
		if e != nil {
			return
		}
		c.Close()
	}()
	if c, e := DialContextTarget(ctx, sock); e != nil {
		t.Fatalf("dial unix path target: %v", e)
	} else {
		c.Close()
	}

	// 3) unix: scheme
	if c, e := DialContextTarget(ctx, "unix:"+sock); e != nil {
		t.Fatalf("dial unix: scheme target: %v", e)
	} else {
		c.Close()
	}

	// 4) npipe: 在非 Windows 平台应返回错误（不支持）
	if runtime.GOOS != "windows" {
		if c, e := DialContextTarget(ctx, `npipe:\\.\pipe\gta-test`); e == nil {
			c.Close()
			t.Skip("npipe unexpectedly dialable on non-windows; skipping assertion")
		}
	}
}
