package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClaimAccessCodeParsesConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/access/claim" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("code") != "GTA-ABC1-DEF2" {
			t.Fatalf("code not passed through: %q", r.URL.Query().Get("code"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"server":       "192.168.1.10:9091",
			"ingest_addr":  "192.168.1.10:9092",
			"token":        "gta_team",
			"session":      "s-1",
			"bpf":          "tcp port 8080 or udp port 8080",
			"plugin_names": []string{"http"},
		})
	}))
	defer srv.Close()

	hostPort := hostPortOf(t, srv.URL)
	cfg, err := claimAccessCode(context.Background(), hostPort, "GTA-ABC1-DEF2")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != "192.168.1.10:9091" {
		t.Fatalf("Server = %q", cfg.Server)
	}
	if cfg.IngestAddr != "192.168.1.10:9092" {
		t.Fatalf("IngestAddr = %q", cfg.IngestAddr)
	}
	if cfg.Token != "gta_team" || cfg.SessionID != "s-1" {
		t.Fatalf("token/session = %q/%q", cfg.Token, cfg.SessionID)
	}
	if cfg.BPF == "" || len(cfg.BindPlugins) != 1 || cfg.BindPlugins[0] != "http" {
		t.Fatalf("bpf/bind = %q/%v", cfg.BPF, cfg.BindPlugins)
	}
}

func TestClaimAccessCodeHttpError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid access code", http.StatusNotFound)
	}))
	defer srv.Close()

	hostPort := hostPortOf(t, srv.URL)
	if _, err := claimAccessCode(context.Background(), hostPort, "GTA-NOPE-0000"); err == nil {
		t.Fatal("expected error for non-200, got nil")
	}
}

func hostPortOf(t *testing.T, u string) string {
	t.Helper()
	// httptest URL 形如 http://127.0.0.1:port ，取其 host:port。
	return hostPortStripScheme(u)
}

func hostPortStripScheme(u string) string {
	for _, p := range []string{"http://", "https://"} {
		if len(u) > len(p) && u[:len(p)] == p {
			return u[len(p):]
		}
	}
	return u
}
