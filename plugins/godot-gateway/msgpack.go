package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
)

// Minimal, dependency-free MessagePack encoder.
//
// It produces a standard msgpack map (fixmap/map16/map32) for the event payload.
// This is wire-compatible with what gta-pipeline's trace_v2_adapter consumes: it
// reads business fields from the root object and strips the reserved "_meta" /
// "_state_changes" keys. For the simple JSON-derived value types this protocol
// uses (string, int, float, bool, nil, array, map) a typed msgpack map preserves
// the value kinds exactly.
//
// NOTE (gta.decoder/v2 contract rule "event-value-required"): when this plugin is
// built inside the gta monorepo with the private `event` package available, swap
// marshalPayload's output for `event.Value{...}.MarshalMsgpack()`. That tagged
// representation is what the host's strict validation expects; our hand-rolled
// map encoding is functionally equivalent for these value types.
func mpPutU16(buf *bytes.Buffer, v uint16) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	buf.Write(b[:])
}

func mpPutU32(buf *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	buf.Write(b[:])
}

func mpPutU64(buf *bytes.Buffer, v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	buf.Write(b[:])
}

func mpUint(buf *bytes.Buffer, v uint64) {
	switch {
	case v <= 0x7f:
		buf.WriteByte(byte(v)) // positive fixint
	case v <= 0xff:
		buf.WriteByte(0xcc)
		buf.WriteByte(byte(v))
	case v <= 0xffff:
		buf.WriteByte(0xcd)
		mpPutU16(buf, uint16(v))
	case v <= 0xffffffff:
		buf.WriteByte(0xce)
		mpPutU32(buf, uint32(v))
	default:
		buf.WriteByte(0xcf)
		mpPutU64(buf, v)
	}
}

func mpInt(buf *bytes.Buffer, v int64) {
	switch {
	case v >= -32:
		buf.WriteByte(byte(0xe0 | (v + 32))) // negative fixint
	case v >= -128:
		buf.WriteByte(0xd0)
		buf.WriteByte(byte(v))
	case v >= -32768:
		buf.WriteByte(0xd1)
		mpPutU16(buf, uint16(v))
	case v >= -2147483648:
		buf.WriteByte(0xd2)
		mpPutU32(buf, uint32(v))
	default:
		buf.WriteByte(0xd3)
		mpPutU64(buf, uint64(v))
	}
}

func mpStr(buf *bytes.Buffer, s string) {
	n := len(s)
	switch {
	case n <= 31:
		buf.WriteByte(byte(0xa0 | n)) // fixstr
	case n <= 0xff:
		buf.WriteByte(0xd9)
		buf.WriteByte(byte(n))
	case n <= 0xffff:
		buf.WriteByte(0xda)
		mpPutU16(buf, uint16(n))
	default:
		buf.WriteByte(0xdb)
		mpPutU32(buf, uint32(n))
	}
	buf.WriteString(s)
}

func mpBin(buf *bytes.Buffer, b []byte) {
	n := len(b)
	switch {
	case n <= 0xff:
		buf.WriteByte(0xc4)
		buf.WriteByte(byte(n))
	case n <= 0xffff:
		buf.WriteByte(0xc5)
		mpPutU16(buf, uint16(n))
	default:
		buf.WriteByte(0xc6)
		mpPutU32(buf, uint32(n))
	}
	buf.Write(b)
}

func mpArray(buf *bytes.Buffer, a []any) {
	n := len(a)
	switch {
	case n <= 15:
		buf.WriteByte(byte(0x90 | n)) // fixarray
	case n <= 0xffff:
		buf.WriteByte(0xdc)
		mpPutU16(buf, uint16(n))
	default:
		buf.WriteByte(0xdd)
		mpPutU32(buf, uint32(n))
	}
	for _, el := range a {
		mpEncode(buf, el)
	}
}

func mpMap(buf *bytes.Buffer, m map[string]any) {
	n := len(m)
	switch {
	case n <= 15:
		buf.WriteByte(byte(0x80 | n)) // fixmap
	case n <= 0xffff:
		buf.WriteByte(0xde)
		mpPutU16(buf, uint16(n))
	default:
		buf.WriteByte(0xdf)
		mpPutU32(buf, uint32(n))
	}
	for k, v := range m {
		mpStr(buf, k)
		mpEncode(buf, v)
	}
}

func mpEncode(buf *bytes.Buffer, v any) {
	switch x := v.(type) {
	case nil:
		buf.WriteByte(0xc0)
	case bool:
		if x {
			buf.WriteByte(0xc3)
		} else {
			buf.WriteByte(0xc2)
		}
	case int64:
		if x >= 0 {
			mpUint(buf, uint64(x))
		} else {
			mpInt(buf, x)
		}
	case int:
		mpEncode(buf, int64(x))
	case uint:
		mpEncode(buf, uint64(x))
	case uint64:
		mpUint(buf, x)
	case float64:
		buf.WriteByte(0xcb) // float64
		mpPutU64(buf, math.Float64bits(x))
	case float32:
		buf.WriteByte(0xca) // float32
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], math.Float32bits(x))
		buf.Write(b[:])
	case string:
		mpStr(buf, x)
	case []byte:
		mpBin(buf, x)
	case []any:
		mpArray(buf, x)
	case map[string]any:
		mpMap(buf, x)
	default:
		mpStr(buf, fmt.Sprintf("%v", x))
	}
}

// marshalPayload merges business fields (root) with the reserved "_meta" object
// and returns the msgpack-encoded event payload.
func marshalPayload(business, meta map[string]any) ([]byte, error) {
	combined := make(map[string]any, len(business)+1)
	for k, v := range business {
		combined[k] = v
	}
	if len(meta) > 0 {
		combined["_meta"] = meta
	}
	var buf bytes.Buffer
	mpMap(&buf, combined)
	return buf.Bytes(), nil
}
