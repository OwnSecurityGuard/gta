package main

import (
	"github.com/OwnSecurityGuard/gta-plugin-sdk"
	"github.com/OwnSecurityGuard/gta-plugin-sdk/event"
	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
)

func main() {
	sdk.RunRegisterLoop(decodePacket)
}

// decodePacket implements sdk.DecodeFuncV2 (gta.decoder/v2).
// req.Payload is a complete link-layer frame on pcap paths; we strip it with
// framing.ExtractL7 and reassemble the per-flow TCP stream before parsing HTTP.
func decodePacket(req *pb.DecodeRequest, stream pb.Decoder_DecodeV2Server) error {
	events, err := Decode(req)
	if err != nil {
		return stream.Send(&pb.DecodeResponseV2{
			InputId: req.GetInputId(),
			Done:    true,
			Error:   err.Error(),
		})
	}

	for _, e := range events {
		combined := make(map[string]any, len(e.Payload)+1)
		for k, v := range e.Payload {
			combined[k] = v
		}
		if len(e.Meta) > 0 {
			combined["_meta"] = e.Meta
		}
		val := event.ValueFromMap(combined)
		mp, mErr := val.MarshalMsgpack()
		if mErr != nil {
			return stream.Send(&pb.DecodeResponseV2{
				InputId: req.GetInputId(),
				Done:    true,
				Error:   "marshal: " + mErr.Error(),
			})
		}
		if err := stream.Send(&pb.DecodeResponseV2{
			InputId:          req.GetInputId(),
			EventType:        e.EventType,
			SchemaId:         e.SchemaID,
			PayloadMsgpack:   mp,
			CorrelationKey:   e.CorrelationKey,
			CausationInputId: e.CausationInputID,
		}); err != nil {
			return err
		}
	}

	return stream.Send(&pb.DecodeResponseV2{InputId: req.GetInputId(), Done: true})
}
