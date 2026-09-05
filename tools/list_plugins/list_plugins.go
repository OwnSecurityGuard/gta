package main

import (
	"context"
	"fmt"
	"log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "gametrace/pkg/internalipc/proto"
)

func main() {
	conn, err := grpc.NewClient("localhost:8088", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	client := pb.NewCaptureControlClient(conn)
	resp, err := client.ListPlugins(context.Background(), &pb.ListPluginsRequest{})
	if err != nil {
		log.Fatal(err)
	}
	for _, p := range resp.Plugins {
		fmt.Printf("name=%s protocol=%s type=%s online=%v instance_id=%s\n", p.Name, p.Protocol, p.Type, p.Online, p.InstanceId)
	}
}
