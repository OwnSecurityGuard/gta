package matcher

import (
	"errors"
	"testing"
)

func TestGet(t *testing.T) {
	json := `{"header":{"cmd":1001},"body":{"seq":123}}`

	v, ok := Get(json, "header.cmd")
	if !ok || v != "1001" {
		t.Fatalf("Get header.cmd = %q, %v", v, ok)
	}
	if _, ok := Get(json, "body.nonexist"); ok {
		t.Fatalf("expected missing field to return false")
	}
	if _, ok := Get(json, ""); ok {
		t.Fatalf("expected empty expr to return false")
	}
}

func TestKeyOf(t *testing.T) {
	if got := KeyOf("body.seq"); got != "seq" {
		t.Fatalf("KeyOf(body.seq) = %q", got)
	}
	if got := KeyOf("seq"); got != "seq" {
		t.Fatalf("KeyOf(seq) = %q", got)
	}
}

func TestEquals(t *testing.T) {
	cases := []struct {
		name     string
		json     string
		expr     string
		expected any
		want     bool
	}{
		{"int vs int", `{"v":10}`, "v", 10, true},
		{"int vs string", `{"v":10}`, "v", "10", true},
		{"string vs int", `{"v":"10"}`, "v", 10, true},
		{"float matches int", `{"v":10.0}`, "v", 10, true},
		{"string equality", `{"v":"abc"}`, "v", "abc", true},
		{"bool true", `{"v":true}`, "v", true, true},
		{"bool mismatch", `{"v":true}`, "v", false, false},
		{"int mismatch", `{"v":11}`, "v", 10, false},
		{"missing field", `{}`, "v", 10, false},
		{"null", `{"v":null}`, "v", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := Raw(tc.json, tc.expr)
			got := ok && Equals(r, tc.expected)
			if got != tc.want {
				t.Fatalf("Equals(%s, %#v) = %v, want %v (raw=%v)", tc.expr, tc.expected, got, tc.want, r.String())
			}
		})
	}
}

func TestJSONOf(t *testing.T) {
	if _, err := JSONOf(&jsonOK{}); err != nil {
		t.Fatalf("JSONOf ok payload: %v", err)
	}
	if _, err := JSONOf(&jsonErr{err: "boom"}); err == nil {
		t.Fatalf("expected marshal error to surface")
	}
}

type jsonOK struct{}

func (j *jsonOK) ToJSON() ([]byte, error) { return []byte(`{}`), nil }

type jsonErr struct{ err string }

func (j *jsonErr) ToJSON() ([]byte, error) { return nil, errors.New(j.err) }
