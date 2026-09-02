// Package internalipc 提供进程间 gRPC 通信的跨平台 transport。
//
// 地址模型：调用方只传 workDir 与 socket name（如 "capture"），
// 由平台实现拼成 Unix Socket 路径或 Windows 命名管道名。
package internalipc

import (
	"context"
	"fmt"
	"net"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// DialGRPC 创建连接到指定 socket 的 gRPC ClientConn。
// 使用 passthrough scheme + WithContextDialer 避免 DNS 解析。
func DialGRPC(workDir, name string) (*grpc.ClientConn, error) {
	return grpc.NewClient(
		"passthrough:///"+name,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return Dial(workDir, name)
		}),
	)
}

// DialContextTarget 解析一个通用目标地址并建立 net.Conn，供 gRPC WithContextDialer 使用。
// 支持的形式：
//   - "host:port"             → TCP（跨机器部署）
//   - "unix:/abs/path.sock"   → Unix socket
//   - "npipe:\\.\pipe\name"   → Windows 命名管道
//   - 裸路径（含 "/"）          → 视为 Unix socket
func DialContextTarget(ctx context.Context, target string) (net.Conn, error) {
	switch {
	case strings.HasPrefix(target, "unix:"):
		return (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(target, "unix:"))
	case strings.HasPrefix(target, "npipe:"):
		return dialNamedPipe(strings.TrimPrefix(target, "npipe:"))
	case strings.HasPrefix(target, `\\.\pipe\`):
		return dialNamedPipe(target)
	case strings.ContainsRune(target, '/'):
		// 裸路径视为 Unix socket
		return (&net.Dialer{}).DialContext(ctx, "unix", target)
	default:
		// host:port 走 TCP（跨机器部署）
		return (&net.Dialer{}).DialContext(ctx, "tcp", target)
	}
}

// DialGRPCAddr 拨号到显式网络地址（用于跨机器部署）。
// addr 可为 host:port（TCP）、unix:/path、npipe:\\.\pipe\name。
func DialGRPCAddr(addr string) (*grpc.ClientConn, error) {
	return grpc.NewClient(
		"passthrough:///"+addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return DialContextTarget(ctx, addr)
		}),
	)
}

// ListenAddr 在显式地址上监听，用于跨机器部署。
// addr 形如 "host:port"（TCP）；若含 "/" 或以 "unix:" 开头则为 Unix socket。
// 空字符串返回错误，调用方应回退到 Listen(workDir, name)。
func ListenAddr(addr string) (net.Listener, error) {
	if addr == "" {
		return nil, fmt.Errorf("empty listen address")
	}
	if strings.HasPrefix(addr, "unix:") {
		return net.Listen("unix", strings.TrimPrefix(addr, "unix:"))
	}
	if strings.ContainsRune(addr, '/') {
		return net.Listen("unix", addr)
	}
	return net.Listen("tcp", addr)
}
