package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/OwnSecurityGuard/gta-plugin-sdk/framing"
	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
)

// ra reassembles the per-flow TCP byte stream across captured segments. One
// instance is shared by the whole plugin process (goroutine-safe), keyed by
// FlowKey with one directional buffer per stream.
var ra = framing.NewReassembler()

// Event is the decoded representation of one HTTP message on the gateway link.
type Event struct {
	EventType        string
	SchemaID         string
	Payload          map[string]any // business fields (root of payload_msgpack)
	Meta             map[string]any // reserved "_meta" object
	CorrelationKey   string
	CausationInputID string
}

// httpMsg is one parsed HTTP message (request or response).
type httpMsg struct {
	isRequest  bool
	method     string
	path       string
	statusCode int
	statusText string
	headers    map[string]string
	body       []byte
}

// Decode turns one captured frame into zero or more gateway events. It never
// panics and never returns an error on malformed input — it yields whatever
// complete messages it can and ignores truncated/unknown bytes (contract rules:
// malformed-input-safe, one-input-may-carry-many-messages).
func Decode(req *pb.DecodeRequest) (events []*Event, err error) {
	events = []*Event{} // never nil, even with no decodable messages
	defer func() {
		if r := recover(); r != nil {
			events = []*Event{}
		}
	}()

	// Strip the encapsulation selected by link_type. Loopback frames (Null=0,
	// Loop=108) carry a 4-byte AF_* header; Ethernet 14 bytes; RawIP none.
	seg, ok := framing.ExtractL7(req.GetPayload(), req.GetLinkType())
	if !ok || len(seg.Payload) == 0 {
		// Non-IP traffic, truncated frame, or a pure ACK / handshake / FIN.
		return events, nil
	}

	s := ra.Push(seg)
	for {
		raw := s.Bytes()
		if len(raw) == 0 {
			break // not enough contiguous bytes yet (gap, or all consumed)
		}
		msgs, consumed := splitHTTPMessages(raw)
		if consumed == 0 {
			break // incomplete message: wait for the next segment
		}
		s.Consume(consumed)
		for _, m := range msgs {
			if m == nil {
				continue
			}
			events = append(events, messageToEvent(m)...)
		}
		if consumed < len(raw) && len(msgs) == 0 {
			break
		}
	}
	return events, nil
}

// messageToEvent converts a fully parsed HTTP message into zero or more events.
// Unknown gateway endpoints and unrecognized responses yield no events so the
// traffic can be left for the generic http plugin.
func messageToEvent(m *httpMsg) []*Event {
	if m.isRequest {
		if endpointFromPath(m.path) == "unknown" {
			return nil
		}
		return []*Event{decodeRequestMsg(m)}
	}
	endpoint, inferred, fields, recognized := decodeResponseBody(parseBody(m.body))
	if !recognized {
		return nil
	}
	return []*Event{buildResponseEvent(endpoint, inferred, fields, m.statusCode)}
}

// splitHTTPMessages extracts every complete HTTP message from the stream and
// reports how many leading bytes were consumed by those complete messages (so
// the caller can trim the reassembly buffer). Trailing bytes that do not yet
// form a complete message (Content-Length larger than available body) are left
// for the next segment.
func splitHTTPMessages(data []byte) (out []*httpMsg, consumed int) {
	for len(data) > 0 {
		m, rest, ok := nextMessage(data)
		if !ok {
			break
		}
		n := len(data) - len(rest)
		if m != nil {
			out = append(out, m)
		}
		consumed += n
		if rest == nil {
			break
		}
		data = rest
	}
	return out, consumed
}

