module godot-gateway

go 1.25.5

require github.com/OwnSecurityGuard/gta-plugin-sdk v0.1.0

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/google/gopacket v1.1.19 // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	golang.org/x/net v0.34.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.21.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250115164207-1a7da9e5054f // indirect
	google.golang.org/grpc v1.71.0 // indirect
	google.golang.org/protobuf v1.36.9 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// 该插件只依赖已发布的 github.com/OwnSecurityGuard/gta-plugin-sdk，不依赖 gta 源码树。
// 发布/获取 SDK：go get github.com/OwnSecurityGuard/gta-plugin-sdk@v0.1.0（或你们的模块代理）。
// 本地开发时临时加 replace 调试：go mod edit -replace github.com/OwnSecurityGuard/gta-plugin-sdk=<sdk路径>
