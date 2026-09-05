package resolver

import (
	"testing"

	"gametrace/pkg/protocol/correlation"
)

// correlationKey 把一条消息的关联信息（规则名 + 值）在指定 flow 内定位到唯一键。
// Request/Response 同 rule、同 value，因而键一致，可完成配对。
func correlationKey(flow, rule, value string) string {
	return flow + "|" + rule + "|" + value
}

// TestRequestResponsePairing 端到端验证：Request 被记住，Response 据此配对到前驱。
func TestRequestResponsePairing(t *testing.T) {
	r := newTestResolver(t)
	store := correlation.New(0)

	const flow = "1234|5678"

	// 收到 Request
	reqRes := r.Resolve(`{"header":{"cmd":1001},"body":{"seq":10}}`)
	if reqRes.Correlation == nil || reqRes.Correlation.Direction != "request" {
		t.Fatalf("expected request correlation, got %+v", reqRes.Correlation)
	}
	key := correlationKey(flow, reqRes.Correlation.Rule, reqRes.Correlation.Value)
	store.Remember(key, "req-event-1")

	// 收到 Response
	respRes := r.Resolve(`{"header":{"cmd":1002},"body":{"seq":10}}`)
	if respRes.Correlation == nil || respRes.Correlation.Direction != "response" {
		t.Fatalf("expected response correlation, got %+v", respRes.Correlation)
	}
	respKey := correlationKey(flow, respRes.Correlation.Rule, respRes.Correlation.Value)
	if respKey != key {
		t.Fatalf("response key %q != request key %q", respKey, key)
	}
	p, ok := store.Lookup(respKey)
	if !ok {
		t.Fatalf("response did not match a pending request")
	}
	if p.CausationID != "req-event-1" {
		t.Fatalf("causation = %q, want req-event-1", p.CausationID)
	}
}
