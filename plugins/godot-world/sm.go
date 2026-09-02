package main

// sm.go — Godot 4.6 SceneMultiplayer packet parser.
//
// Packet layout (Godot 4.x, SceneRPCInterface):
//   - meta byte: bits 0-2 command, bits 3-4 node-id compression
//     (0=1B,1=2B,2=4B,3=4B), bit 6 name-id compression (8-bit here), bit 7
//     byte_only (single PackedByteArray arg).
//   - SYS (7): [meta][sys_cmd:u8][payload]. Auth payload is the raw bytes the
//     sender passed to send_auth (server: ascii challenge; client:
//     var_to_bytes(token)).
//   - SIMPLIFY_PATH (1): [meta][md5 hex:32 bytes][0x00][psc_id:u32][path\0].
//   - CONFIRM_PATH (2): [meta][psc_id:u32].
//   - REMOTE_CALL (0): node id per compression width. High bit of a 32-bit id
//     marks a path-offset: id & 0x7FFFFFFF is the offset of a null-terminated
//     relative path appended at the end of the packet. Otherwise the id is a
//     psc_id resolved via the per-direction simplify cache. Then name_id:u8,
//     then either the byte_only PackedByteArray or [argc:u8][compressed args].

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

const (
	cmdRemoteCall    = 0
	cmdSimplifyPath  = 1
	cmdConfirmPath   = 2
	cmdRaw           = 3
	cmdSpawn         = 4
	cmdDespawn       = 5
	cmdSync          = 6
	cmdSYS           = 7
)

const sysCmdAuth = 0

var cmdNames = map[int]string{
	cmdRemoteCall: "REMOTE_CALL", cmdSimplifyPath: "SIMPLIFY_PATH",
	cmdConfirmPath: "CONFIRM_PATH", cmdRaw: "RAW", cmdSpawn: "SPAWN",
	cmdDespawn: "DESPAWN", cmdSync: "SYNC", cmdSYS: "SYS",
}

// nodeWidth maps the node-id compression field to a byte width.
var nodeWidth = [4]int{1, 2, 4, 4}

// smPacket is the decoded shape of one SceneMultiplayer packet.
type smPacket struct {
	meta    byte
	cmd     int
	cmdName string
	// SYS
	sysCmd  int
	authPayload []byte
	// path cache
	pscID   uint32
	path    string
	hash    string
	// remote call
	nodeIsPath bool
	nodePath   string
	nodePSC    uint32
	nameID     uint8
	byteOnly   []byte
	args       []any
	argc       int
	rawBody    []byte
}

// smResolve resolves node paths for a packet that needs the per-direction
// psc cache (called after parseSm with the owning flow's map).
func (p *smPacket) resolvedNode(psc map[uint32]string) string {
	if p.nodeIsPath {
		return p.nodePath
	}
	if p.nodePSC != 0 {
		if s, ok := psc[p.nodePSC]; ok {
			return s
		}
		return fmt.Sprintf("<psc:%d>", p.nodePSC)
	}
	return ""
}

// parseSm decodes one SceneMultiplayer packet. Path-offset resolution reads
// directly from the packet; psc_id nodes are resolved by the caller.
func parseSm(pkt []byte) *smPacket {
	p := &smPacket{rawBody: pkt}
	if len(pkt) == 0 {
		return p
	}
	p.meta = pkt[0]
	p.cmd = int(p.meta & 0x07)
	p.cmdName = cmdNames[p.cmd]
	o := 1

	if p.cmd == cmdSYS {
		if o >= len(pkt) {
			return p
		}
		p.sysCmd = int(pkt[o])
		o++
		if p.sysCmd == sysCmdAuth {
			p.authPayload = pkt[o:]
		}
		return p
	}

	nodeComp := (p.meta >> 4) & 0x03
	nameComp := (p.meta >> 6) & 0x01
	byteOnly := (p.meta >> 7) & 0x01
	w := nodeWidth[nodeComp]
	if o+w > len(pkt) {
		return p
	}
	var nodeTarget uint64
	switch w {
	case 1:
		nodeTarget = uint64(pkt[o])
	case 2:
		nodeTarget = uint64(binary.LittleEndian.Uint16(pkt[o:]))
	case 4:
		nodeTarget = uint64(binary.LittleEndian.Uint32(pkt[o:]))
	}
	o += w

	switch p.cmd {
	case cmdSimplifyPath:
		// [hash:32][00][psc_id:u32][path cstring]
		if o+32 > len(pkt) {
			return p
		}
		p.hash = string(pkt[o : o+32])
		o += 32
		// optional separator byte
		if o < len(pkt) && pkt[o] == 0 {
			o++
		}
		if o+4 > len(pkt) {
			return p
		}
		p.pscID = binary.LittleEndian.Uint32(pkt[o:])
		o += 4
		if end := bytes.IndexByte(pkt[o:], 0); end >= 0 {
			p.path = string(pkt[o : o+end])
		} else {
			p.path = string(pkt[o:])
		}
		return p
	case cmdConfirmPath:
		// [psc_id:u32]
		if o+4 <= len(pkt) {
			p.pscID = binary.LittleEndian.Uint32(pkt[o:])
		}
		return p
	case cmdRemoteCall:
		_ = nameComp
		end := len(pkt)
		if nodeTarget&0x80000000 != 0 {
			ofs := nodeTarget & 0x7FFFFFFF
			p.nodeIsPath = true
			if ofs < uint64(len(pkt)) {
				end = int(ofs) // path is appended at the end; args stop there
				if pe := bytes.IndexByte(pkt[ofs:], 0); pe >= 0 {
					p.nodePath = string(pkt[ofs : ofs+uint64(pe)])
				} else {
					p.nodePath = string(pkt[ofs:])
				}
			}
		} else {
			p.nodePSC = uint32(nodeTarget)
		}
		if o >= end {
			return p
		}
		p.nameID = pkt[o]
		o++
		if byteOnly != 0 {
			p.byteOnly = pkt[o:end]
			return p
		}
		if o >= end {
			return p
		}
		argc := int(pkt[o])
		p.argc = argc
		o++
		v := &vbuf{b: pkt, o: o, end: end}
		for i := 0; i < argc && argc < 64; i++ {
			val, ok := decodeVariantArg(v)
			if !ok {
				break
			}
			p.args = append(p.args, val)
		}
		return p
	default:
		p.rawBody = pkt[o:]
		return p
	}
}