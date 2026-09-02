package main

import (
	"fmt"
	"os"
	"testing"

	sdk "github.com/OwnSecurityGuard/gta-plugin-sdk"
	sdkcontract "github.com/OwnSecurityGuard/gta-plugin-sdk/contract"
	sdkevent "github.com/OwnSecurityGuard/gta-plugin-sdk/event"
	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
	"google.golang.org/grpc"
)

// captureStream is a minimal Decoder_DecodeV2Server mock that records Send calls.
type captureStream struct {
	grpc.BidiStreamingServer[pb.DecodeRequest, pb.DecodeResponseV2]
	responses []*pb.DecodeResponseV2
}

func (c *captureStream) Send(r *pb.DecodeResponseV2) error {
	c.responses = append(c.responses, r)
	return nil
}

func loadManifest(t *testing.T) *sdk.Manifest {
	t.Helper()
	raw, err := os.ReadFile("plugin.yaml")
	if err != nil {
		t.Fatalf("read plugin.yaml: %v", err)
	}
	m, err := sdk.ParseManifest(raw)
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if err := sdk.ValidateManifest(m); err != nil {
		t.Fatalf("validate manifest: %v", err)
	}
	return m
}

// TestManifestContractCheck runs the declaration-phase contract check on
// plugin.yaml: runtime / schema / state layers must be violation-free.
func TestManifestContractCheck(t *testing.T) {
	m := loadManifest(t)
	rep := sdkcontract.NewPluginChecker().Check(m)
	for _, v := range rep.Violations {
		t.Errorf("contract %s: %s", v.RuleID, v.Message)
	}
}

