//go:build windows

package plugin

import (
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"

	"google.golang.org/grpc"
)

// StartListen 在 Windows 上使用命名管道监听 registry socket。
// addr 应为管道名（不含 \\.\pipe\ 前缀），或完整管道路径。
func (s *RegistryServer) StartListen(addr string) (*grpc.Server, net.Listener, error) {
	// 如果 addr 不含 \\.\pipe\ 前缀，添加它
	if len(addr) < 10 || addr[:10] != `\\.\pipe\` {
		addr = `\\.\pipe\gt-` + addr
	}
	lis, err := winio.ListenPipe(addr, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("listen registry socket: %w", err)
	}
	grpcSrv := grpc.NewServer()
	pb.RegisterPluginRegistryServer(grpcSrv, s)
	return grpcSrv, lis, nil
}
