package resolver

import (
	"testing"

	"gametrace/pkg/protocol"
	"gametrace/pkg/protocol/config"
)

const testYAML = `
message:
  id:
    expr: "header.cmd"
  definitions:
    - value: 1001
      name: LoginRequest
      role: request
    - value: 1002
      name: LoginResponse
      role: response
    - value: 2001
      name: PlayerNotify
      role: push

correlation:
  rules:
    - name: rpc_seq
      request:
        expr: "body.seq"
      response:
        expr: "body.seq"

error:
  code:
    expr: "body.error_code"
  success:
    values:
      - 0
`

func newTestResolver(t *testing.T) *ProtocolResolver {
	t.Helper()
	cfg, err := config.Parse([]byte(testYAML))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("new resolver: %v", err)
	}
	return r
}

func TestResolveRequest(t *testing.T) {
	r := newTestResolver(t)
	res := r.Resolve(`{"header":{"cmd":1001},"body":{"seq":10,"error_code":0}}`)

	if !res.HasMessage || res.Message != "LoginRequest" {
		t.Fatalf("message = %s (has=%v)", res.Message, res.HasMessage)
	}
	if res.Role != protocol.RoleRequest {
		t.Fatalf("role = %s, want request", res.Role)
	}
	if res.Delivery != "request" {
		t.Fatalf("delivery = %s", res.Delivery)
	}
	if res.Correlation == nil || res.Correlation.Direction != "request" ||
		res.Correlation.Key != "seq" || res.Correlation.Value != "10" {
		t.Fatalf("correlation = %+v", res.Correlation)
	}
	if res.Error == nil || res.Error.Failed {
		t.Fatalf("error should be success, got %+v", res.Error)
	}
}

func TestResolveResponse(t *testing.T) {
	r := newTestResolver(t)
	res := r.Resolve(`{"header":{"cmd":1002},"body":{"seq":10}}`)

	if res.Message != "LoginResponse" {
		t.Fatalf("message = %s", res.Message)
	}
	if res.Role != protocol.RoleResponse {
		t.Fatalf("role = %s", res.Role)
	}
	if res.Correlation == nil || res.Correlation.Direction != "response" ||
		res.Correlation.Value != "10" {
		t.Fatalf("correlation = %+v", res.Correlation)
	}
}

func TestResolvePush(t *testing.T) {
	r := newTestResolver(t)
	res := r.Resolve(`{"header":{"cmd":2001},"body":{"seq":5}}`)

	if res.Message != "PlayerNotify" {
		t.Fatalf("message = %s", res.Message)
	}
	if res.Role != protocol.RolePush {
		t.Fatalf("role = %s", res.Role)
	}
	if res.Delivery != "push" {
		t.Fatalf("delivery = %s", res.Delivery)
	}
}

func TestResolveUnknown(t *testing.T) {
	r := newTestResolver(t)
	res := r.Resolve(`{"header":{"cmd":9999},"body":{"seq":1}}`)
	if res.HasMessage {
		t.Fatalf("unexpected known message: %+v", res)
	}
	if res.Role != protocol.RoleUnknown {
		t.Fatalf("role = %s, want unknown", res.Role)
	}
	if res.Delivery != "unknown" {
		t.Fatalf("delivery = %s", res.Delivery)
	}
}

func TestResolveErrorFailed(t *testing.T) {
	r := newTestResolver(t)
	res := r.Resolve(`{"header":{"cmd":1002},"body":{"seq":1,"error_code":10001}}`)
	if res.Error == nil || !res.Error.Failed || res.Error.Code != "10001" {
		t.Fatalf("error = %+v", res.Error)
	}
}

func TestPlatformContext(t *testing.T) {
	r := newTestResolver(t)
	ctx := r.Resolve(`{"header":{"cmd":1001},"body":{"seq":10,"error_code":0}}`).ProtocolContext()
	if ctx.Message != "LoginRequest" || ctx.Role != protocol.RoleRequest {
		t.Fatalf("context = %+v", ctx)
	}
	if ctx.Correlation == nil || ctx.Correlation.Value != "10" {
		t.Fatalf("context correlation = %+v", ctx.Correlation)
	}
}

func TestNewRejectsInvalidRole(t *testing.T) {
	cfg, err := config.Parse([]byte(`
message:
  id:
    expr: "header.cmd"
  definitions:
    - value: 1001
      name: X
      role: bogus
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := New(cfg); err == nil {
		t.Fatalf("expected error for invalid role")
	}
}
