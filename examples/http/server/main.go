package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
)

func main() {
	addr := flag.String("addr", ":8984", "listen address")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	var count atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		slog.Info("received request", "n", n, "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(w, "pong %d\n", n)
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		slog.Info("echo request", "method", r.Method, "body_len", r.ContentLength)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(w, "ok\n")
	})

	slog.Info("http server listening", "addr", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}
