package schema

import "testing"

func TestRegistryLookup(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(&SchemaDecl{ID: "http.request.v1", Version: 1})
	decl, ok := r.Lookup("http.request.v1")
	if !ok {
		t.Fatal("expected schema registered")
	}
	if decl.ID != "http.request.v1" {
		t.Fatalf("expected http.request.v1 got %s", decl.ID)
	}
}

func TestRegistryLookupMissing(t *testing.T) {
	r := NewRegistry()
	_, ok := r.Lookup("nonexistent")
	if ok {
		t.Fatal("expected not found")
	}
}

func TestRegistryRegisterNil(t *testing.T) {
	r := NewRegistry()
	err := r.Register(nil)
	if err == nil {
		t.Fatal("expected error for nil decl")
	}
}

func TestRegistryRegisterEmptyID(t *testing.T) {
	r := NewRegistry()
	err := r.Register(&SchemaDecl{ID: ""})
	if err == nil {
		t.Fatal("expected error for empty id")
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(&SchemaDecl{ID: "test.v1", Version: 1})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			r.Lookup("test.v1")
		}
	}()

	for i := 0; i < 100; i++ {
		decl, ok := r.Lookup("test.v1")
		if !ok || decl.ID != "test.v1" {
			t.Errorf("concurrent read failed: ok=%v id=%s", ok, decl.ID)
		}
	}
	<-done
}
