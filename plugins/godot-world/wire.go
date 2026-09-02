package main

// wire.go — the game's custom wire codec (source/common/network/wire_codec.gd)
// used for state synchronisation payloads carried as a single PackedByteArray
// RPC argument.
//
//   - bootstrap (on_bootstrap): [u16 map_n][(u16 pid)(u32-len string path)(u8 wire_type)]*
//     [u16 obj_n][(u32 eid)(u16 pairs_n)[(u16 pid)(value)]*]*
//   - delta    (on_state_delta / on_client_delta):
//     [u16 block_n][(u32 eid)(u16 pairs_n)[(u16 pid)(value)]*]*
//   - container (on_props_bootstrap / on_props_delta):
//     [u32 cid][u16 spawn_n][(u16 child)(u16 scene)(var init)]*
//     [u16 pairs_n][(u32 cpid)(value by type_of(cpid & 0xFFFF))]*
//     [u16 despawn_n][(u16 id)]*
//     [u16 ops_n][(u16 child)(u32-len string method)(u8 argc)(var args)*]*
//
// pid -> (path, wire type) is the PathRegistry schema; bootstrap map updates
// extend it. wire_type values follow source/common/network/wire.gd.

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Wire.Type enum values.
const (
	wtVariant = 0
	wtBool    = 1
	wtU8      = 2
	wtU16     = 3
	wtU32     = 4
	wtU64     = 5
	wtS8      = 6
	wtS16     = 7
	wtS32     = 8
	wtS64     = 9
	wtF16     = 10
	wtF32     = 11
	wtF64     = 12
	wtStrU16  = 13
	wtStrU32  = 14
	wtBytes16 = 17
	wtBytes32 = 18
	wtVec2F32 = 19
)

// pathField is one PathRegistry entry.
type pathField struct {
	path string
	wt   int
}

// pathRegistry holds the static schema plus any bootstrap map updates.
type pathRegistry struct {
	byPID map[uint32]pathField
}

func newPathRegistry() *pathRegistry {
	r := &pathRegistry{byPID: map[uint32]pathField{}}
	// source/common/registry/path_registry.gd _static_init (in registration order)
	r.set(1, ":position", wtVec2F32)
	r.set(2, ":flipped", wtBool)
	r.set(3, ":anim", wtU8)
	r.set(4, ":pivot", wtF16)
	r.set(5, ":scale", wtVec2F32)
	r.set(6, ":display_name", wtVariant)
	r.set(7, ":skin_id", wtU16)
	r.set(8, ":zone_flags", wtU16)
	r.set(9, "EquipmentComponent:mainhand_id", wtU16)
	r.set(10, "StatsComponent:stats:health", wtF32)
	r.set(11, "StatsComponent:stats:health_max", wtF32)
	r.set(12, "StatsComponent:stats:mana", wtF32)
	r.set(13, "StatsComponent:stats:mana_max", wtF32)
	return r
}

func (r *pathRegistry) set(pid uint32, path string, wt int) {
	r.byPID[pid] = pathField{path: path, wt: wt}
}

func (r *pathRegistry) field(pid uint32) pathField {
	if f, ok := r.byPID[pid]; ok {
		return f
	}
	return pathField{path: fmt.Sprintf("<pid:%d>", pid), wt: wtVariant}
}

// applyMapUpdates applies the bootstrap map_updates to the registry.
func (r *pathRegistry) applyMapUpdates(updates []any) {
	for _, u := range updates {
		entry, ok := u.([]any)
		if !ok || len(entry) < 3 {
			continue
		}
		pid, ok := toU32(entry[0])
		if !ok {
			continue
		}
		path := fmt.Sprintf("%v", entry[1])
		wt, ok := toInt(entry[2])
		if !ok {
			wt = wtVariant
		}
		r.set(pid, path, wt)
	}
}

// decodeBootstrap parses an on_bootstrap payload. It mutates the registry with
// map updates before returning them so later deltas resolve pids correctly.
func (r *pathRegistry) decodeBootstrap(b []byte) (map[string]any, bool) {
	v := &vbuf{b: b}
	mapN, ok := v.u16()
	if !ok {
		return nil, false
	}
	updates := make([]any, 0, mapN)
	for i := 0; i < int(mapN) && i < 100000; i++ {
		pid, ok := v.u16()
		if !ok {
			return nil, false
		}
		path, ok := v.stringNoPad()
		if !ok {
			return nil, false
		}
		wt, ok := v.u8()
		if !ok {
			return nil, false
		}
		updates = append(updates, []any{uint64(pid), path, int64(wt)})
	}
	r.applyMapUpdates(updates)

	objN, ok := v.u16()
	if !ok {
		return nil, false
	}
	objects := make([]any, 0, objN)
	for i := 0; i < int(objN) && i < 100000; i++ {
		obj, ok := r.decodeEntityBlock(v)
		if !ok {
			return nil, false
		}
		objects = append(objects, obj)
	}
	return map[string]any{"map_updates": updates, "objects": objects}, true
}

