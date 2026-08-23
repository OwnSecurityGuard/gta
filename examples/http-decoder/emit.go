package main

import (
	"encoding/json"

	"github.com/OwnSecurityGuard/gta-plugin-sdk/event"
	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
)

// 消息定义：与 pkg/protocol protocol.yaml 的 message.definitions 保持一致。
const (
	cmdLoginRequest  = 1001 // 请求消息（role=request）
	cmdLoginResponse = 1002 // 正常响应（role=response）
	cmdPlayerNotify  = 2001 // 推送消息（role=push）
)

// msgName maps a cmd message id to its symbolic name (unknown when not declared).
func msgName(cmd int64) string {
	switch cmd {
	case cmdLoginRequest:
		return "LoginRequest"
	case cmdLoginResponse:
		return "LoginResponse"
	case cmdPlayerNotify:
		return "PlayerNotify"
	default:
		return "unknown"
	}
}

// envelopeSemantics holds the decoded header/body envelope fields of one HTTP
// message, covering message id / role / seq correlation / push rule / error.
type envelopeSemantics struct {
	Cmd       int64
	MsgName   string
	IsPush    bool // push rule: cmd==2001 或 body.seq==0
	Seq       int64
	ErrorCode int64
	IsError   bool // error rule: error_code != 0
}

// parseEnvelope best-effort extracts the envelope semantics from a JSON body.
// A malformed or non-JSON body yields the zero semantics (unknown, no error).
func parseEnvelope(body []byte) envelopeSemantics {
	var raw struct {
		Header struct {
			Cmd int64 `json:"cmd"`
		} `json:"header"`
		Body struct {
			Seq       int64 `json:"seq"`
			ErrorCode int64 `json:"error_code"`
		} `json:"body"`
	}
	_ = json.Unmarshal(body, &raw)

	s := envelopeSemantics{
		Cmd:       raw.Header.Cmd,
		Seq:       raw.Body.Seq,
		ErrorCode: raw.Body.ErrorCode,
	}
	s.MsgName = msgName(s.Cmd)
	s.IsPush = s.Cmd == cmdPlayerNotify || s.Seq == 0
	s.IsError = s.ErrorCode != 0
	return s
}

// role returns the communication role and push flag for one direction.
func (s envelopeSemantics) role(isRequest bool) (string, bool) {
	if s.IsPush {
		return "push", true
	}
	if isRequest {
		return "request", false
	}
	return "response", false
}

// emit turns one parsed HTTP message into a schema-conformant event carrying
// the envelope semantics and the declared state-change entry, then sends it.
func (d *decoder) emit(stream pb.Decoder_DecodeV2Server, inputID, flowID string, m *httpMessage) error {
	c := d.counts[flowID]
	sem := parseEnvelope(m.body)
	role, isPush := sem.role(m.isRequest)

	meta := map[string]any{
		"direction": "client_to_server",
		"msg_name":  sem.MsgName,
		"role":      role,
		"is_push":   isPush,
	}
	if !m.isRequest {
		meta["direction"] = "server_to_client"
	}

	payload := map[string]any{
		"flow_id":        flowID,
		"cmd":            sem.Cmd,
		"msg_name":       sem.MsgName,
		"role":           role,
		"is_push":        isPush,
		"seq":            sem.Seq,
		"body_text":      string(m.body),
		"body_truncated": m.bodyTruncated,
		"_meta":          meta,
	}

	var draft event.Draft
	if m.isRequest {
		c.requests++
		payload["method"] = m.method
		payload["path"] = m.path
		payload["requests"] = c.requests
		payload["_state_changes"] = []any{
			map[string]any{
				"subject_type": "http_request",
				"subject_id":   flowID,
				"op":           "set",
				"path":         "requests",
				"before":       c.requests - 1,
				"after":        c.requests,
				"version":      c.requests + c.responses,
			},
		}
		draft = event.Draft{
			Type:           "http.request",
			SchemaRef:      "http.request.v1",
			Value:          event.ValueFromMap(payload),
			CorrelationKey: flowID,
		}
	} else {
		c.responses++
		payload["status"] = m.status
		payload["responses"] = c.responses
		payload["error_code"] = sem.ErrorCode
		payload["is_error"] = sem.IsError
		payload["_state_changes"] = []any{
			map[string]any{
				"subject_type": "http_response",
				"subject_id":   flowID,
				"op":           "set",
				"path":         "responses",
				"before":       c.responses - 1,
				"after":        c.responses,
				"version":      c.requests + c.responses,
			},
		}
		draft = event.Draft{
			Type:           "http.response",
			SchemaRef:      "http.response.v1",
			Value:          event.ValueFromMap(payload),
			CorrelationKey: flowID,
		}
	}
	d.counts[flowID] = c

	resp, err := draft.ToResponse(inputID)
	if err != nil {
		return stream.Send(&pb.DecodeResponseV2{InputId: inputID, Done: true, Error: err.Error()})
	}
	return stream.Send(resp)
}
