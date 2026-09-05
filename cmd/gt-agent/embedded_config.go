//go:build embedded

// 下载形态的固化配置载体（仅当以 -tags embedded 构建时参与编译）。
//
// 服务端下发 agent 前会把下方 config.embedded.json 写入本包目录再执行
// `go build -tags "embedded pcap"`。go:embed 在编译期把该文件内容烧进二进制，
// 于是终端用户拿到的产物无需任何命令行参数即可回连服务端、托管指定插件并抓包。
//
// 不带 -tags embedded 的普通构建走 embedded_config_stub.go（返回空配置），
// 行为与既有命令行用法完全一致。
package main

import (
	_ "embed"
	"encoding/json"
)

//go:embed config.embedded.json
var embeddedConfigBytes []byte

// loadEmbeddedConfig 载入并解析固化配置；文件缺失或解析失败返回 (nil, false)。
// 失败按「无内嵌配置」处理：下载产物若有问题应在上游服务端构建时暴露，而非让
// 客户端静默退回命令行模式。
func loadEmbeddedConfig() (*embeddedAgentConfig, bool) {
	if len(embeddedConfigBytes) == 0 {
		return nil, false
	}
	var cfg embeddedAgentConfig
	if err := json.Unmarshal(embeddedConfigBytes, &cfg); err != nil {
		return nil, false
	}
	return &cfg, true
}
