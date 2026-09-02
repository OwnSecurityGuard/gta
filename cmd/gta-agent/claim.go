// claim.go — agent 首启启动码领取：带 GTA-XXXX 启动码调服务端 /access/claim，
// 把返回的 sidecar 配置直接用做默认配置，实现目标机免参数自动注册并回连抓包。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// claimAccessCode 带启动码调服务端 /access/claim，返回可直接用作默认配置的
// embeddedAgentConfig。hostPort 是 mcp HTTP 地址（默认 127.0.0.1:8781）；
// code 形如 GTA-XXXX-XXXX。返回的 JSON 字段与 embeddedAgentConfig 的 json tag
// 一一对应（server/ingest_addr/token/session/bpf/plugin_names），故直接 Unmarshal。
func claimAccessCode(ctx context.Context, hostPort, code string) (embeddedAgentConfig, error) {
	var cfg embeddedAgentConfig
	if hostPort == "" {
		hostPort = "127.0.0.1:8781"
	}
	u := (&url.URL{
		Scheme: "http",
		Host:   hostPort,
		Path:   "/access/claim",
	}).String() + "?code=" + url.QueryEscape(code)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return cfg, err
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return cfg, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return cfg, fmt.Errorf("claim failed: HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("decode claim response: %w", err)
	}
	// server 缺 ingest 时由 deriveAddrs 推导（registry 端口 +1）。
	if cfg.IngestAddr == "" && cfg.Server != "" {
		if reg, ingest, err := deriveAddrs(cfg.Server, cfg.RegistryAddr, cfg.IngestAddr); err == nil {
			cfg.RegistryAddr, cfg.IngestAddr = reg, ingest
		}
	}
	return cfg, nil
}