func TestParseMessageRequest(t *testing.T) {
	body := `{"header":{"cmd":1001},"body":{"seq":1234}}`
	raw := fmt.Sprintf("POST /echo HTTP/1.1\r\nHost: 127.0.0.1:8984\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	msg, n, ok := parseMessage([]byte(raw))
	if !ok {
		t.Fatal("expected request to parse")
	}
	if n != len(raw) {
		t.Fatalf("consumed = %d, want %d", n, len(raw))
	}
	if !msg.isRequest || msg.method != "POST" || msg.path != "/echo" {
		t.Fatalf("unexpected message: %+v", msg)
	}
	if string(msg.body) != body {
		t.Fatalf("unexpected body: %q", msg.body)
	}
}

func TestParseMessageResponse(t *testing.T) {
	body := `{"header":{"cmd":1002},"body":{"seq":1234}}`
	raw := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	msg, n, ok := parseMessage([]byte(raw))
	if !ok {
		t.Fatal("expected response to parse")
	}
	if n != len(raw) {
		t.Fatalf("consumed = %d, want %d", n, len(raw))
	}
	if msg.isRequest || msg.status != 200 {
		t.Fatalf("unexpected message: %+v", msg)
	}
	if string(msg.body) != body {
		t.Fatalf("unexpected body: %q", msg.body)
	}
}

func TestParseMessageIncomplete(t *testing.T) {
	raw := "POST /echo HTTP/1.1\r\nHost: 127.0.0.1:8984\r\nContent-Length: 39\r\n\r\n" +
		`{"header":{"cm`
	_, _, ok := parseMessage([]byte(raw))
	if ok {
		t.Fatal("incomplete message must not parse")
	}
}

func TestParseMessageTwoInOneBuffer(t *testing.T) {
	first := "POST /a HTTP/1.1\r\nHost: h\r\nContent-Length: 2\r\n\r\n{}"
	second := "POST /b HTTP/1.1\r\nHost: h\r\nContent-Length: 2\r\n\r\n{}"
	buf := []byte(first + second)
	_, n, ok := parseMessage(buf)
	if !ok || n != len(first) {
		t.Fatalf("first message: ok=%v consumed=%d want=%d", ok, n, len(first))
	}
	_, n2, ok2 := parseMessage(buf[n:])
	if !ok2 || n2 != len(second) {
		t.Fatalf("second message: ok=%v consumed=%d want=%d", ok2, n2, len(second))
	}
}

func TestParseEnvelopeFormats(t *testing.T) {
	cases := []struct {
		name string
		body string
		want envelopeSemantics
	}{
		{"login_request", `{"header":{"cmd":1001},"body":{"seq":1234}}`,
			envelopeSemantics{Cmd: 1001, MsgName: "LoginRequest", IsPush: false, Seq: 1234, IsError: false}},
		{"login_response", `{"header":{"cmd":1002},"body":{"seq":1234}}`,
			envelopeSemantics{Cmd: 1002, MsgName: "LoginResponse", IsPush: false, Seq: 1234, IsError: false}},
		{"push_by_cmd", `{"header":{"cmd":2001},"body":{"seq":0}}`,
			envelopeSemantics{Cmd: 2001, MsgName: "PlayerNotify", IsPush: true, Seq: 0, IsError: false}},
		{"push_by_seq_zero", `{"header":{"cmd":1001},"body":{"seq":0}}`,
			envelopeSemantics{Cmd: 1001, MsgName: "LoginRequest", IsPush: true, Seq: 0, IsError: false}},
		{"error", `{"header":{"cmd":1002},"body":{"seq":1234,"error_code":1}}`,
			envelopeSemantics{Cmd: 1002, MsgName: "LoginResponse", IsPush: false, Seq: 1234, ErrorCode: 1, IsError: true}},
		{"success_with_error_code_zero", `{"header":{"cmd":1002},"body":{"seq":1234,"error_code":0}}`,
			envelopeSemantics{Cmd: 1002, MsgName: "LoginResponse", IsPush: false, Seq: 1234, IsError: false}},
		{"unknown_empty", `{}`,
			envelopeSemantics{Cmd: 0, MsgName: "unknown", IsPush: true, IsError: false}},
	}
	for _, tc := range cases {
		got := parseEnvelope([]byte(tc.body))
		if got != tc.want {
			t.Errorf("%s: got %+v want %+v", tc.name, got, tc.want)
		}
	}
}

// TestEmitSchemaConformance emits request and response events and runs the
// runtime event/schema/state checks against the declared manifest, so any drift
// between emit.go and plugin.yaml surfaces as a test failure.
func TestEmitSchemaConformance(t *testing.T) {
	m := loadManifest(t)
	pc := sdkcontract.NewPluginChecker()
	d := newDecoder()
	flowID := "tcp 127.0.0.1:12345=127.0.0.1:8984"

	req := &httpMessage{
		isRequest: true,
		method:    "POST",
		path:      "/echo",
		body:      []byte(`{"header":{"cmd":1001},"body":{"seq":1234}}`),
	}
	stream := &captureStream{}
	if err := d.emit(stream, "in-1", flowID, req); err != nil {
		t.Fatalf("emit request: %v", err)
	}

	resp := &httpMessage{
		isRequest: false,
		status:    200,
		body:      []byte(`{"header":{"cmd":1002},"body":{"seq":1234,"error_code":1}}`),
	}
	if err := d.emit(stream, "in-2", flowID, resp); err != nil {
		t.Fatalf("emit response: %v", err)
	}

	if len(stream.responses) != 2 {
		t.Fatalf("responses = %d, want 2", len(stream.responses))
	}
	for _, r := range stream.responses {
		if r.Done {
			t.Fatalf("unexpected done response for %s", r.EventType)
		}
		v, err := sdkevent.UnmarshalValueMsgpack(r.PayloadMsgpack)
		if err != nil {
			t.Fatalf("unmarshal payload for %s: %v", r.EventType, err)
		}
		draft := &sdkevent.Draft{
			Type:      sdkevent.EventType(r.EventType),
			SchemaRef: r.SchemaId,
			Value:     v,
		}
		rep := pc.CheckEvent(m, draft)
		for _, viol := range rep.Violations {
			t.Errorf("%s: %s: %s", r.EventType, viol.RuleID, viol.Message)
		}
	}
}
