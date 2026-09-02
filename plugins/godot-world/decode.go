package main

// decode.go — entry point: strip encapsulation, reassemble the TCP stream,
// skip the HTTP upgrade, parse WebSocket frames, then decode each Godot
// SceneMultiplayer packet into semantic game events.

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"github.com/OwnSecurityGuard/gta-plugin-sdk/framing"
	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
)

const worldPort = 8087

var ra = framing.NewReassembler()

// Event is one decoded message on the world link.
type Event struct {
	EventType        string
	SchemaID         string
	Payload          map[string]any
	Meta             map[string]any
	CorrelationKey   string
	CausationInputID string
}

// flowState is the per-direction decode context for one TCP stream.
type flowState struct {
	isServer      bool // server→client
	handshakeDone bool
	firstPacket   bool
	peerID        uint32
	psc           map[uint32]string // psc_id → path (this direction)
	pending       []byte            // ws continuation fragments
	pathReg       *pathRegistry     // shared per conversation
	regKey        string
}

var flowStates = struct {
	sync.Mutex
	m map[framing.FlowKey]*flowState
}{m: map[framing.FlowKey]*flowState{}}

var regs = struct {
	sync.Mutex
	m map[string]*pathRegistry
}{m: map[string]*pathRegistry{}}

func getReg(key string) *pathRegistry {
	regs.Lock()
	defer regs.Unlock()
	r, ok := regs.m[key]
	if !ok {
		r = newPathRegistry()
		regs.m[key] = r
	}
	return r
}

func getFlow(key framing.FlowKey, isServer bool, regKey string) *flowState {
	flowStates.Lock()
	defer flowStates.Unlock()
	fs, ok := flowStates.m[key]
	if !ok {
		fs = &flowState{
			isServer:    isServer,
			firstPacket: true,
			psc:         map[uint32]string{},
			pathReg:     getReg(regKey),
			regKey:      regKey,
		}
		flowStates.m[key] = fs
	}
	return fs
}

func resetFlow(key framing.FlowKey) {
	flowStates.Lock()
	defer flowStates.Unlock()
	delete(flowStates.m, key)
}

// Decode turns one captured frame into zero or more world events. Malformed
// input never panics and never aborts the run — it yields what it can.
func Decode(req *pb.DecodeRequest) (events []*Event, err error) {
	events = []*Event{}
	defer func() {
		if r := recover(); r != nil {
			events = []*Event{}
		}
	}()

	seg, ok := framing.ExtractL7(req.GetPayload(), req.GetLinkType())
	if !ok || len(seg.Payload) == 0 {
		return events, nil
	}
	if !seg.IsTCP {
		return events, nil
	}
	if seg.Flags.SYN {
		resetFlow(seg.Flow)
	}

	// The server's source port is the world port; client uses an ephemeral one.
	// isServer marks the flow carrying server→client traffic.
	isServer := seg.Flow.Src.Port() == worldPort
	regKey := seg.Flow.Canonical()
	fs := getFlow(seg.Flow, isServer, regKey)

	s := ra.Push(seg)
	for {
		raw := s.Bytes()
		if len(raw) == 0 {
			break
		}
		if !fs.handshakeDone {
			if looksLikeHTTP(raw) {
				n := indexHTTPHeader(raw)
				if n < 0 {
					break // wait for the rest of the upgrade handshake
				}
				events = append(events, handshakeEvent(fs, raw[:n]))
				s.Consume(n)
				fs.handshakeDone = true
				continue
			}
			// No HTTP upgrade in the buffer: the capture began after the
			// WebSocket handshake already completed (a live capture almost
			// always starts here) or this is a raw ws stream. The connection
			// is past the upgrade, so go straight to frame parsing.
			fs.handshakeDone = true
		}
		frames, consumed := parseWSFrames(raw)
		if consumed == 0 {
			// Either an incomplete trailing frame (normal — wait for more
			// bytes) or garbage left over from a mid-connection capture that
			// started inside a frame. Resync to the next binary-frame header
			// so the decoder doesn't stall forever on the bad prefix.
			if idx := wsResync(raw); idx > 0 {
				s.Consume(idx)
				continue
			}
			break
		}
		s.Consume(consumed)
		for _, f := range frames {
			if f.opcode == 2 { // binary
				if !f.fin {
					fs.pending = append(fs.pending, f.payload...)
					continue
				}
				var pkt []byte
				if len(fs.pending) > 0 {
					pkt = append(fs.pending, f.payload...)
					fs.pending = nil
				} else {
					pkt = f.payload
				}
				events = append(events, processPacket(fs, pkt)...)
			}
		}
	}
	return events, nil
}

