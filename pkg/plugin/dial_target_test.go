package plugin

import (
	"context"
	"net"
	"testing"
)

// TestDialTargetRouting 验证 Register 回调插件 Decode 地址时的路由：
// 纯 host:port 走 TCP（跨机器），裸路径 / unix: 走 Unix socket。
func TestDialTargetRouting(t *testing.T) {
	ctx := context.Background()

	// TCP host:port
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	defer tcpLn.Close()
	go func() {
		c, e := tcpLn.Accept()
		if e == nil {
			c.Close()
		}
	}()
	if c, e := dialTarget(ctx, tcpLn.Addr().String()); e != nil {
		t.Fatalf("dial tcp target: %v", e)
	} else {
		c.Close()
	}

	// Unix socket：裸路径
	dir := t.TempDir()
	sock := dir + "/d.sock"
	uLn, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer uLn.Close()
	go func() {
		c, e := uLn.Accept()
		if e == nil {
			c.Close()
		}
	}()
	if c, e := dialTarget(ctx, sock); e != nil {
		t.Fatalf("dial unix path target: %v", e)
	} else {
		c.Close()
	}

	// unix: scheme
	if c, e := dialTarget(ctx, "unix:"+sock); e != nil {
		t.Fatalf("dial unix: scheme target: %v", e)
	} else {
		c.Close()
	}
}
