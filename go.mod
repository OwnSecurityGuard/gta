module gta

go 1.25.5

require (
	github.com/Microsoft/go-winio v0.6.2
	github.com/OwnSecurityGuard/gta-plugin-sdk v0.1.0
	github.com/expr-lang/expr v1.17.0
	github.com/google/gopacket v1.1.19
	github.com/google/uuid v1.6.0
	github.com/mark3labs/mcp-go v0.56.0
	github.com/vmihailenco/msgpack/v5 v5.4.1
	google.golang.org/grpc v1.71.0
	google.golang.org/protobuf v1.36.9
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.54.0
)

// 契约单向流动：SDK 定义，gta 消费。
// 开发期指向同级工作区的 SDK 源码，发布时去掉 replace 用打 tag 的版本。
replace github.com/OwnSecurityGuard/gta-plugin-sdk => ../ai_workspace/gta-plugin-sdk

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/jsonschema-go v0.4.2 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	github.com/spf13/cast v1.7.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/net v0.34.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.21.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250115164207-1a7da9e5054f // indirect
	modernc.org/libc v1.74.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
