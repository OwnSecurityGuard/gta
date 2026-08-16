//go:build !windows

package plugin

import (
	"fmt"
	"net"
	"strings"

	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"

	"google.golang.org/grpc"
)

// StartListen 监听 registry socket，返回 *grpc.Server（调用方负责 Serve + Close）。
// addr 格式："unix:/path/to/sock" 或文件路径。
func (s *RegistryServer) StartListen(addr string) (*grpc.Server, net.Listener, error) {
	// 如果是纯路径（无 unix: 前缀），转为 unix socket
	if !strings.HasPrefix(addr, "unix:") {
		addr = "unix:" + addr
	}
	lis, err := net.Listen("unix", strings.TrimPrefix(addr, "unix:"))
	if err != nil {
		return nil, nil, fmt.Errorf("listen registry socket: %w", err)
	}
	grpcSrv := grpc.NewServer()
	pb.RegisterPluginRegistryServer(grpcSrv, s)
	return grpcSrv, lis, nil
}
