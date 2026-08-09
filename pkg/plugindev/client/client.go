// Package client is the gta-mcp side of the Developer Plane: a thin gRPC
// adapter that lets the MCP layer call pkg/plugindev without knowing anything
// about the transport or the filesystem. This is what keeps gta-mcp free of
// exec.Command and os.WriteFile — every implementation routes through the
// PluginDev gRPC service.
package client

import (
	"context"

	pb "gta/pkg/plugindev/proto"
	"google.golang.org/grpc"
)

// PluginDev is the subset of the Developer Plane gta-mcp forwards to. Keeping
// it an interface (rather than a raw gRPC client) makes the MCP handlers
// trivially testable with an in-memory fake.
type PluginDev interface {
	Scaffold(ctx context.Context, name, protocol, protocolVersion string, hints []string) (*pb.ScaffoldResponse, error)
	ListPlugins(ctx context.Context) (*pb.ListPluginsResponse, error)
	Build(ctx context.Context, name string, timeoutSec int) (*pb.BuildResponse, error)
}

type grpcClient struct {
	cc pb.PluginDevClient
}

// NewGRPCClient wraps a gRPC connection as a PluginDev.
func NewGRPCClient(conn *grpc.ClientConn) PluginDev {
	return &grpcClient{cc: pb.NewPluginDevClient(conn)}
}

func (c *grpcClient) Scaffold(ctx context.Context, name, protocol, protocolVersion string, hints []string) (*pb.ScaffoldResponse, error) {
	return c.cc.Scaffold(ctx, &pb.ScaffoldRequest{
		Name:            name,
		Protocol:        protocol,
		ProtocolVersion: protocolVersion,
		Hints:           hints,
	})
}

func (c *grpcClient) ListPlugins(ctx context.Context) (*pb.ListPluginsResponse, error) {
	return c.cc.ListPlugins(ctx, &pb.ListPluginsRequest{})
}

func (c *grpcClient) Build(ctx context.Context, name string, timeoutSec int) (*pb.BuildResponse, error) {
	return c.cc.Build(ctx, &pb.BuildRequest{Name: name, TimeoutSec: int32(timeoutSec)})
}
