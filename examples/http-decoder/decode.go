package main

import (
	"fmt"

	"github.com/OwnSecurityGuard/gta-plugin-sdk/framing"
	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
)

// maxStreamBytes caps per-flow reassembly memory. A flow whose bytes never
// parse as HTTP is dropped instead of growing without bound.
const maxStreamBytes = 4 << 20

// maxBodyBytes caps the captured body text per HTTP message (64KB).
// Longer bodies are truncated and flagged via body_truncated.
const maxBodyBytes = 64 << 10

// decoder holds all state that must survive across Decode calls: TCP
// reassembly is inherently cross-packet, and the per-flow message counters
// back the declared state layer.
type decoder struct {
	reasm  *framing.Reassembler
	counts map[string]flowCount // key: FlowKey.Canonical()
}

type flowCount struct {
	requests  int64
	responses int64
}

func newDecoder() *decoder {
	return &decoder{
		reasm:  framing.NewReassembler(),
		counts: make(map[string]flowCount),
	}
}

// decode implements sdk.DecodeFuncV2 for one captured frame. The reassembler
// and counters persist across calls via the decoder receiver.
func (d *decoder) decode(req *pb.DecodeRequest, stream pb.Decoder_DecodeV2Server) error {
	inputID := req.GetInputId()
	defer func() {
		if r := recover(); r != nil {
			_ = stream.Send(&pb.DecodeResponseV2{
				InputId: inputID, Done: true,
				Error: fmt.Sprintf("decoder panic: %v", r),
			})
		}
	}()

	// req.Payload is a FULL link-layer frame, not L7 bytes. ExtractL7 uses
	// link_type as the framing selector. ok=false (ARP/ICMP/truncated) is a
	// normal mixed-capture outcome, not an error.
	seg, ok := framing.ExtractL7(req.GetPayload(), req.GetLinkType())
	if !ok {
		return stream.Send(&pb.DecodeResponseV2{InputId: inputID, Done: true})
	}

	// Push even empty payloads: SYN/ACK/FIN flags drive per-flow state.
	st := d.reasm.Push(seg)
	flowID := seg.Flow.Canonical()

	for {
		buf := st.Bytes()
		if len(buf) == 0 {
			break
		}
		if len(buf) > maxStreamBytes {
			// Unparseable flow; drop the buffer to protect memory.
			st.Consume(len(buf))
			break
		}
		msg, n, ok := parseMessage(buf)
		if !ok {
			// Incomplete message: wait for more segments on a later call.
			break
		}
		if msg != nil {
			if err := d.emit(stream, inputID, flowID, msg); err != nil {
				return err
			}
		}
		st.Consume(n)
	}

	// Every input must be terminated with done=true, even when nothing decoded.
	return stream.Send(&pb.DecodeResponseV2{InputId: inputID, Done: true})
}
