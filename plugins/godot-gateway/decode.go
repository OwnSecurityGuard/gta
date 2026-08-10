package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"sync"
)

// Event is the decoded representation of one HTTP message on the gateway link.
// The gtasdk harness turns it into one DecodeResponseV2 (non-terminal), followed
// by a terminal Done response.
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

// Reassembly buffers, keyed by directional TCP flow. The gta-pipeline's loopback
// capture path does NOT reassemble TCP streams before invoking the decoder, so
// HTTP requests/responses arrive segmented across several raw packets, and the
// payload is a complete link-layer frame (LINKTYPE_NULL + IPv4 + TCP on loopback).
// We strip the headers (see stripToTCP) and rebuild the complete HTTP messages
// per flow here. Captures that already deliver L7 are handled by decodeL7.
var (
	reasmMu   sync.Mutex
	reasmBufs = map[string]*reasmStream{}
)

type reasmStream struct {
	buf []byte
}

func flowKey(srcIP string, srcPort int, dstIP string, dstPort int) string {
	return srcIP + ":" + strconv.Itoa(srcPort) + "->" + dstIP + ":" + strconv.Itoa(dstPort)
}

// Decode turns one captured packet into zero or more gateway events. It never
// panics and never returns an error on malformed input — it yields whatever
// complete messages it can and ignores truncated/unknown bytes (contract rules:
// malformed-input-safe, one-input-may-carry-many-messages).
func Decode(pkt []byte) (events []*Event, err error) {
	events = []*Event{} // never nil, even with no decodable messages
	// Defensive: a single bad input must not crash the plugin process.
	defer func() {
		if r := recover(); r != nil {
			events = []*Event{}
		}
	}()

	// Peel the link/network/transport headers. On the loopback capture path the
	// pipeline hands the full frame (LINKTYPE_NULL + IPv4 + TCP); on an L7
	// capture it hands the HTTP bytes directly (ok=false here).
	payload, srcIP, srcPort, dstIP, dstPort, ok := stripToTCP(pkt)
	if !ok {
		return decodeL7(pkt), nil
	}
	if len(payload) == 0 {
		return events, nil // pure ACK / empty segment: nothing to decode
	}

	key := flowKey(srcIP, srcPort, dstIP, dstPort)
	reasmMu.Lock()
	defer reasmMu.Unlock()

	st, exists := reasmBufs[key]
	if !exists {
		st = &reasmStream{}
		reasmBufs[key] = st
	}
	st.buf = append(st.buf, payload...)
	// Guard against unbounded growth on a flow that never completes a message.
	if len(st.buf) > 8*1024*1024 {
		st.buf = st.buf[:0]
	}

	msgs, consumed := splitHTTPMessages(st.buf)
	for _, m := range msgs {
		if m == nil {
			continue
		}
		events = append(events, messageToEvent(m)...)
	}

	// Keep any partial trailing message for the next segment.
	if consumed > 0 {
		if consumed >= len(st.buf) {
			st.buf = st.buf[:0]
		} else {
			st.buf = append([]byte(nil), st.buf[consumed:]...)
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

// decodeL7 handles payloads the pipeline already delivered as L7 (HTTP or bare JSON). Used when
// the pipeline delivers stripped payloads rather than full frames.
func decodeL7(pkt []byte) []*Event {
	events := []*Event{}
	l7 := extractL7(pkt)
	msgs, _ := splitHTTPMessages(l7)
	for _, m := range msgs {
		if m == nil {
			continue
		}
		events = append(events, messageToEvent(m)...)
	}
	if len(msgs) == 0 {
		if e := decodeBareJSON(pkt); e != nil {
			events = append(events, e)
		}
	}
	return events
}

// extractL7 is a pass-through for the L7-delivered fallback. When stripToTCP returns
// ok=false the pipeline has already stripped link headers, so nothing to do.
func extractL7(pkt []byte) []byte {
	return pkt
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
		EventType: "godot-gateway.request",
		SchemaID:  "godot-gateway.request.v1",
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
		EventType: "godot-gateway.response",
		SchemaID:  "godot-gateway.response.v1",
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
//
// recognized is false when the body does not look like any gateway response, so
// the caller can leave non-gateway traffic for the generic http plugin.
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
	// Top-level keys look like character ids, each {name, level, skin}.
	if isCharList(body) {
		fields["characters"] = decodeCharacters(body)
		return "world_characters", true, fields, true
	}
	return "unknown", false, fields, false
}

// decodeBareJSON handles the case where the pipeline hands only the JSON body
// (no HTTP framing). It only claims traffic that clearly looks like the gateway.
func decodeBareJSON(pkt []byte) *Event {
	trimmed := bytes.TrimSpace(pkt)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	body := parseBody(trimmed)
	if len(body) == 0 {
		return nil
	}

	// Request-shaped: carries client-sent short keys.
	if hasAnyKey(body, keyAccountUser, keyAccountPass, keyTokenID, keyWorldID, keyCharID, keyClientVersion) {
		_, fields := decodeRequestBody("/v1/unknown", body)
		if fields == nil {
			fields = map[string]any{}
		}
		fields["endpoint"] = "unknown"
		fields["method"] = "POST"
		fields["path"] = "<bare-json-body>"
		return &Event{
			EventType: "godot-gateway.request",
			SchemaID:  "godot-gateway.request.v1",
			Payload:   fields,
			Meta: map[string]any{
				"direction": "client_to_server",
				"msg_name":  "unknown",
				"is_push":   false,
			},
		}
	}

	// Response-shaped.
	endpoint, inferred, fields, recognized := decodeResponseBody(body)
	if !recognized {
		return nil
	}
	return buildResponseEvent(endpoint, inferred, fields, 0)
}

func hasAnyKey(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
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

func decodeWorlds(w any) []map[string]any {
	out := []map[string]any{}
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

func decodeCharacters(body map[string]any) []map[string]any {
	out := []map[string]any{}
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

// stripToTCP peels LINKTYPE_NULL loopback + IPv4/IPv6 + TCP headers off a
// captured frame and returns the TCP payload plus the 4-tuple (so the caller can
// reassemble per flow). Returns ok=false when pkt is not a recognized IPv4/IPv6
// TCP frame (e.g., an already-L7 payload, or non-TCP traffic).
func stripToTCP(pkt []byte) (payload []byte, srcIP string, srcPort int, dstIP string, dstPort int, ok bool) {
	b := pkt

	// LINKTYPE_NULL (BSD loopback): 4-byte address-family header. Values are
	// host-endian in practice; check both endiannesses for AF_INET/AF_INET6.
	if len(b) >= 4 {
		famLE := binary.LittleEndian.Uint32(b[:4])
		famBE := binary.BigEndian.Uint32(b[:4])
		if famLE == 2 || famBE == 2 || famLE == 30 || famBE == 30 || famLE == 23 || famBE == 23 || famLE == 24 || famBE == 24 {
			b = b[4:]
		}
	}

	// IPv4
	if len(b) >= 20 && (b[0]&0xf0) == 0x40 {
		ihl := int(b[0]&0x0f) * 4
		if ihl < 20 || ihl > len(b) {
			return nil, "", 0, "", 0, false
		}
		if b[9] != 6 { // TCP only
			return nil, "", 0, "", 0, false
		}
		srcIP = net.IPv4(b[12], b[13], b[14], b[15]).String()
		dstIP = net.IPv4(b[16], b[17], b[18], b[19]).String()
		p, sport, dport, ok := stripTCP(b[ihl:])
		if !ok {
			return nil, "", 0, "", 0, false
		}
		return p, srcIP, sport, dstIP, dport, true
	}

	// IPv6
	if len(b) >= 40 && (b[0]&0xf0) == 0x60 {
		if b[6] != 6 { // TCP only
			return nil, "", 0, "", 0, false
		}
		srcIP = net.IP(b[8:24]).String()
		dstIP = net.IP(b[24:40]).String()
		p, sport, dport, ok := stripTCP(b[40:])
		if !ok {
			return nil, "", 0, "", 0, false
		}
		return p, srcIP, sport, dstIP, dport, true
	}

	return nil, "", 0, "", 0, false
}

// stripTCP returns the payload of a TCP segment together with its ports.
func stripTCP(l4 []byte) (payload []byte, srcPort, dstPort int, ok bool) {
	if len(l4) < 20 {
		return nil, 0, 0, false
	}
	dataOff := int(l4[12]>>4) * 4
	if dataOff < 20 || dataOff > len(l4) {
		return nil, 0, 0, false
	}
	srcPort = int(binary.BigEndian.Uint16(l4[0:2]))
	dstPort = int(binary.BigEndian.Uint16(l4[2:4]))
	return l4[dataOff:], srcPort, dstPort, true
}
