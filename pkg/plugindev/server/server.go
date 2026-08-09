// Package server exposes the Developer Plane as a gRPC service. It is a thin
// glue layer that maps proto messages onto pkg/plugindev domain functions and
// owns no filesystem or subprocess logic of its own — that all lives in
// pkg/plugindev so the same code can run embedded (in gta-mcp for dev) or as
// the standalone gta-plugin-dev binary.
package server

import (
	"context"
	"net"

	"gta/pkg/internalipc"
	"gta/pkg/plugindev"
	pb "gta/pkg/plugindev/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Server implements the PluginDev gRPC service. It is scoped to a single
// plugins directory (root) injected at construction time — clients never
// specify where files land, which keeps filesystem ownership entirely server
// side.
type Server struct {
	pb.UnimplementedPluginDevServer
	root string
}

// New constructs a Server scoped to root (the plugins directory).
func New(root string) *Server {
	return &Server{root: root}
}

func (s *Server) Scaffold(ctx context.Context, req *pb.ScaffoldRequest) (*pb.ScaffoldResponse, error) {
	resp, err := plugindev.Scaffold(ctx, &plugindev.ScaffoldRequest{
		Name:            req.GetName(),
		Protocol:        req.GetProtocol(),
		ProtocolVersion: req.GetProtocolVersion(),
		Hints:           req.GetHints(),
		Root:            s.root,
	})
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &pb.ScaffoldResponse{
		Name:      resp.Name,
		OutputDir: resp.OutputDir,
		Created:   resp.Created,
	}, nil
}

func (s *Server) ListPlugins(ctx context.Context, req *pb.ListPluginsRequest) (*pb.ListPluginsResponse, error) {
	plugins, err := plugindev.ListPlugins(s.root)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &pb.ListPluginsResponse{}
	for _, p := range plugins {
		out.Plugins = append(out.Plugins, &pb.DiscoveredPlugin{
			Name:   p.Name,
			Binary: p.Binary,
			Dir:    p.Dir,
		})
	}
	return out, nil
}

func (s *Server) Build(ctx context.Context, req *pb.BuildRequest) (*pb.BuildResponse, error) {
	resp, err := plugindev.Build(ctx, &plugindev.BuildRequest{
		Root:       s.root,
		Name:       req.GetName(),
		TimeoutSec: int(req.GetTimeoutSec()),
	})
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	out := &pb.BuildResponse{Ok: resp.OK, Output: resp.Output}
	for _, e := range resp.Errors {
		out.Errors = append(out.Errors, &pb.BuildError{
			File:    e.File,
			Line:    int32(e.Line),
			Col:     int32(e.Col),
			Message: e.Message,
		})
	}
	return out, nil
}

func (s *Server) Activate(ctx context.Context, req *pb.ActivateRequest) (*pb.ActivateResponse, error) {
	resp, err := plugindev.Activate(ctx, &plugindev.ActivateRequest{
		Root:         s.root,
		Name:         req.GetName(),
		RegistryAddr: req.GetRegistryAddr(),
	})
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &pb.ActivateResponse{
		InstanceId: resp.InstanceID,
		Ok:         resp.OK,
		Message:    resp.Message,
	}, nil
}

func (s *Server) Deactivate(ctx context.Context, req *pb.DeactivateRequest) (*pb.DeactivateResponse, error) {
	resp, err := plugindev.Deactivate(ctx, &plugindev.DeactivateRequest{
		Root: s.root,
		Name: req.GetName(),
	})
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &pb.DeactivateResponse{
		Ok:      resp.OK,
		Message: resp.Message,
	}, nil
}

func (s *Server) Status(ctx context.Context, req *pb.StatusRequest) (*pb.StatusResponse, error) {
	ps, err := plugindev.Status(ctx, &plugindev.StatusRequest{
		Root: s.root,
		Name: req.GetName(),
	})
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	out := &pb.StatusResponse{Name: ps.Name}
	if ps.Artifact != nil {
		out.Artifact = &pb.ArtifactState{
			State:       ps.Artifact.State,
			SourceDir:   ps.Artifact.SourceDir,
			BinaryPath:  ps.Artifact.BinaryPath,
			BinaryStale: ps.Artifact.BinaryStale,
		}
	}
	if ps.DevProcess != nil {
		out.DevProcess = &pb.DevProcess{
			Launched:        ps.DevProcess.Launched,
			Pid:             int64(ps.DevProcess.PID),
			InstanceId:      ps.DevProcess.InstanceID,
			Alive:           ps.DevProcess.Alive,
			LaunchedAtUnix:  ps.DevProcess.LaunchedAt.Unix(),
		}
	}
	if ps.LastAttempt != nil {
		out.LastAttempt = mapLastAttempt(ps.LastAttempt)
	}
	return out, nil
}

func mapLastAttempt(a *plugindev.LastAttempt) *pb.LastAttempt {
	out := &pb.LastAttempt{
		Action:    a.Action,
		Ok:        a.OK,
		AtUnix:    a.At.Unix(),
		DurationMs: a.Duration.Milliseconds(),
		Message:   a.Message,
		ExplainRef: a.ExplainRef,
	}
	for _, e := range a.Errors {
		out.Errors = append(out.Errors, &pb.BuildError{
			File:    e.File,
			Line:    int32(e.Line),
			Col:     int32(e.Col),
			Message: e.Message,
		})
	}
	return out
}

// Serve registers the service on lis and blocks serving until the listener
// closes. It is separated from Start so callers (e.g. gta-mcp) can embed the
// server behind an already-bound listener.
func (s *Server) Serve(lis net.Listener) error {
	grpcServer := grpc.NewServer()
	pb.RegisterPluginDevServer(grpcServer, s)
	return grpcServer.Serve(lis)
}

// Start binds addr (host:port, unix:/path, npipe:\\.\pipe\name) and serves.
func (s *Server) Start(addr string) error {
	lis, err := internalipc.ListenAddr(addr)
	if err != nil {
		return err
	}
	return s.Serve(lis)
}