func nextMessage(data []byte) (m *httpMsg, rest []byte, ok bool) {
	sep := 4
	hi := bytes.Index(data, []byte("\r\n\r\n"))
	if hi < 0 {
		hi = bytes.Index(data, []byte("\n\n"))
		if hi < 0 {
			return nil, nil, false
		}
		sep = 2
	}
	headerBlock := data[:hi]
	bodyAll := data[hi+sep:]

	m = &httpMsg{headers: map[string]string{}}
	lines := bytes.Split(headerBlock, []byte("\r\n"))
	if len(lines) == 1 {
		lines = bytes.Split(headerBlock, []byte("\n"))
	}
	if len(lines) == 0 || len(lines[0]) == 0 {
		return nil, nil, false
	}
	first := strings.Fields(string(lines[0]))
	if len(first) >= 3 && strings.HasPrefix(first[0], "HTTP/") {
		m.isRequest = false
		if c, e := strconv.Atoi(first[1]); e == nil {
			m.statusCode = c
		}
		m.statusText = strings.Join(first[2:], " ")
	} else if len(first) >= 2 {
		m.isRequest = true
		m.method = first[0]
		m.path = first[1]
	} else {
		return nil, nil, false
	}

	for _, ln := range lines[1:] {
		if len(ln) == 0 {
			continue
		}
		kv := bytes.SplitN(ln, []byte(":"), 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(string(kv[0])))
		val := strings.TrimSpace(string(kv[1]))
		m.headers[key] = val
	}

	clStr, hasCL := m.headers["content-length"]
	cl := 0
	if hasCL {
		cl, _ = strconv.Atoi(clStr)
	}
	if hasCL {
		if len(bodyAll) < cl {
			// Incomplete message: not enough body yet. Wait for more data.
			return nil, nil, false
		}
		m.body = bodyAll[:cl]
		rest = bodyAll[cl:]
		return m, rest, true
	}
	// No Content-Length: treat the remainder as the single message body.
	m.body = bodyAll
	return m, nil, true
}