func handshakeEvent(fs *flowState, hdr []byte) *Event {
	line := ""
	for i := 0; i < len(hdr) && hdr[i] != '\r' && hdr[i] != '\n'; i++ {
		line += string(hdr[i])
	}
	dir := "client_to_server"
	if fs.isServer {
		dir = "server_to_client"
	}
	payload := map[string]any{"request_line": line}
	if !fs.isServer {
		if pathStart := strings.Index(line, " "); pathStart >= 0 {
			rest := line[pathStart+1:]
			if pathEnd := strings.Index(rest, " "); pathEnd >= 0 {
				payload["path"] = rest[:pathEnd]
			}
		}
	} else {
		payload["status"] = line
	}
	return &Event{
		EventType: "godot_world.handshake",
		SchemaID:  "godot_world.handshake.v1",
		Payload:   payload,
		Meta:      meta(dir, "handshake", false),
	}
}

// processPacket decodes one SceneMultiplayer packet (one ws binary frame).
func processPacket(fs *flowState, pkt []byte) []*Event {
	// Server assigns the peer id with a raw 4-byte frame right after the 101.
	if fs.isServer && fs.firstPacket && len(pkt) == 4 {
		fs.firstPacket = false
		fs.peerID = binary.LittleEndian.Uint32(pkt)
		return []*Event{{
			EventType: "godot_world.peer_id",
			SchemaID:  "godot_world.peer_id.v1",
			Payload:   map[string]any{"peer_id": int64(fs.peerID)},
			Meta:      meta("server_to_client", "peer_id", false),
		}}
	}
	fs.firstPacket = false

	p := parseSm(pkt)
	dir := "client_to_server"
	if fs.isServer {
		dir = "server_to_client"
	}

	switch p.cmd {
	case cmdSYS:
		if p.sysCmd == sysCmdAuth {
			return []*Event{authEvent(fs, p)}
		}
		return []*Event{{
			EventType: "godot_world.sys",
			SchemaID:  "godot_world.sys.v1",
			Payload:   map[string]any{"sys_cmd": int64(p.sysCmd)},
			Meta:      meta(dir, "sys", true),
		}}
	case cmdSimplifyPath:
		fs.psc[p.pscID] = p.path
		return []*Event{{
			EventType: "godot_world.path_cache",
			SchemaID:  "godot_world.path_cache.v1",
			Payload:   map[string]any{"cmd": "simplify", "psc_id": int64(p.pscID), "path": p.path},
			Meta:      meta(dir, "simplify_path", false),
		}}
	case cmdConfirmPath:
		return []*Event{{
			EventType: "godot_world.path_cache",
			SchemaID:  "godot_world.path_cache.v1",
			Payload:   map[string]any{"cmd": "confirm", "psc_id": int64(p.pscID)},
			Meta:      meta(dir, "confirm_path", false),
		}}
	case cmdRemoteCall:
		node := p.resolvedNode(fs.psc)
		return rpcEvent(fs, p, node)
	default:
		return []*Event{{
			EventType: "godot_world.raw",
			SchemaID:  "godot_world.raw.v1",
			Payload:   map[string]any{"cmd": p.cmdName, "raw_hex": cappedHex(p.rawBody)},
			Meta:      meta(dir, p.cmdName, true),
		}}
	}
}

func authEvent(fs *flowState, p *smPacket) *Event {
	dir := "server_to_client"
	if !fs.isServer {
		dir = "client_to_server"
	}
	payload := map[string]any{}
	if fs.isServer {
		payload["challenge"] = string(p.authPayload)
	} else {
		// Client responds with var_to_bytes(token): [u32 type STRING][u32 len][chars].
		payload["auth_token"] = decodeAuthToken(p.authPayload)
	}
	return &Event{
		EventType: "godot_world.auth",
		SchemaID:  "godot_world.auth.v1",
		Payload:   payload,
		Meta:      meta(dir, "auth", false),
	}
}

func decodeAuthToken(b []byte) string {
	if len(b) >= 8 && binary.LittleEndian.Uint32(b[0:4]) == 4 { // STRING
		n := binary.LittleEndian.Uint32(b[4:8])
		if uint64(n) <= uint64(len(b)-8) {
			return string(b[8 : 8+n])
		}
	}
	return cappedHex(b)
}

