package schema

import "testing"

func TestSchemaLookup(t *testing.T) {
	s := &Schema{Fields: map[string]*Field{
		"payload": {Type: "object", Fields: map[string]*Field{
			"damage": {Type: "number"},
		}},
	}}
	f, err := s.Lookup("payload.damage")
	if err != nil {
		t.Fatal(err)
	}
	if f.Type != "number" {
		t.Fatalf("expected number, got %s", f.Type)
	}
	_, err = s.Lookup("payload.missing")
	if err == nil {
		t.Fatal("expected error")
	}
}
