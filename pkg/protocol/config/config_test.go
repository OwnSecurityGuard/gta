package config

import (
	"strings"
	"testing"
)

func TestParseValid(t *testing.T) {
	data := `
message:
  id:
    expr: "header.cmd"
  definitions:
    - value: 1001
      name: LoginRequest
      role: request
    - value: "2001"
      name: PlayerNotify
      role: push

correlation:
  rules:
    - name: rpc_seq
      request:
        expr: "body.seq"
      response:
        expr: "body.seq"

push:
  rules:
    - name: seq_zero
      when:
        expr: "body.seq"
        equals: 0

error:
  code:
    expr: "body.error_code"
  success:
    values:
      - 0
`
	f, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Message == nil || f.Message.ID.Expr != "header.cmd" {
		t.Fatalf("message.id.expr not parsed: %+v", f.Message)
	}
	if len(f.Message.Definitions) != 2 {
		t.Fatalf("expected 2 definitions, got %d", len(f.Message.Definitions))
	}
	if len(f.Correlation.Rules) != 1 {
		t.Fatalf("expected 1 correlation rule, got %d", len(f.Correlation.Rules))
	}
	if f.Error == nil || f.Error.Code.Expr != "body.error_code" {
		t.Fatalf("error config not parsed: %+v", f.Error)
	}
}

func TestWhenScalarNormalized(t *testing.T) {
	f, err := Parse([]byte(`
push:
  rules:
    - name: seq_zero
      when:
        expr: "body.seq"
        equals: 0
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := f.Push.Rules[0].When.EqualsList()
	if len(got) != 1 || got[0] != 0 {
		t.Fatalf("EqualsList: got %#v", got)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "missing message id expr",
			yaml: `message:
  id: {}
  definitions:
    - value: 1001
      name: LoginRequest`,
			want: "message.id.expr is required",
		},
		{
			name: "correlation missing exprs",
			yaml: `correlation:
  rules:
    - name: rpc_seq
      request:
        expr: "body.seq"
      response: {}`,
			want: "requires request.expr and response.expr",
		},
		{
			name: "push missing expr",
			yaml: `push:
  rules:
    - name: n
      when:
        equals: 1`,
			want: "when.expr is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	f, err := Load("testdata/protocol.yaml")
	if err != nil {
		t.Fatalf("Load testdata: %v", err)
	}
	if f.Message.ID.Expr != "header.cmd" {
		t.Fatalf("unexpected message id expr: %q", f.Message.ID.Expr)
	}
}
