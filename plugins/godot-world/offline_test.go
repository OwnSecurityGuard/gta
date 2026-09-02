package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"

	pb "github.com/OwnSecurityGuard/gta-plugin-sdk/proto"
)

// TestOfflineSession replays the captured session frames (dump_frames.py
// output) through Decode and prints the events. Not part of the plugin build.
func TestOfflineSession(t *testing.T) {
	data, err := os.ReadFile(`C:\Users\SMALLZ~1\AppData\Local\Temp\opencode\session_frames.bin`)
	if err != nil {
		t.Fatalf("read frames: %v", err)
	}
	var total int
	for len(data) >= 8 {
		n := int(binary.LittleEndian.Uint32(data))
		lt := int32(binary.LittleEndian.Uint32(data[4:]))
		payload := data[8 : 8+n]
		data = data[8+n:]

		evs, err := Decode(&pb.DecodeRequest{Payload: payload, LinkType: lt})
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, e := range evs {
			total++
			if e.EventType == "godot-world.rpc" {
				if m, _ := e.Payload["method"].(string); strings.Contains(m, "name:0") || strings.Contains(m, "name:4") {
					if n, _ := e.Payload["node"].(string); strings.Contains(n, "StateSynchronizer") {
						rh, _ := e.Payload["raw_hex"].(string)
						b, _ := hex.DecodeString(rh)
						if strings.Contains(m, "name:0") {
							os.WriteFile(`C:\Users\SMALLZ~1\AppData\Local\Temp\opencode\go_bootstrap.bin`, b, 0644)
						} else {
							os.WriteFile(`C:\Users\SMALLZ~1\AppData\Local\Temp\opencode\go_statedelta.bin`, b, 0644)
						}
					}
				}
			}
			show := e.Payload
			if len(show) > 0 && e.EventType != "godot-world.path_cache" {
				fmt.Printf("%-32s %v\n", e.EventType, show)
			}
		}
	}
	fmt.Println("=== total events:", total)
}