// rpcEvent maps a remote call to a semantic game event based on the resolved
// node path and the RPC name id.
func rpcEvent(fs *flowState, p *smPacket, node string) []*Event {
	dir := "client_to_server"
	if fs.isServer {
		dir = "server_to_client"
	}

	base := node
	if i := strings.LastIndex(node, "/"); i >= 0 {
		base = node[i+1:]
	}

	// Root client node: the game's data RPCs.
	if node == "." {
		switch p.nameID {
		case 0:
			return []*Event{dataRPC(fs, dir, "godot_world.data_request", "data_request", p)}
		case 1:
			return []*Event{dataRPC(fs, dir, "godot_world.data_response", "data_response", p)}
		case 2:
			return []*Event{dataPush(fs, dir, p)}
		}
		return []*Event{genericRPC(fs, dir, node, p)}
	}

	// World server InstanceManager: instance lifecycle.
	if base == "InstanceManager" {
		switch p.nameID {
		case 0: // charge_new_instance(map_path, instance_id)
			if len(p.args) >= 2 {
				return []*Event{{
					EventType: "godot_world.instance_charge",
					SchemaID:  "godot_world.instance_charge.v1",
					Payload:   map[string]any{"map_path": fmt.Sprintf("%v", p.args[0]), "instance_id": fmt.Sprintf("%v", p.args[1])},
					Meta:      meta(dir, "charge_new_instance", true),
				}}
			}
		}
		return []*Event{genericRPC(fs, dir, node, p)}
	}

	// Instance server node (server side of an instance): player lifecycle.
	if base == "StateSynchronizerManager" {
		return stateSyncRPC(fs, dir, p)
	}

	// InstanceClient / InstanceServer: ready_to_enter_instance / spawn_player / despawn_player.
	if strings.Contains(node, "InstanceManager/") && p.nameID >= 0 && p.nameID <= 2 && len(p.byteOnly) == 0 {
		names := []string{"despawn_player", "ready_to_enter_instance", "spawn_player"}
		evType := "godot_world.instance_rpc"
		payload := map[string]any{"method": names[p.nameID]}
		if id := instanceIDFromNode(node); id != "" {
			payload["instance_id"] = id
		}
		if len(p.args) > 0 {
			payload["player_id"] = p.args[0]
		}
		return []*Event{{
			EventType: evType,
			SchemaID:  evType + ".v1",
			Payload:   payload,
			Meta:      meta(dir, names[p.nameID], !fs.isServer),
		}}
	}

	return []*Event{genericRPC(fs, dir, node, p)}
}

func instanceIDFromNode(node string) string {
	if i := strings.LastIndex(node, "/"); i >= 0 {
		id := node[i+1:]
		if id != "StateSynchronizerManager" {
			return id
		}
	}
	return ""
}

// stateSyncRPC handles the StateSynchronizerManager wire-codec RPCs.
func stateSyncRPC(fs *flowState, dir string, p *smPacket) []*Event {
	payload := p.byteOnly
	if payload == nil && len(p.args) > 0 {
		if s, ok := p.args[0].(string); ok && strings.HasPrefix(s, "<bytes:") {
			payload = nil // raw bytes unavailable in non-byte-only form
		}
	}
	if payload == nil {
		return []*Event{genericRPC(fs, dir, "StateSynchronizerManager", p)}
	}
	reg := getReg(fs.regKey)

	// Manager RPC name ids are alphabetical.
	switch p.nameID {
	case 0: // on_bootstrap
		if d, ok := reg.decodeBootstrap(payload); ok {
			d["method"] = "on_bootstrap"
			return []*Event{{
				EventType: "godot_world.state_bootstrap",
				SchemaID:  "godot_world.state_bootstrap.v1",
				Payload:   d,
				Meta:      meta(dir, "on_bootstrap", true),
			}}
		}
	case 1: // on_client_delta (c2s)
		if blocks, ok := reg.decodeDelta(payload); ok {
			return []*Event{{
				EventType: "godot_world.client_delta",
				SchemaID:  "godot_world.client_delta.v1",
				Payload:   map[string]any{"blocks": blocks, "count": len(blocks)},
				Meta:      meta(dir, "on_client_delta", false),
			}}
		}
	case 2: // on_props_bootstrap
		if d, ok := reg.decodeContainerBlock(payload); ok {
			d["method"] = "on_props_bootstrap"
			return []*Event{{
				EventType: "godot_world.props_bootstrap",
				SchemaID:  "godot_world.props_bootstrap.v1",
				Payload:   d,
				Meta:      meta(dir, "on_props_bootstrap", true),
			}}
		}
	case 3: // on_props_delta
		if d, ok := reg.decodeContainerBlock(payload); ok {
			d["method"] = "on_props_delta"
			return []*Event{{
				EventType: "godot_world.props_delta",
				SchemaID:  "godot_world.props_delta.v1",
				Payload:   d,
				Meta:      meta(dir, "on_props_delta", true),
			}}
		}
	case 4: // on_state_delta (s2c)
		if blocks, ok := reg.decodeDelta(payload); ok {
			return []*Event{{
				EventType: "godot_world.state_delta",
				SchemaID:  "godot_world.state_delta.v1",
				Payload:   map[string]any{"blocks": blocks, "count": len(blocks)},
				Meta:      meta(dir, "on_state_delta", true),
			}}
		}
	}
	return []*Event{genericRPC(fs, dir, "StateSynchronizerManager", p)}
}