// decodeDelta parses an on_state_delta / on_client_delta payload.
func (r *pathRegistry) decodeDelta(b []byte) ([]any, bool) {
	v := &vbuf{b: b}
	n, ok := v.u16()
	if !ok {
		return nil, false
	}
	blocks := make([]any, 0, n)
	for i := 0; i < int(n) && i < 100000; i++ {
		obj, ok := r.decodeEntityBlock(v)
		if !ok {
			return nil, false
		}
		blocks = append(blocks, obj)
	}
	return blocks, true
}

// decodeEntityBlock decodes one entity block: [u32 eid][u16 pairs][(u16 pid)(value)].
func (r *pathRegistry) decodeEntityBlock(v *vbuf) (map[string]any, bool) {
	eid, ok := v.u32()
	if !ok {
		return nil, false
	}
	pairN, ok := v.u16()
	if !ok {
		return nil, false
	}
	pairs := make([]any, 0, pairN)
	for j := 0; j < int(pairN) && j < 100000; j++ {
		pid, ok := v.u16()
		if !ok {
			return nil, false
		}
		f := r.field(uint32(pid))
		val, ok := r.decodeWireValue(v, f.wt)
		if !ok {
			return nil, false
		}
		pairs = append(pairs, map[string]any{"fid": int64(pid), "path": f.path, "value": val})
	}
	return map[string]any{"eid": int64(eid), "pairs": pairs}, true
}

// decodeContainerBlock parses an on_props_bootstrap / on_props_delta payload.
func (r *pathRegistry) decodeContainerBlock(b []byte) (map[string]any, bool) {
	v := &vbuf{b: b}
	cid, ok := v.u32()
	if !ok {
		return nil, false
	}
	// spawns
	spn, ok := v.u16()
	if !ok {
		return nil, false
	}
	spawns := make([]any, 0, spn)
	for i := 0; i < int(spn) && i < 100000; i++ {
		child, ok := v.u16()
		if !ok {
			return nil, false
		}
		scene, ok := v.u16()
		if !ok {
			return nil, false
		}
		init, ok := r.decodePutVar(v)
		if !ok {
			return nil, false
		}
		spawns = append(spawns, map[string]any{"child_id": int64(child), "scene_id": int64(scene), "init": init})
	}
	// pairs
	prn, ok := v.u16()
	if !ok {
		return nil, false
	}
	pairs := make([]any, 0, prn)
	for i := 0; i < int(prn) && i < 100000; i++ {
		cpid, ok := v.u32()
		if !ok {
			return nil, false
		}
		fid := cpid & 0xFFFF
		f := r.field(fid)
		val, ok := r.decodeWireValue(v, f.wt)
		if !ok {
			return nil, false
		}
		pairs = append(pairs, map[string]any{"cpid": int64(cpid), "fid": int64(fid), "path": f.path, "value": val})
	}
	// despawns
	dspn, ok := v.u16()
	if !ok {
		return nil, false
	}
	despawns := make([]any, 0, dspn)
	for i := 0; i < int(dspn) && i < 100000; i++ {
		d, ok := v.u16()
		if !ok {
			return nil, false
		}
		despawns = append(despawns, int64(d))
	}
	// ops_named
	opn, ok := v.u16()
	if !ok {
		return nil, false
	}
	ops := make([]any, 0, opn)
	for i := 0; i < int(opn) && i < 100000; i++ {
		child, ok := v.u16()
		if !ok {
			return nil, false
		}
		method, ok := v.stringNoPad()
		if !ok {
			return nil, false
		}
		argc, ok := v.u8()
		if !ok {
			return nil, false
		}
		args := make([]any, 0, argc)
		for a := 0; a < int(argc) && a < 64; a++ {
			av, ok := r.decodePutVar(v)
			if !ok {
				return nil, false
			}
			args = append(args, av)
		}
		ops = append(ops, map[string]any{"child_id": int64(child), "method": method, "args": args})
	}
	return map[string]any{"cid": int64(cid), "spawns": spawns, "pairs": pairs, "despawns": despawns, "ops_named": ops}, true
}

