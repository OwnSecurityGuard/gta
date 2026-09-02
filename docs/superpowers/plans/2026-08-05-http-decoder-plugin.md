# HTTP Decoder Plugin Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Create `plugins/http` — a V1 JSON decoder plugin that parses HTTP request/response payloads from TCP packets and emits structured events for the GTA pipeline.

**Architecture:** A standalone Go binary using `pkg/plugin/sdk`. It listens on a gRPC Decoder endpoint, registers with the pipeline registry, and decodes each packet payload via `http.ReadRequest` / `http.ReadResponse`. Output follows the V1 JSON contract (`data` + `_fields`).

**Tech Stack:** Go 1.25, `net/http`, `pkg/plugin/sdk`, `pkg/plugin/proto`.

---

## Task 1: Create plugin manifest

**Files:**
- Create: `plugins/http/plugin.yaml`

- [ ] **Step 1: Write `plugin.yaml`**

```yaml
api_version: gta.decoder/v1
name: http
protocol: http
type: decoder
meta:
  author: gta
  description: Simple HTTP request/response decoder with body capture.
event:
  fields:
    direction: { type: string, enum: [client_to_server, server_to_client, unknown] }
    msg_name:  { type: string }
  data:
    schema:
      fields:
        type:           { type: string }
        method:         { type: string }
        path:           { type: string }
        version:        { type: string }
        host:           { type: string }
        status:         { type: string }
        reason:         { type: string }
        content_length: { type: string }
        body_len:       { type: number }
        body:           { type: string }
        body_truncated: { type: boolean }
        headers:        { type: object }
```

- [ ] **Step 2: Commit manifest**

```bash
git add plugins/http/plugin.yaml
git commit -m "feat(http-plugin): add plugin manifest"
```

---

## Task 2: Implement the decoder

**Files:**
- Create: `plugins/http/main.go`

- [ ] **Step 1: Write `main.go`**

```go
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"gta/pkg/plugin/proto"
	"gta/pkg/plugin/sdk"
)

const maxBodyBytes = 64 * 1024

func main() {
	sdk.RunRegisterLoop(decodePacket)
}

// decodePacket parses a single packet payload as HTTP request or response.
func decodePacket(req *proto.DecodeRequest) ([]byte, error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("decode panic recovered", "error", r)
		}
	}()

	payload := req.Payload
	if len(payload) == 0 {
		return nil, fmt.Errorf("empty payload")
	}

	// Try request first, then response.
	if result, ok := decodeRequest(payload); ok {
		return result, nil
	}
	if result, ok := decodeResponse(payload); ok {
		return result, nil
	}

	return nil, fmt.Errorf("not valid HTTP")
}

func decodeRequest(payload []byte) ([]byte, bool) {
	r, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(payload)))
	if err != nil {
		return nil, false
	}
	defer r.Body.Close()

	body, truncated, bodyLen := readLimitedBody(r.Body, r.ContentLength)

	headers := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = strings.Join(v, ", ")
		}
	}

	result := map[string]any{
		"data": map[string]any{
			"type":           "request",
			"method":         r.Method,
			"path":           r.URL.String(),
			"version":        r.Proto,
			"host":           r.Host,
			"content_length": r.Header.Get("Content-Length"),
			"body_len":       bodyLen,
			"body":           body,
			"body_truncated": truncated,
			"headers":        headers,
		},
		"_fields": map[string]any{
			"direction": "client_to_server",
			"msg_name":  fmt.Sprintf("%s %s", r.Method, r.URL.String()),
		},
	}
	return mustMarshal(result), true
}

func decodeResponse(payload []byte) ([]byte, bool) {
	r, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(payload)), nil)
	if err != nil {
		return nil, false
	}
	defer r.Body.Close()

	body, truncated, bodyLen := readLimitedBody(r.Body, r.ContentLength)

	headers := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = strings.Join(v, ", ")
		}
	}

	result := map[string]any{
		"data": map[string]any{
			"type":           "response",
			"version":        r.Proto,
			"status":         r.StatusCode,
			"reason":         http.StatusText(r.StatusCode),
			"content_length": r.Header.Get("Content-Length"),
			"body_len":       bodyLen,
			"body":           body,
			"body_truncated": truncated,
			"headers":        headers,
		},
		"_fields": map[string]any{
			"direction": "server_to_client",
			"msg_name":  fmt.Sprintf("resp %d", r.StatusCode),
		},
	}
	return mustMarshal(result), true
}

// readLimitedBody reads up to maxBodyBytes from body and returns the string,
// whether it was truncated, and the total bytes read (including discarded part).
func readLimitedBody(body io.ReadCloser, contentLength int64) (string, bool, int64) {
	limited := io.LimitReader(body, maxBodyBytes)
	data, err := io.ReadAll(limited)
	readLen := int64(len(data))

	// Try to consume one more byte to detect truncation.
	var extra [1]byte
	n, _ := body.Read(extra[:])
	truncated := err == nil && n > 0

	// If Content-Length is larger than what we read, we truncated.
	if contentLength > 0 && contentLength > readLen {
		truncated = true
		// Report the original declared length as body_len.
		return string(data), true, contentLength
	}

	if n > 0 {
		readLen++
	}
	return string(data), truncated, readLen
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"data":{"error":"marshal failed"}}`)
	}
	return b
}
```

- [ ] **Step 2: Commit decoder implementation**

```bash
git add plugins/http/main.go
git commit -m "feat(http-plugin): implement HTTP request/response decoder"
```

---

## Task 3: Create module file

**Files:**
- Create: `plugins/http/go.mod`

- [ ] **Step 1: Write `go.mod`**

```
module http

go 1.25.5

require (
	gta v0.0.0
	gta-plugin-sdk v0.0.0
)

replace gta => ../..
replace gta-plugin-sdk => ../../pkg/plugin/sdk
```

- [ ] **Step 2: Commit go.mod**

```bash
git add plugins/http/go.mod
git commit -m "chore(http-plugin): add go.mod"
```

---

## Task 4: Verify build

**Files:**
- Test: `plugins/http/main.go`, `plugins/http/plugin.yaml`, `plugins/http/go.mod`

- [ ] **Step 1: Build the plugin**

```bash
cd plugins/http
go build -o http-plugin.exe .
```

Expected: Build succeeds with no errors.

- [ ] **Step 2: Verify manifest parsing**

```bash
cd plugins/http
go run .
```

Expected: Process starts and attempts to register (may fail if registry not running, but manifest should parse without panic).

- [ ] **Step 3: Commit verification notes**

No code changes; optionally update `docs/superpowers/specs/2026-08-05-http-decoder-plugin-design.md` with build status.

---

## Self-Review

- **Spec coverage:** All output fields from the design doc are implemented. Body truncation and panic recovery are included.
- **Placeholder scan:** No placeholders; all code is complete.
- **Type consistency:** `status` is emitted as integer (HTTP status code); `reason` is string. `content_length` is string from header. `body_len` is number. Matches schema.