func parseBody(b []byte) map[string]any {
	if len(bytes.TrimSpace(b)) == 0 {
		return map[string]any{}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return map[string]any{}
	}
	m, ok := normalize(raw).(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return m
}

// normalize walks a decoded JSON value and converts json.Number to int64/uint64/
// float64 so the msgpack encoder keeps proper types (no stringly-typed numbers).
func normalize(v any) any {
	switch x := v.(type) {
	case json.Number:
		return jsonNumber(x)
	case []any:
		for i, e := range x {
			x[i] = normalize(e)
		}
		return x
	case map[string]any:
		for k, e := range x {
			x[k] = normalize(e)
		}
		return x
	default:
		return v
	}
}

func jsonNumber(n json.Number) any {
	s := n.String()
	if strings.ContainsAny(s, ".eE") {
		if f, err := n.Float64(); err == nil {
			return f
		}
		return s
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	if u, err := strconv.ParseUint(s, 10, 64); err == nil {
		return u
	}
	return s
}

func decodeRequestMsg(m *httpMsg) *Event {
	body := parseBody(m.body)
	endpoint, fields := decodeRequestBody(m.path, body)

	payload := fields
	if payload == nil {
		payload = map[string]any{}
	}
	payload["endpoint"] = endpoint
	payload["method"] = m.method
	payload["path"] = m.path

	return &Event{
		EventType: "godot_gateway.request",
		SchemaID:  "godot_gateway.request.v1",
		Payload:   payload,
		Meta: map[string]any{
			"direction": "client_to_server",
			"msg_name":  endpoint,
			"is_push":   false,
		},
	}
}

// decodeRequestBody expands the wire-short keys into readable fields.
func decodeRequestBody(path string, body map[string]any) (string, map[string]any) {
	endpoint := endpointFromPath(path)
	fields := map[string]any{}

	switch endpoint {
	case "handshake":
		copyField(fields, body, keyClientVersion, "client_version")
	case "login":
		copyField(fields, body, keyAccountUser, "account_username")
		copyField(fields, body, keyAccountPass, "account_password")
		copyField(fields, body, keyClientVersion, "client_version")
	case "guest":
		// no body fields
	case "account_create":
		copyField(fields, body, keyAccountUser, "account_username")
		copyField(fields, body, keyAccountPass, "account_password")
	case "worlds":
		// empty body
	case "world_characters":
		copyField(fields, body, keyWorldID, "world_id")
		copyField(fields, body, keyAccountID, "account_id")
		copyField(fields, body, keyAccountUser, "account_username")
		copyField(fields, body, keyTokenID, "token")
	case "world_enter":
		copyField(fields, body, keyTokenID, "token")
		copyField(fields, body, keyAccountUser, "account_username")
		copyField(fields, body, keyWorldID, "world_id")
		copyField(fields, body, keyCharID, "character_id")
	case "character_create":
		copyField(fields, body, keyTokenID, "token")
		copyField(fields, body, keyAccountUser, "account_username")
		copyField(fields, body, keyWorldID, "world_id")
		if d, ok := body["data"].(map[string]any); ok {
			copyField(fields, d, "name", "character_name")
			copyField(fields, d, "skin", "skin")
		}
	default:
		// unknown endpoint: still report raw body for inspection
		for k, v := range body {
			fields[k] = v
		}
	}
	return endpoint, fields
}

func buildResponseEvent(endpoint string, inferred bool, fields map[string]any, statusCode int) *Event {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["endpoint"] = endpoint
	fields["endpoint_inferred"] = inferred
	fields["status_code"] = statusCode

	return &Event{
		EventType: "godot_gateway.response",
		SchemaID:  "godot_gateway.response.v1",
		Payload:   fields,
		Meta: map[string]any{
			"direction": "server_to_client",
			"msg_name":  endpoint,
			"is_push":   false,
		},
	}
}

// decodeResponseBody best-effort maps a gateway response body to an endpoint.
// Responses carry no URL, so the endpoint is inferred from the body shape and
// flagged via endpoint_inferred. (Client→gateway requests are the primary target
// per the task; responses are decoded opportunistically.)
func decodeResponseBody(body map[string]any) (endpoint string, inferred bool, fields map[string]any, recognized bool) {
	fields = map[string]any{}
	if len(body) == 0 {
		return "unknown", false, fields, false
	}
	if errv, ok := body["error"]; ok {
		fields["error"] = errv
		fields["error_name"] = errorName(errv)
		if msg, ok := body["msg"]; ok {
			fields["error_message"] = msg
		}
		return "error", false, fields, true
	}
	if ok, _ := body["ok"].(bool); ok {
		fields["ok"] = true
		return "handshake", false, fields, true
	}
	if _, hasAddr := body["address"]; hasAddr {
		copyField(fields, body, "address", "world_address")
		copyField(fields, body, "port", "world_port")
		copyField(fields, body, "auth-token", "auth_token")
		return "world_enter_or_character_create", true, fields, true
	}
	if sid, ok := body["session_id"]; ok {
		fields["session_id"] = sid
		copyField(fields, body, "name", "account_name")
		copyField(fields, body, "id", "account_id")
		if w, ok := body["w"]; ok {
			fields["worlds"] = decodeWorlds(w)
		}
		return "login_or_guest", true, fields, true
	}
	if w, ok := body["w"]; ok {
		if _, hasName := body["name"]; hasName {
			copyField(fields, body, "name", "account_name")
			copyField(fields, body, "id", "account_id")
			fields["worlds"] = decodeWorlds(w)
			return "account_create", true, fields, true
		}
		fields["worlds"] = decodeWorlds(w)
		return "worlds", false, fields, true
	}
	if isCharList(body) {
		fields["characters"] = decodeCharacters(body)
		return "world_characters", true, fields, true
	}
	return "unknown", false, fields, false
}

// isCharList reports whether every top-level value looks like a character entry
// ({name, level, ...}). Used to recognize a world_characters response.
func isCharList(body map[string]any) bool {
	if len(body) == 0 {
		return false
	}
	for _, v := range body {
		c, ok := v.(map[string]any)
		if !ok {
			return false
		}
		if _, hasName := c["name"]; !hasName {
			return false
		}
	}
	return true
}

// decodeWorlds / decodeCharacters return []any of map[string]any so the SDK's
// event.ValueFromAny encodes them as msgpack arrays of objects (a bare
// []map[string]any has no ValueFromAny case and would degrade to a string).
func decodeWorlds(w any) []any {
	out := []any{}
	m, ok := w.(map[string]any)
	if !ok {
		return out
	}
	for id, val := range m {
		entry := map[string]any{"world_id": id}
		if info, ok := val.(map[string]any); ok {
			if inf, ok := info["info"].(map[string]any); ok {
				copyField(entry, inf, "name", "name")
				copyField(entry, inf, "motd", "motd")
				copyField(entry, inf, "pvp", "pvp")
			}
		}
		out = append(out, entry)
	}
	return out
}

func decodeCharacters(body map[string]any) []any {
	out := []any{}
	for id, val := range body {
		entry := map[string]any{"character_id": id}
		if c, ok := val.(map[string]any); ok {
			copyField(entry, c, "name", "name")
			copyField(entry, c, "level", "level")
			copyField(entry, c, "skin", "skin")
		}
		out = append(out, entry)
	}
	return out
}