// decodePutVar decodes a StreamPeerBuffer.put_var value: [u32 byte_size][encode_variant].
// Godot 4.6 prepends the encoded size so get_var can bound allocations.
func (r *pathRegistry) decodePutVar(v *vbuf) (any, bool) {
	size, ok := v.u32()
	if !ok {
		return nil, false
	}
	if uint64(size) > uint64(v.remaining()) {
		return nil, false
	}
	start := v.o
	val, ok := decodeVariantFull(v)
	if !ok {
		return nil, false
	}
	end := start + int(size)
	if end > len(v.b) {
		end = len(v.b)
	}
	v.o = end
	return val, true
}

// decodeWireValue decodes one value given its wire type.
func (r *pathRegistry) decodeWireValue(v *vbuf, wt int) (any, bool) {
	switch wt {
	case wtVariant:
		return r.decodePutVar(v)
	case wtBool:
		b, ok := v.u8()
		return b != 0, ok
	case wtU8:
		b, ok := v.u8()
		return int64(b), ok
	case wtU16:
		x, ok := v.u16()
		return int64(x), ok
	case wtU32:
		x, ok := v.u32()
		return int64(x), ok
	case wtU64:
		if v.o+8 > len(v.b) {
			return nil, false
		}
		x := binary.LittleEndian.Uint64(v.b[v.o:])
		v.o += 8
		return x, true
	case wtS8:
		return v.i8()
	case wtS16:
		return v.i16()
	case wtS32:
		return v.i32()
	case wtS64:
		return v.i64()
	case wtF16:
		if v.o+2 > len(v.b) {
			return nil, false
		}
		h := binary.LittleEndian.Uint16(v.b[v.o:])
		v.o += 2
		return float64(f16tof32(h)), true
	case wtF32:
		return v.f32()
	case wtF64:
		return v.f64()
	case wtStrU16:
		n, ok := v.u16()
		if !ok || uint64(n) > uint64(v.remaining()) {
			return nil, false
		}
		s := string(v.b[v.o : v.o+int(n)])
		v.o += int(n)
		return s, true
	case wtStrU32:
		return v.stringNoPad()
	case wtBytes16:
		n, ok := v.u16()
		if !ok || uint64(n) > uint64(v.remaining()) {
			return nil, false
		}
		v.o += int(n)
		return fmt.Sprintf("<bytes:%d>", n), true
	case wtBytes32:
		n, ok := v.u32()
		if !ok || uint64(n) > uint64(v.remaining()) {
			return nil, false
		}
		v.o += int(n)
		return fmt.Sprintf("<bytes:%d>", n), true
	case wtVec2F32:
		x, ok := v.f32()
		if !ok {
			return nil, false
		}
		y, ok := v.f32()
		if !ok {
			return nil, false
		}
		return []any{math.Round(x*100) / 100, math.Round(y*100) / 100}, true
	default:
		return fmt.Sprintf("<wt:%d>", wt), true
	}
}

// f16tof32 converts a half-precision float to float32.
func f16tof32(h uint16) float32 {
	sign := uint32(h>>15) & 1
	exp := uint32(h>>10) & 0x1F
	frac := uint32(h) & 0x3FF
	var out uint32
	switch {
	case exp == 0:
		if frac == 0 {
			out = sign << 31
		} else {
			e := int32(exp) - 15 + 127
			m := frac
			for m&0x400 == 0 {
				m <<= 1
				e--
			}
			m &= 0x3FF
			out = sign<<31 | uint32(e)<<23 | m<<13
		}
	case exp == 31:
		out = sign<<31 | 0x7F800000 | frac<<13
	default:
		out = sign<<31 | (exp-15+127)<<23 | frac<<13
	}
	return math.Float32frombits(out)
}

func toU32(v any) (uint32, bool) {
	switch x := v.(type) {
	case uint64:
		return uint32(x), true
	case int64:
		return uint32(x), true
	case float64:
		return uint32(int64(x)), true
	case int:
		return uint32(x), true
	}
	return 0, false
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int64:
		return int(x), true
	case uint64:
		return int(x), true
	case float64:
		return int(x), true
	case int:
		return x, true
	}
	return 0, false
}
