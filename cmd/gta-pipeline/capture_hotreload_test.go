package main

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
)

// fakeDecoderClient 仅用于区分"不同插件实例"（指针不同即视为不同实例）
// 并满足 pb.DecoderClient 接口，便于单测热加载策略。
type fakeDecoderClient struct{ id int }

func (fakeDecoderClient) DecodeV2(ctx context.Context, opts ...grpc.CallOption) (grpc.BidiStreamingClient[pb.DecodeRequest, pb.DecodeResponseV2], error) {
	return nil, nil
}

func TestDecoderAction(t *testing.T) {
	c1 := &fakeDecoderClient{1}
	c2 := &fakeDecoderClient{2}

	cases := []struct {
		name           string
		client         pb.DecoderClient
		current        pb.DecoderClient
		haveDispatcher bool
		want           string
	}{
		{"no plugin, no dispatcher", nil, nil, false, "idle"},
		{"plugin gone, had dispatcher", nil, c1, true, "drop"},
		{"same instance, has dispatcher", c1, c1, true, "keep"},
		{"same instance, no dispatcher", c1, c1, false, "keep"},
		{"new instance replaces", c2, c1, true, "build"},
		{"first plugin appears", c1, nil, false, "build"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decoderAction(tc.client, tc.current, tc.haveDispatcher)
			if got != tc.want {
				t.Errorf("decoderAction() = %q, want %q", got, tc.want)
			}
		})
	}
}
