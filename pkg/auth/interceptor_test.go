package auth

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// mustResolver 构造测试用的 resolver，失败即终止（测试 fixture 不该有失败分支）。
func mustResolver(t *testing.T, spec string) *StaticResolver {
	t.Helper()
	r, err := ParseTokens(spec)
	if err != nil {
		t.Fatalf("构造 resolver 失败: %v", err)
	}
	return r
}

// ctxWithAuth 模拟客户端发来的 metadata（gRPC 的 metadata.New/Pairs 会把 key 规范成小写，
// 所以这里同时覆盖 "Authorization" 与 "authorization" 两种写法）。
func ctxWithAuth(key, value string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(key, value))
}

// runUnary 执行 Unary 拦截器，返回 handler 是否被调用以及它看到的 owner。
func runUnary(t *testing.T, r Resolver, ctx context.Context) (called bool, owner string, err error) {
	t.Helper()
	called = false
	owner = ""
	_, err = UnaryInterceptor(r)(ctx, "req", &grpc.UnaryServerInfo{FullMethod: "/gametrace.v1.Registry/Register"},
		func(ctx context.Context, req any) (any, error) {
			called = true
			owner = OwnerFrom(ctx)
			return "resp", nil
		})
	return called, owner, err
}

// TestUnaryInterceptor_AnonymousPassesThrough 验证匿名模式下不带任何凭证的旧客户端仍能通过。
func TestUnaryInterceptor_AnonymousPassesThrough(t *testing.T) {
	t.Parallel()
	r := mustResolver(t, "")
	called, owner, err := runUnary(t, r, context.Background())
	if err != nil {
		t.Fatalf("匿名模式应放行，实际报错: %v", err)
	}
	if !called {
		t.Fatal("匿名模式应调用下游 handler")
	}
	if owner != AnonymousOwner {
		t.Fatalf("匿名模式 owner 应为 %q，实际 %q", AnonymousOwner, owner)
	}
}

// TestUnaryInterceptor_ValidToken 验证 Bearer token 被解析成 owner 并注入 context。
func TestUnaryInterceptor_ValidToken(t *testing.T) {
	t.Parallel()
	r := mustResolver(t, "alice=gt_aaa,bob=gt_bbb")
	called, owner, err := runUnary(t, r, ctxWithAuth("authorization", "Bearer gt_aaa"))
	if err != nil {
		t.Fatalf("正确 token 不应被拒: %v", err)
	}
	if !called {
		t.Fatal("正确 token 应调用下游 handler")
	}
	if owner != "alice" {
		t.Fatalf("owner 应为 alice，实际 %q", owner)
	}
}

// TestUnaryInterceptor_HeaderKeyAndSchemeAreCaseInsensitive 验证大小写不敏感：
// HTTP 的 header 名与 auth scheme 都规定为大小写不敏感，客户端实现五花八门，必须容忍。
func TestUnaryInterceptor_HeaderKeyAndSchemeAreCaseInsensitive(t *testing.T) {
	t.Parallel()
	cases := []struct{ key, value string }{
		{"authorization", "Bearer gt_aaa"},
		{"Authorization", "Bearer gt_aaa"},
		{"AUTHORIZATION", "bearer gt_aaa"},
		{"authorization", "BEARER gt_aaa"},
		{"authorization", "Bearer  gt_aaa"}, // 多余空格
	}
	for _, c := range cases {
		r := mustResolver(t, "alice=gt_aaa")
		called, owner, err := runUnary(t, r, ctxWithAuth(c.key, c.value))
		if err != nil || !called {
			t.Fatalf("%q: %q 应通过，err=%v called=%v", c.key, c.value, err, called)
		}
		if owner != "alice" {
			t.Fatalf("%q: %q owner 应为 alice，实际 %q", c.key, c.value, owner)
		}
	}
}

// TestUnaryInterceptor_Rejects 验证错误凭证被拦在 handler 之外，且错误码是 PermissionDenied。
func TestUnaryInterceptor_Rejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ctx  context.Context
	}{
		{"完全没有 Authorization", context.Background()},
		{"空 metadata", metadata.NewIncomingContext(context.Background(), metadata.MD{})},
		{"错误的 token", ctxWithAuth("authorization", "Bearer gt_zzz")},
		{"空 Bearer", ctxWithAuth("authorization", "Bearer ")},
		{"裸的错误 token", ctxWithAuth("authorization", "gt_zzz")},
		{"header 名不对", ctxWithAuth("x-auth", "Bearer gt_aaa")},
		{"已撤销的 token", ctxWithAuth("authorization", "Bearer gt_ccc")},
		{"token 被截断", ctxWithAuth("authorization", "Bearer gt_aa")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := mustResolver(t, "alice=gt_aaa,bob=gt_bbb")
			called, _, err := runUnary(t, r, c.ctx)
			if err == nil {
				t.Fatal("应被拒绝，实际放行")
			}
			if st, ok := status.FromError(err); !ok || st.Code() != codes.PermissionDenied {
				t.Fatalf("错误码应为 PermissionDenied，实际 %v", err)
			}
			if called {
				t.Fatal("被拒绝时绝不能调用下游 handler")
			}
		})
	}
}

// TestStreamInterceptor 验证流拦截器与一元拦截器行为一致（注册插件、心跳都走流）。
func TestStreamInterceptor(t *testing.T) {
	t.Parallel()

	t.Run("匿名模式放行", func(t *testing.T) {
		t.Parallel()
		called, owner, err := runStream(t, mustResolver(t, ""), context.Background())
		if err != nil || !called {
			t.Fatalf("匿名模式应放行: err=%v called=%v", err, called)
		}
		if owner != AnonymousOwner {
			t.Fatalf("owner 应为 %q，实际 %q", AnonymousOwner, owner)
		}
	})

	t.Run("正确 token 注入 owner", func(t *testing.T) {
		t.Parallel()
		called, owner, err := runStream(t, mustResolver(t, "alice=gt_aaa"), ctxWithAuth("authorization", "Bearer gt_aaa"))
		if err != nil || !called {
			t.Fatalf("正确 token 应放行: err=%v called=%v", err, called)
		}
		if owner != "alice" {
			t.Fatalf("owner 应为 alice，实际 %q", owner)
		}
	})

	t.Run("缺 token 被拒且不进 handler", func(t *testing.T) {
		t.Parallel()
		called, _, err := runStream(t, mustResolver(t, "alice=gt_aaa"), context.Background())
		if err == nil {
			t.Fatal("应被拒绝")
		}
		if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied {
			t.Fatalf("错误码应为 PermissionDenied，实际 %v", err)
		}
		if called {
			t.Fatal("被拒绝时绝不能调用下游 handler")
		}
	})
}

// fakeServerStream 是 grpc.ServerStream 的最小实现，只为把 context 交给拦截器。
// 用真实 gRPC server 测这段逻辑需要 bufconn + 完整 service，收益不抵复杂度。
type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f *fakeServerStream) Context() context.Context { return f.ctx }

func runStream(t *testing.T, r Resolver, ctx context.Context) (called bool, owner string, err error) {
	t.Helper()
	called = false
	owner = ""
	ss := &fakeServerStream{ctx: ctx}
	err = StreamInterceptor(r)(nil, ss, &grpc.StreamServerInfo{FullMethod: "/gametrace.v1.Registry/Heartbeat"},
		func(srv any, stream grpc.ServerStream) error {
			called = true
			owner = OwnerFrom(stream.Context())
			return nil
		})
	return called, owner, err
}
