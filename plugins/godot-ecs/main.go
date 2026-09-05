package main

import (
	"os"

	"github.com/OwnSecurityGuard/gta-plugin-sdk"
	"github.com/OwnSecurityGuard/gta-plugin-sdk/event"
	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
)

func main() {
	// gta-agent 托管模式会注入 GTA_TUNNEL=1 / GTA_AUTH_TOKEN；
	// 手动启动时两个变量通常都不存在，opts 取零值，行为与 RunRegisterLoop 完全一致。
	//
	// 注意隧道与非隧道都要带 token：两种模式只是「谁拨谁」不同，
	// Register 走的是同一条鉴权链路，非隧道路径漏传 token 会被 registry 直接拒。
	sdk.RunRegisterLoopWithOptions(decodePacket, sdk.RegisterOptions{
		Tunnel: os.Getenv("GTA_TUNNEL") != "",
		// token 来源：优先 GTA_AUTH_TOKEN 环境变量（gta-agent 托管模式会注入）；
		// 回退到 zzz 的自助注册 token——IDE go run 不好传 env，先保住插件归属，
		// 换正式身份/改用脚本启动时删掉回退值。（SDK 侧同样有 env 回退逻辑。）
		AuthToken: envOr("GTA_AUTH_TOKEN", "gta_b8f3ffe5e91fce6b785e8d728d541492359e88027e388c14"),
	})
}

// envOr 取环境变量，未设置时回退默认值。
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// decodePacket 实现 sdk.DecodeFuncV2（gta.decoder/v2）。
// req.Payload 在 pcap 路径下是完整链路层帧，先经 framing.ExtractL7 剥头、
// Reassembler 重组，再按 [4B 长度][JSON] 切帧解码。
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
			combined["_meta"] = e.Meta // 保留键，宿主解析进 Context 后剔除
		}
		mp, mErr := valueFromMap(combined).MarshalMsgpack()
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

	// 每个 input 必须以 done=true 收尾（契约 done-required）。
	return stream.Send(event.Done(req.GetInputId()))
}
