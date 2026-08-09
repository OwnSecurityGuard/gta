//go:build !windows

package plugin

import (
	"fmt"
	"net"
)

// StartListen 监听 registry socket，返回 *grpc.Server（调用方负责 Serve + Close）。
// addr 格式："unix:/path/to/sock" 或文件路径。
func (s *RegistryServer) StartListen(addr string) (*grpc.Server, net.Listener, error) {
	// 如果是纯路径（无 unix: 前缀），转为 unix socket
	if !hasPrefix(addr, "unix:") {
		addr = "unix:" + addr
	}
	lis, err := net.Listen("unix", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("listen registry socket: %w", err)
	}
	grpcSrv := grpc.NewServer()
	pb.RegisterPluginRegistryServer(grpcSrv, s)
	return grpcSrv, lis, nil
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
