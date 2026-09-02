package main

// variant.go — Godot 4.6 variant (de)serialization (marshalls) for the wire.
//
// Two forms are used by the world protocol:
//   - encode_and_compress_variant: one byte 0b[boolval|intmode|type] first.
//     BOOL stores its value in bit 7; INT stores a size mode in bits 6-7 and the
//     value follows in 1/2/4/8 little-endian bytes. Every other type falls
//     through to the full form below (the leading byte equals the low byte of
//     the u32 type field, so it is redundant).
//   - encode_variant (full): a little-endian u32 type field followed by type
//     data. This is what Dictionary keys/values, Array elements and
//     StreamPeerBuffer.put_var use. Non-compressed INT/FLOAT set bit 16
//     (ENCODE_FLAG_64) when the value needs 64 bits. Strings are padded to a
//     4-byte boundary measured from the start of the type field.

import (
	"encoding/binary"
	"fmt"
	"math"
)

type vbuf struct {
	b   []byte
	o   int
	end int
}

func (v *vbuf) limit() int {
	if v.end > 0 && v.end <= len(v.b) {
		return v.end
	}
	return len(v.b)
}
func (v *vbuf) remaining() int { return v.limit() - v.o }
func (v *vbuf) u8() (byte, bool) {
	if v.o >= v.limit() {
		return 0, false
	}
	b := v.b[v.o]
	v.o++
	return b, true
}
func (v *vbuf) u16() (uint16, bool) {
	if v.o+2 > v.limit() {
		return 0, false
	}
	x := binary.LittleEndian.Uint16(v.b[v.o:])
	v.o += 2
	return x, true
}
func (v *vbuf) u32() (uint32, bool) {
	if v.o+4 > v.limit() {
		return 0, false
	}
	x := binary.LittleEndian.Uint32(v.b[v.o:])
	v.o += 4
	return x, true
}
func (v *vbuf) i8() (int64, bool) {
	b, ok := v.u8()
	if !ok {
		return 0, false
	}
	return int64(int8(b)), true
}
func (v *vbuf) i16() (int64, bool) {
	x, ok := v.u16()
	if !ok {
		return 0, false
	}
	return int64(int16(x)), true
}
func (v *vbuf) i32() (int64, bool) {
	x, ok := v.u32()
	if !ok {
		return 0, false
	}
	return int64(int32(x)), true
}
func (v *vbuf) i64() (int64, bool) {
	if v.o+8 > len(v.b) {
		return 0, false
	}
	x := int64(binary.LittleEndian.Uint64(v.b[v.o:]))
	v.o += 8
	return x, true
}
func (v *vbuf) f32() (float64, bool) {
	if v.o+4 > len(v.b) {
		return 0, false
	}
	x := math.Float32frombits(binary.LittleEndian.Uint32(v.b[v.o:]))
	v.o += 4
	return float64(x), true
}
func (v *vbuf) f64() (float64, bool) {
	if v.o+8 > len(v.b) {
		return 0, false
	}
	x := math.Float64frombits(binary.LittleEndian.Uint64(v.b[v.o:]))
	v.o += 8
	return x, true
}

// skipPad advances to the next 4-byte boundary relative to start (the position
// of the u32 type field). Godot pads string payloads like this.
func skipPad(o, start int) int {
	total := o - start
	for total%4 != 0 {
		o++
		total++
	}
	return o
}

// decodeVariantArg decodes one encode_and_compress_variant (RPC argument).
// For BOOL/INT the meta byte fully describes the value. For every other type the
// meta byte IS the low byte of the u32 type field (still in the stream) and the
// remaining 3 bytes follow — so we must NOT consume it; decodeVariantFull reads
// the whole u32 from the current offset.
func decodeVariantArg(v *vbuf) (any, bool) {
	if v.o >= len(v.b) {
		return nil, false
	}
	meta := v.b[v.o] // peek without advancing
	typ := meta & 0x3F
	switch typ {
	case 1: // BOOL compressed
		v.o++ // consume the meta byte
		return (meta & 0x80) != 0, true
	case 2: // INT compressed
		v.o++ // consume the meta byte
		switch meta & 0xC0 {
		case 0x00:
			return v.i8()
		case 0x40:
			return v.i16()
		case 0x80:
			return v.i32()
		default:
			return v.i64()
		}
	}
	return decodeVariantFull(v)
}

