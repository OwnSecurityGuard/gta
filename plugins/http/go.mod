module http

go 1.25.5

require (
	github.com/OwnSecurityGuard/gta-plugin-sdk v0.1.0
	github.com/google/gopacket v1.1.19
)

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
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

// 仓库内开发时指向本地 SDK 源码（E:\ai_workspace\gta-plugin-sdk）。
// 从 plugins/http 出发需向上三级再进 ai_workspace。
// 注意：SDK 已从 E:\gta-plugin-sdk 迁至 ai_workspace，旧目录是废弃副本
//（其 proto 的 go_package 仍是 gta/pkg/plugin/proto，与宿主冲突），不要再指向它。
// 分发给用户时去掉此 replace，直接引用已发布的远程模块：
// go get github.com/OwnSecurityGuard/gta-plugin-sdk@vX.Y.Z
replace github.com/OwnSecurityGuard/gta-plugin-sdk => ../../../ai_workspace/gta-plugin-sdk
