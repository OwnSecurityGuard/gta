package main

import (
	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
	"github.com/OwnSecurityGuard/gta-plugin-sdk"
	"github.com/OwnSecurityGuard/gta-plugin-sdk/event"
)

func main() {
	sdk.RunRegisterLoop(decodePacket)
}

// decodePacket implements sdk.DecodeFuncV2 (gta.decoder/v2).
// req.Payload is the L7 application data (HTTP message on the gateway link).
// We never panic and always terminate the input with Done: true.
func decodePacket(req *pb.DecodeRequest, stream pb.Decoder_DecodeV2Server) error {
	defer func() {
		if r := recover(); r != nil {
			_ = stream.Send(&pb.DecodeResponseV2{
				InputId: req.GetInputId(),
				Error:   "decoder panic",
				Done:    true,
			})
		}
	}()

	events, derr := Decode(req.Payload)
	if derr != nil {
		return stream.Send(&pb.DecodeResponseV2{
			InputId: req.GetInputId(),
			Done:    true,
			Error:   derr.Error(),
		})
	}

	for _, e := range events {
		// business fields at root + reserved "_meta" object
		combined := make(map[string]any, len(e.Payload)+1)
		for k, v := range e.Payload {
			combined[k] = v
		}
		if len(e.Meta) > 0 {
			combined["_meta"] = e.Meta
		}
		// event.Value tagged encoding (contract rule: event-value-required)
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

	// terminal Done for this input_id
	return stream.Send(&pb.DecodeResponseV2{
		InputId: req.GetInputId(),
		Done:    true,
	})
}
