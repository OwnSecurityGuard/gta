package main

// ws.go — minimal RFC6455 WebSocket frame parser. Frames on the world link are
// binary (opcode 2); the HTTP upgrade handshake on each direction is skipped by
// the caller before frame parsing starts.

import (
	"encoding/binary"
)

type wsFrame struct {
	opcode  byte
	fin     bool
	payload []byte
}

// parseWSFrames extracts every complete frame from data. It returns the frames
// (payloads copied, unmasked) and the number of leading bytes consumed. A
// trailing partial frame yields consumed < len(data); callers wait for more
// bytes. Continuation fragments are NOT merged here — the caller buffers them.
func parseWSFrames(data []byte) ([]wsFrame, int) {
	var frames []wsFrame
	consumed := 0
	for {
		if len(data) < 2 {
			break
		}
		fin := data[0]&0x80 != 0
		op := data[0] & 0x0F
		masked := data[1]&0x80 != 0
		plen := int(data[1] & 0x7F)
		hdr := 2
		switch plen {
		case 126:
			if len(data) < 4 {
				return frames, consumed
			}
			plen = int(binary.BigEndian.Uint16(data[2:4]))
			hdr = 4
		case 127:
			if len(data) < 10 {
				return frames, consumed
			}
			plen = int(binary.BigEndian.Uint64(data[2:10]))
			hdr = 10
		}
		var mask []byte
		if masked {
			if len(data) < hdr+4 {
				return frames, consumed
			}
			mask = data[hdr : hdr+4]
			hdr += 4
		}
		if len(data) < hdr+plen {
			return frames, consumed
		}
		// Copy before unmasking so the payload never aliases the reassembly
		// buffer (invalidated by the next Consume).
		payload := append([]byte(nil), data[hdr:hdr+plen]...)
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		frames = append(frames, wsFrame{opcode: op, fin: fin, payload: payload})
		consumed += hdr + plen
		data = data[hdr+plen:]
	}
	return frames, consumed
}

// indexHTTPHeader finds the end of an HTTP header block (\r\n\r\n or \n\n).
func indexHTTPHeader(data []byte) int {
	if i := indexBytes(data, []byte("\r\n\r\n")); i >= 0 {
		return i + 4
	}
	if i := indexBytes(data, []byte("\n\n")); i >= 0 {
		return i + 2
	}
	return -1
}

func indexBytes(haystack, needle []byte) int {
	if len(needle) == 0 {
		return 0
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		ok := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}
