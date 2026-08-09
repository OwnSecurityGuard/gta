package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	url := flag.String("url", "http://127.0.0.1:8984/ping", "target url")
	interval := flag.Duration("interval", 5*time.Second, "request interval")
	//post := flag.Bool("post", false, "send POST instead of GET")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	client := &http.Client{Timeout: 5 * time.Second}
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	data := map[string]any{}
	for i := 1; ; i++ {
		var req *http.Request
		var err error
		data["resr"] = 23
		data["cds"] = map[string]string{"id": strconv.Itoa(232 + i), "sbe": "www"}
		jd, _ := json.Marshal(data)
		req, err = http.NewRequest(http.MethodPost, *url, strings.NewReader(string(jd)))
		req.Header.Set("Content-Type", "application/json")

		if err != nil {
			slog.Error("build request", "error", err)
			continue
		}
		fmt.Println(string(jd))
		resp, err := client.Do(req)
		if err != nil {
			slog.Error("request failed", "n", i, "error", err)
		} else {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			slog.Info("request ok", "n", i, "status", resp.StatusCode, "body", strings.TrimSpace(string(body)))
		}

		<-tick.C
	}
}
