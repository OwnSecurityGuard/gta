package auth

import (
	"context"
	"testing"
)

// TestPrincipalFrom_Empty 验证未注入身份时安全退化：不 panic，返回空串 / false。
// 下游（插件命名空间、会话隔离）依赖 "" 表示「无主」，是显式约定而非意外。
func TestPrincipalFrom_Empty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if got := OwnerFrom(ctx); got != "" {
		t.Fatalf("空 context 的 owner 应为 \"\"，实际 %q", got)
	}
	if p, ok := PrincipalFrom(ctx); ok || p != nil {
		t.Fatalf("空 context 不应取到 Principal: %+v ok=%v", p, ok)
	}
}

// TestWithPrincipal_RoundTrip 验证写入与读出一致。
func TestWithPrincipal_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := WithPrincipal(context.Background(), &Principal{Owner: "alice", IsAdmin: true})
	p, ok := PrincipalFrom(ctx)
	if !ok {
		t.Fatal("应能取回注入的 Principal")
	}
	if p.Owner != "alice" || !p.IsAdmin {
		t.Fatalf("Principal 往返不一致: %+v", p)
	}
	if got := OwnerFrom(ctx); got != "alice" {
		t.Fatalf("OwnerFrom 应为 alice，实际 %q", got)
	}
}

// TestWithPrincipal_Nil 验证注入 nil 不会造成下游 panic（防御调用方误用）。
func TestWithPrincipal_Nil(t *testing.T) {
	t.Parallel()
	ctx := WithPrincipal(context.Background(), nil)
	if got := OwnerFrom(ctx); got != "" {
		t.Fatalf("nil Principal 的 owner 应为 \"\"，实际 %q", got)
	}
	if _, ok := PrincipalFrom(ctx); ok {
		t.Fatal("nil Principal 应视为未取到")
	}
}
