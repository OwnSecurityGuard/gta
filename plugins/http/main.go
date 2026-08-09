package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"

	"github.com/OwnSecurityGuard/gta-plugin-sdk"
	"github.com/OwnSecurityGuard/gta-plugin-sdk/event"
	"github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
)

const maxBodyBytes = 64 * 1024

func main() {
	sdk.RunRegisterLoop(decodePacketV2)
}

// extractTCPPayload strips link-layer and IP/TCP headers and returns the TCP payload.
// For custom LinkType values where the payload is already application-layer,
// returns the raw bytes unchanged.
func extractTCPPayload(raw []byte, linkType int32) ([]byte, bool) {
	lt := event.LinkType(linkType)
	switch lt {
	case event.LinkTypeProxyPayload, event.LinkTypeTLSPlaintext:
		return raw, true
	}

	pkt := gopacket.NewPacket(raw, layers.LinkType(linkType), gopacket.Default)
	if pkt == nil {
		return nil, false
	}
	tcpLayer := pkt.Layer(layers.LayerTypeTCP)
	if tcpLayer == nil {
		return nil, false
	}
	return tcpLayer.LayerPayload(), true
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
			"body":           parseBodyIfJSON(body),
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
			"body":           parseBodyIfJSON(body),
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

// parseBodyIfJSON 若 body 是合法的 JSON 对象/数组，则解析为结构化值返回；
// 否则原样返回字符串。目的是避免把整个 JSON 文档当作字符串存储，
// 否则下游序列化时内部引号会被转义成 \"，既难读也不可用字段路径查询。
// 非 JSON 文本（如纯文本、标量）保持字符串不变。
func parseBodyIfJSON(body string) any {
	t := strings.TrimSpace(body)
	if len(t) == 0 || (t[0] != '{' && t[0] != '[') {
		return body
	}
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return body
	}
	switch v.(type) {
	case map[string]any, []any:
		return v
	default:
		return body
	}
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"data":{"error":"marshal failed"}}`)
	}
	return b
}

// decodePacketV2 implements the DecodeV2 RPC with multiple results and done marker.
func decodePacketV2(req *proto.DecodeRequest, stream proto.Decoder_DecodeV2Server) error {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("decode panic recovered", "error", r)
		}
	}()

	payload := req.Payload
	if len(payload) == 0 {
		return stream.Send(&proto.DecodeResponseV2{InputId: req.InputId, Done: true})
	}

	tcpPayload, ok := extractTCPPayload(payload, req.LinkType)
	if !ok || len(tcpPayload) == 0 {
		return stream.Send(&proto.DecodeResponseV2{InputId: req.InputId, Done: true})
	}

	sent := false
	if result, ok := decodeRequest(tcpPayload); ok {
		msgpackBytes, err := jsonToMsgpack(result)
		if err == nil {
			if err := stream.Send(&proto.DecodeResponseV2{
				InputId:        req.InputId,
				EventType:      "http.request",
				SchemaId:       "http.request.v1",
				PayloadMsgpack: msgpackBytes,
			}); err != nil {
				return err
			}
			sent = true
		}
	}

	if result, ok := decodeResponse(tcpPayload); ok {
		msgpackBytes, err := jsonToMsgpack(result)
		if err == nil {
			if err := stream.Send(&proto.DecodeResponseV2{
				InputId:        req.InputId,
				EventType:      "http.response",
				SchemaId:       "http.response.v1",
				PayloadMsgpack: msgpackBytes,
			}); err != nil {
				return err
			}
			sent = true
		}
	}

	if !sent {
		_ = stream.Send(&proto.DecodeResponseV2{InputId: req.InputId, Error: "not valid HTTP"})
	}

	return stream.Send(&proto.DecodeResponseV2{InputId: req.InputId, Done: true})
}

// jsonToMsgpack converts the JSON output of decodeRequest/decodeResponse to MsgPack.
func jsonToMsgpack(jsonBytes []byte) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(jsonBytes, &obj); err != nil {
		return nil, err
	}
	v := event.ValueFromAny(obj)
	return v.MarshalMsgpack()
}
