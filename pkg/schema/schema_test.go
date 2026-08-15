package schema

import (
	"testing"

	sdkschema "github.com/OwnSecurityGuard/gta-plugin-sdk/schema"
)

func TestSchemaLookup(t *testing.T) {
	s := &Schema{Fields: map[string]*Field{
		"payload": {Type: sdkschema.TypeObject, Fields: map[string]*Field{
			"damage": {Type: sdkschema.TypeFloat64},
		}},
	}}
	f, err := Lookup(s, "payload.damage")
	if err != nil {
		t.Fatal(err)
	}
	if f.Type != sdkschema.TypeFloat64 {
		t.Fatalf("expected float64, got %s", f.Type)
	}
	_, err = Lookup(s, "payload.missing")
	if err == nil {
		t.Fatal("expected error")
	}
}
