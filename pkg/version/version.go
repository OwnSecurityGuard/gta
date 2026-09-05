// version.go — 构建版本信息（T14）。
//
// 三个变量均为包级 var（非 const），供构建时通过 -ldflags "-X" 注入：
//
//	go build -ldflags "-X gametrace/pkg/version.Version=v0.5.0 -X gametrace/pkg/version.Commit=abc1234" ./cmd/gt-pipeline
//
// 未注入时保留 dev 值：本地 go build / go run 出的二进制不带版本信息，
// 报告 "dev (unknown)"。各入口（gt-pipeline / gt-mcp / gt-agent）提供
// -version flag 打印后退出。
package version

// Version 是发布版本号，构建时注入 git tag（如 v0.5.0）。
var Version = "dev"

// Commit 是构建时的 git commit 短哈希。
var Commit = "unknown"

// String 返回 "dev (unknown)" 形式的人读版本串。
func String() string {
	return Version + " (" + Commit + ")"
}
