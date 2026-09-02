package main

import (
	"testing"
	"time"

	"gta/pkg/event"
	protocolconfig "gta/pkg/protocol/config"
	protocolcorrelation "gta/pkg/protocol/correlation"
	protocolresolver "gta/pkg/protocol/resolver"
)

func newProtocolTask(t *testing.T) *captureTask {
	t.Helper()
	cfg, err := protocolconfig.Parse([]byte(protocolTestYAML))
	if err != nil {
		t.Fatalf("parse protocol config: %v", err)
	}
	r, err := protocolresolver.New(cfg)
	if err != nil {
		t.Fatalf("new resolver: %v", err)
	}
	return &captureTask{
		protocolResolver: r,
		corrStore:        protocolcorrelation.New(0),
	}
}

const protocolTestYAML = `
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

correlation:
  rules:
    - name: rpc_seq
      request:
        expr: "body.seq"
      response:
        expr: "body.seq"
`

func mustEvent(t *testing.T, json string) *event.Event {
	t.Helper()
	v, err := event.ValueFromJSON([]byte(json))
	if err != nil {
		t.Fatalf("ValueFromJSON: %v", err)
	}
	return &event.Event{
		Identity: event.Identity{ID: event.NewEventID(), Type: "test", Timestamp: time.Now()},
		Context:  event.EventContext{FlowID: "flow-1"},
		Payload:  event.Payload{SchemaID: "test.v1", Value: v},
	}
}

func TestEnrichProtocolRequest(t *testing.T) {
	task := newProtocolTask(t)
	ev := mustEvent(t, `{"header":{"cmd":1001},"body":{"seq":10}}`)
	task.enrichProtocol(ev)

	// _meta.protocol 已被写入
	proto, ok := ev.MetaValue("protocol")
	if !ok {
		t.Fatalf("_meta.protocol not set")
	}
	obj, ok := proto.AsObject()
	if !ok {
		t.Fatalf("_meta.protocol not object: %v", proto)
	}
	if msg, _ := obj["message"]; msg.Str != "LoginRequest" {
		t.Fatalf("message = %v", msg)
	}
	if role, _ := obj["role"]; role.Str != "request" {
		t.Fatalf("role = %v", role)
	}

	// 请求已被记住
	if task.corrStore.Len() != 1 {
		t.Fatalf("pending requests = %d, want 1", task.corrStore.Len())
	}
}

func TestEnrichProtocolResponsePairing(t *testing.T) {
	task := newProtocolTask(t)

	// Request
	req := mustEvent(t, `{"header":{"cmd":1001},"body":{"seq":10}}`)
	reqID := req.Identity.ID
	task.enrichProtocol(req)

	// Response 用同 seq 配对，CausationID 应指向请求
	resp := mustEvent(t, `{"header":{"cmd":1002},"body":{"seq":10}}`)
	task.enrichProtocol(resp)

	if !resp.Trace.HasCausation() || string(resp.Trace.CausationID) != string(reqID) {
		t.Fatalf("response causation = %q, want %q", resp.Trace.CausationID, reqID)
	}
	if resp.Trace.CorrelationID != "flow-1|rpc_seq|10" {
		t.Fatalf("response correlation id = %q", resp.Trace.CorrelationID)
	}
	// 请求方也共享同一 group key
	if req.Trace.CorrelationID != "flow-1|rpc_seq|10" {
		t.Fatalf("request correlation id = %q", req.Trace.CorrelationID)
	}
}

func TestEnrichProtocolNoResolver(t *testing.T) {
	task := &captureTask{}
	ev := mustEvent(t, `{"header":{"cmd":1001},"body":{"seq":10}}`)
	task.enrichProtocol(ev) // 不应 panic
	if _, ok := ev.MetaValue("protocol"); ok {
		t.Fatalf("protocol should not be set without resolver")
	}
}
