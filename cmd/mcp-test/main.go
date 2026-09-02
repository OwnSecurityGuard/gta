package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func main() {
	base := "http://localhost:8781"
	resp, err := http.Get(base + "/sse")
	if err != nil {
		fmt.Println("GET /sse error:", err)
		return
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	var sessionID string
	for {
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data: /message?sessionId=") {
			sessionID = strings.TrimPrefix(line, "data: /message?sessionId=")
			break
		}
	}
	fmt.Println("session:", sessionID)

	call := func(id int, name string, args map[string]any) {
		reqBody := map[string]any{
			"jsonrpc": "2.0", "id": id, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": args},
		}
		b, _ := json.Marshal(reqBody)
		url := fmt.Sprintf("%s/message?sessionId=%s", base, sessionID)
		r, err := http.Post(url, "application/json", bytes.NewReader(b))
		if err != nil {
			fmt.Printf("%s post error: %v\n", name, err)
			return
		}
		defer r.Body.Close()
		body, _ := io.ReadAll(r.Body)
		fmt.Printf("%s response: %s\n", name, string(body))
	}

	call(1, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mcp-test", "version": "1.0"},
	})
	call(2, "start_capture", map[string]any{"port": 8080, "plugin": "http", "pcap_file": "test-http.pcap"})
	call(3, "stop_capture", map[string]any{})
	call(4, "list_decoded_data", map[string]any{"limit": 20})
	call(5, "aggregate_query", map[string]any{"expression": "name == \"http_req_count\""})
	call(6, "aggregate_query", map[string]any{"expression": "name == \"http_req_rate\""})
}