// dataRPC builds events for _data_request/_data_response/data_push on the root
// node: args = [request_id, type, data, instance_id?].
func dataRPC(fs *flowState, dir, evType, name string, p *smPacket) *Event {
	payload := map[string]any{}
	if len(p.args) >= 1 {
		payload["request_id"] = p.args[0]
	}
	if len(p.args) >= 2 {
		payload["type"] = fmt.Sprintf("%v", p.args[1])
	}
	if len(p.args) >= 3 {
		payload["data"] = p.args[2]
	}
	if len(p.args) >= 4 {
		payload["instance_id"] = fmt.Sprintf("%v", p.args[3])
	}
	if _, hasData := payload["data"]; !hasData {
		payload["data"] = map[string]any{}
	}
	msgName := name
	if t, ok := payload["type"].(string); ok && t != "" {
		msgName = t
	}
	isPush := evType == "godot_world.data_push"
	return &Event{
		EventType:      evType,
		SchemaID:       evType + ".v1",
		Payload:        payload,
		Meta:           meta(dir, msgName, isPush),
		CorrelationKey: correlationFor(evType, payload),
	}
}

// dataPush builds the data_push event: args = [type, data?].
func dataPush(fs *flowState, dir string, p *smPacket) *Event {
	payload := map[string]any{}
	if len(p.args) >= 1 {
		payload["type"] = fmt.Sprintf("%v", p.args[0])
	}
	if len(p.args) >= 2 {
		payload["data"] = p.args[1]
	} else {
		payload["data"] = map[string]any{}
	}
	msgName := "data_push"
	if t, ok := payload["type"].(string); ok && t != "" {
		msgName = t
	}
	return &Event{
		EventType: "godot_world.data_push",
		SchemaID:  "godot_world.data_push.v1",
		Payload:   payload,
		Meta:      meta(dir, msgName, true),
	}
}

func genericRPC(fs *flowState, dir, node string, p *smPacket) *Event {
	payload := map[string]any{
		"node":   node,
		"method": fmt.Sprintf("<name:%d>", p.nameID),
	}
	if len(p.args) > 0 {
		payload["args"] = p.args
	}
	if len(p.byteOnly) > 0 {
		payload["raw_hex"] = cappedHex(p.byteOnly)
	}
	msgName := node
	if t, ok := payload["type"].(string); ok && t != "" {
		msgName = t
	}
	return &Event{
		EventType: "godot_world.rpc",
		SchemaID:  "godot_world.rpc.v1",
		Payload:   payload,
		Meta:      meta(dir, msgName, false),
	}
}

func meta(dir, msgName string, isPush bool) map[string]any {
	return map[string]any{
		"direction": dir,
		"msg_name":  msgName,
		"is_push":   isPush,
	}
}

func correlationFor(evType string, payload map[string]any) string {
	if id, ok := payload["request_id"]; ok {
		return fmt.Sprintf("req:%v", id)
	}
	return ""
}

func cappedHex(b []byte) string {
	if len(b) > 8192 {
		b = b[:8192]
	}
	return hex.EncodeToString(b)
}

// looksLikeHTTP reports whether the buffered bytes begin with an HTTP request
// or response line — i.e. a WebSocket upgrade. Mid-connection captures start
// with raw websocket frames (not HTTP), so they must skip the handshake gate.
func looksLikeHTTP(raw []byte) bool {
	if len(raw) < 4 {
		return false
	}
	n := 16
	if len(raw) < n {
		n = len(raw)
	}
	s := string(raw[:n])
	return strings.HasPrefix(s, "GET ") ||
		strings.HasPrefix(s, "POST ") ||
		strings.HasPrefix(s, "PUT ") ||
		strings.HasPrefix(s, "OPTIONS ") ||
		strings.HasPrefix(s, "HTTP/")
}

// wsResync returns the number of leading bytes to discard so the reassembly
// buffer re-aligns on a binary WebSocket frame header. It returns 0 when the
// buffer already starts on a valid-looking frame (an incomplete trailing frame
// — wait for more bytes) or contains no plausible frame start.
func wsResync(raw []byte) int {
	if len(raw) < 2 {
		return 0
	}
	// Already at a valid-looking frame header? Do not resync.
	if raw[0]&0x0F == 2 && raw[1]&0x7F < 126 {
		return 0
	}
	for i := 1; i < len(raw)-1; i++ {
		if raw[i]&0x0F == 2 && raw[i+1]&0x7F < 126 {
			return i
		}
	}
	return 0
}
