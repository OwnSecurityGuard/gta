// http-decoder plugin dependencies (locked, based on plugins/go.mod.template).
// Uses the published gta-plugin-sdk v0.4.1 (no replace), so it builds on any
// machine and in CI. Add a local replace temporarily only when debugging the SDK.
module http-decoder

go 1.25.5

require (
	github.com/OwnSecurityGuard/gta-plugin-sdk v0.4.1
	google.golang.org/grpc v1.71.0
)

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/google/gopacket v1.1.19 // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	golang.org/x/net v0.34.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.21.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250115164207-1a7da9e5054f // indirect
	google.golang.org/protobuf v1.36.9 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
