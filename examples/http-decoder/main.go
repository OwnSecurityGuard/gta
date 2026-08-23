// Command http-decoder is a GTA decoder plugin for the examples/http traffic.
//
// Pipeline: capture frame -> framing.ExtractL7 -> framing.Reassembler ->
// HTTP message -> envelope semantics -> event. It decodes HTTP/1.1 requests
// and responses on TCP, extracts the JSON envelope semantics
// (header.cmd / body.seq / body.error_code) and emits schema-conformant
// http.request / http.response events with state changes and _meta metadata.
package main

import (
	"github.com/OwnSecurityGuard/gta-plugin-sdk"
)

func main() {
	sdk.RunRegisterLoop(newDecoder().decode)
}