// decodeVariantFull decodes one encode_variant ([u32 type][data]).
func decodeVariantFull(v *vbuf) (any, bool) {
	typ, ok := v.u32()
	if !ok {
		return nil, false
	}
	start := v.o - 4
	return decodeVariantBody(v, typ, start)
}

// decodeVariantBody decodes the type-specific payload; start is the byte offset
// of the type field (for string alignment).
func decodeVariantBody(v *vbuf, typ uint32, start int) (any, bool) {
	base := typ & 0xFFFF
	switch base {
	case 0: // NIL
		return nil, true
	case 1: // BOOL
		b, ok := v.u8()
		if !ok {
			return nil, false
		}
		v.o = skipPad(v.o, start)
		return b != 0, true
	case 2: // INT
		if typ&0x10000 != 0 {
			return v.i64()
		}
		return v.i32()
	case 3: // FLOAT
		if typ&0x10000 != 0 {
			return v.f64()
		}
		return v.f32()
	case 4: // STRING
		return v.string(start)
	case 21: // STRING_NAME
		return v.string(start)
	case 22: // NODE_PATH
		return v.string(start)
	case 27: // DICTIONARY
		cnt, ok := v.u32()
		if !ok {
			return nil, false
		}
		d := map[string]any{}
		for i := uint32(0); i < cnt && cnt < 100000; i++ {
			k, ok := decodeVariantFull(v)
			if !ok {
				return nil, false
			}
			val, ok := decodeVariantFull(v)
			if !ok {
				return nil, false
			}
			d[fmt.Sprintf("%v", k)] = val
		}
		return d, true
	case 28: // ARRAY
		cnt, ok := v.u32()
		if !ok {
			return nil, false
		}
		arr := make([]any, 0, cnt)
		for i := uint32(0); i < cnt && cnt < 100000; i++ {
			val, ok := decodeVariantFull(v)
			if !ok {
				return nil, false
			}
			arr = append(arr, val)
		}
		return arr, true
	case 5: // VECTOR2
		x, ok := v.f32()
		if !ok {
			return nil, false
		}
		y, ok := v.f32()
		if !ok {
			return nil, false
		}
		return []any{math.Round(x*100) / 100, math.Round(y*100) / 100}, true
	case 19: // PACKED_BYTE_ARRAY
		n, ok := v.u32()
		if !ok {
			return nil, false
		}
		if uint64(n) > uint64(v.remaining()) {
			return nil, false
		}
		v.o += int(n)
		v.o = skipPad(v.o, start)
		return fmt.Sprintf("<bytes:%d>", n), true
	case 30: // PACKED_INT32_ARRAY
		n, ok := v.u32()
		if !ok {
			return nil, false
		}
		arr := make([]any, 0, n)
		for i := uint32(0); i < n && i < 100000; i++ {
			x, ok := v.i32()
			if !ok {
				return nil, false
			}
			arr = append(arr, x)
		}
		v.o = skipPad(v.o, start)
		return arr, true
	case 34: // PACKED_STRING_ARRAY
		n, ok := v.u32()
		if !ok {
			return nil, false
		}
		arr := make([]any, 0, n)
		for i := uint32(0); i < n && i < 100000; i++ {
			s, ok := v.stringNoPad()
			if !ok {
				return nil, false
			}
			arr = append(arr, s)
		}
		v.o = skipPad(v.o, start)
		return arr, true
	default:
		return fmt.Sprintf("<type %d>", typ), true
	}
}

// string decodes a Godot string: [u32 len][utf8 bytes] + 4-byte alignment.
func (v *vbuf) string(start int) (string, bool) {
	n, ok := v.u32()
	if !ok {
		return "", false
	}
	if uint64(n) > uint64(v.remaining()) {
		return "", false
	}
	s := string(v.b[v.o : v.o+int(n)])
	v.o += int(n)
	v.o = skipPad(v.o, start)
	return s, true
}

// stringNoPad decodes a string without the trailing alignment (used inside
// PACKED_STRING_ARRAY where the surrounding type handles alignment).
func (v *vbuf) stringNoPad() (string, bool) {
	n, ok := v.u32()
	if !ok {
		return "", false
	}
	if uint64(n) > uint64(v.remaining()) {
		return "", false
	}
	s := string(v.b[v.o : v.o+int(n)])
	v.o += int(n)
	return s, true
}
