// Command four-layer-demo is an example GTA decoder plugin that carries a
// COMPLETE four-layer semantic-contract declaration in its plugin.yaml:
//
//	Layer 1 (runtime)  -> capabilities: decode + schema + state + evidence + rules
//	Layer 2 (schema)   -> schemas: demo.player.v1
//	Layer 3 (state)    -> states: subject "player" backed by demo.player.v1
//	Layer 4 (evidence) -> evidence: demo.observation.login / .hp
//	Layer 5 (rule)     -> rules: demo.auth.login-success / demo.combat.hp-change
//
// It decodes a trivial single-line TCP protocol:
//
//	LOGIN <player_id> <name>   -> demo.login  event + hp state change + login evidence
//	HP    <player_id> <value>  -> demo.hp     event + hp state change + hp evidence
//
// The emitted payloads deliberately exercise every declared layer so the host's
// full-chain semantic validation (registration-time Check + decode-time
// CheckEvent) has something concrete to validate.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"

	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
	"github.com/OwnSecurityGuard/gta-plugin-sdk"
	"github.com/OwnSecurityGuard/gta-plugin-sdk/event"
	"github.com/OwnSecurityGuard/gta-plugin-sdk/framing"
)

func main() {
	sdk.RunRegisterLoop(decodePacket)
}

// decodePacket implements sdk.DecodeFuncV2. It strips the link-layer frame with
// framing.ExtractL7, then parses the L7 text line-by-line into events.
func decodePacket(req *pb.DecodeRequest, stream pb.Decoder_DecodeV2Server) error {
	inputID := req.GetInputId()
	defer func() {
		if r := recover(); r != nil {
			_ = stream.Send(&pb.DecodeResponseV2{
				InputId: inputID, Done: true,
				Error: fmt.Sprintf("decoder panic: %v", r),
			})
		}
	}()

	seg, ok := framing.ExtractL7(req.GetPayload(), req.GetLinkType())
	if !ok || len(seg.Payload) == 0 {
		// Nothing decodable in this frame; close the input cleanly.
		return stream.Send(&pb.DecodeResponseV2{InputId: inputID, Done: true})
	}

	sc := bufio.NewScanner(bytes.NewReader(seg.Payload))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 3)
		switch parts[0] {
		case "LOGIN":
			if len(parts) < 3 {
				continue
			}
			if err := emitLogin(stream, inputID, parts[1], parts[2]); err != nil {
				return err
			}
		case "HP":
			if len(parts) < 3 {
				continue
			}
			val, err := strconv.ParseInt(parts[2], 10, 64)
			if err != nil {
				continue
			}
			if err := emitHP(stream, inputID, parts[1], val); err != nil {
				return err
			}
		}
	}
	return stream.Send(&pb.DecodeResponseV2{InputId: inputID, Done: true})
}

// emitLogin builds and sends a demo.login event carrying the full four-layer
// payload: schema-conformant fields, a _state_changes (player hp set), and an
// _evidence (login observation referencing a declared rule).
func emitLogin(stream pb.Decoder_DecodeV2Server, inputID, pid, name string) error {
	d := event.Draft{
		Type:      "demo.login",
		SchemaRef: "demo.player.v1",
		Value: event.ValueFromMap(map[string]any{
			"player_id": pid,
			"name":      name,
			"hp":        int64(100),
			"_state_changes": []any{
				map[string]any{
					"subject_type": "player",
					"subject_id":   pid,
					"op":           "set",
					"path":         "hp",
					"before":       int64(0),
					"after":        int64(100),
					"version":      int64(1),
				},
			},
			"_evidence": []any{
				map[string]any{
					"kind":      "observation",
					"semantic":  "demo.observation.login",
					"statement": "player " + pid + " logged in",
					"strength":  float64(1.0),
					"method":    "decode",
					"sources":   []any{map[string]any{"kind": "event", "local": "self.event"}},
					"rule_id":   "demo.auth.login-success",
				},
			},
			"_meta": map[string]any{
				"direction": "client_to_server",
			},
		}),
		CorrelationKey: pid,
	}
	return sendDraft(stream, inputID, d)
}

// emitHP builds and sends a demo.hp event carrying a hp state change and an hp
// evidence entry referencing the declared combat rule.
func emitHP(stream pb.Decoder_DecodeV2Server, inputID, pid string, hp int64) error {
	d := event.Draft{
		Type:      "demo.hp",
		SchemaRef: "demo.player.v1",
		Value: event.ValueFromMap(map[string]any{
			"player_id": pid,
			"hp":        hp,
			"_state_changes": []any{
				map[string]any{
					"subject_type": "player",
					"subject_id":   pid,
					"op":           "set",
					"path":         "hp",
					"before":       int64(0),
					"after":        hp,
					"version":      int64(2),
				},
			},
			"_evidence": []any{
				map[string]any{
					"kind":      "observation",
					"semantic":  "demo.observation.hp",
					"statement": "player " + pid + " hp=" + strconv.FormatInt(hp, 10),
					"strength":  float64(1.0),
					"method":    "decode",
					"sources":   []any{map[string]any{"kind": "state_change", "local": "self.state[0]"}},
					"rule_id":   "demo.combat.hp-change",
				},
			},
			"_meta": map[string]any{
				"direction": "server_to_client",
			},
		}),
		CorrelationKey: pid,
	}
	return sendDraft(stream, inputID, d)
}

func sendDraft(stream pb.Decoder_DecodeV2Server, inputID string, d event.Draft) error {
	resp, err := d.ToResponse(inputID)
	if err != nil {
		return stream.Send(&pb.DecodeResponseV2{InputId: inputID, Done: true, Error: err.Error()})
	}
	if err := stream.Send(resp); err != nil {
		return err
	}
	return nil
}
