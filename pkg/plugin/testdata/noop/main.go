package main

import (
	"fmt"
	"net"
	"os"

	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
	"google.golang.org/grpc"
)

type server struct {
	pb.UnimplementedDecoderServer
}

func (s *server) Decode(stream pb.Decoder_DecodeServer) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			return err
		}
		// 业务字段统一放在 data 子对象下，顶层只允许 data 和 _fields
		_ = stream.Send(&pb.DecodeResponse{
			SessionId: req.SessionId,
			Json:      []byte(`{"data":{"protocol":"noop"}}`),
		})
	}
}

func main() {
	socket := os.Args[1]
	_ = os.Remove(socket)
	lis, err := net.Listen("unix", socket)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	srv := grpc.NewServer()
	pb.RegisterDecoderServer(srv, &server{})
	if err := srv.Serve(lis); err != nil {
		fmt.Println(err)
	}
}
