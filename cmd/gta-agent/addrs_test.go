package main

import "testing"

func TestDeriveAddrs(t *testing.T) {
	cases := []struct {
		name             string
		server, reg, ing string
		wantReg, wantIng string
		wantErr          bool
	}{
		{"host only", "pipe.example.com", "", "", "pipe.example.com:9091", "pipe.example.com:9092", false},
		{"host with port", "pipe.example.com:9091", "", "", "pipe.example.com:9091", "pipe.example.com:9092", false},
		{"host with custom port", "pipe.example.com:9100", "", "", "pipe.example.com:9100", "pipe.example.com:9101", false},
		{"overrides win", "pipe.example.com", "r:1", "i:2", "r:1", "i:2", false},
		{"no input errors", "", "", "", "", "", true},
		{"bad port", "host:abc", "", "", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg, ing, err := deriveAddrs(tc.server, tc.reg, tc.ing)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q / %q", reg, ing)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if reg != tc.wantReg || ing != tc.wantIng {
				t.Fatalf("got (%q, %q), want (%q, %q)", reg, ing, tc.wantReg, tc.wantIng)
			}
		})
	}
}
