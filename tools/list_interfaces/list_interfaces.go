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
	resp, err := client.ListInterfaces(context.Background(), &pb.ListInterfacesRequest{})
	if err != nil {
		log.Fatal(err)
	}
	for _, name := range resp.Names {
		fmt.Println(name)
	}
}
