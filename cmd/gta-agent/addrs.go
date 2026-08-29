package main

import (
	"fmt"
	"net"
)

const (
	defaultRegistryPort = "9091"
	defaultIngestPort   = "9092"
)

// deriveAddrs 由 --server（host 或 host:port）与可选的
// --registry-addr / --ingest-addr 覆盖项推导出插件注册地址与抓包推流地址。
//
// 规则：
//   - --server 只有 host：registry = host:9091，ingest = host:9092；
//   - --server 是 host:port：port 视为 registry 端口，ingest = port+1；
//   - 显式覆盖项优先；
//   - 两者最终都为空时报错（至少要给 --server 或显式地址）。
func deriveAddrs(server, registryOverride, ingestOverride string) (registry, ingest string, err error) {
	if server != "" {
		host, port, perr := net.SplitHostPort(server)
		if perr != nil {
			// 无端口：按纯 host 处理。
			host = server
			registry = net.JoinHostPort(host, defaultRegistryPort)
			ingest = net.JoinHostPort(host, defaultIngestPort)
		} else {
			p := 0
			if _, err := fmt.Sscanf(port, "%d", &p); err != nil || p <= 0 || p > 65535 {
				return "", "", fmt.Errorf("invalid --server port %q", port)
			}
			registry = net.JoinHostPort(host, port)
			ingest = net.JoinHostPort(host, fmt.Sprintf("%d", p+1))
		}
	}
	if registryOverride != "" {
		registry = registryOverride
	}
	if ingestOverride != "" {
		ingest = ingestOverride
	}
	if registry == "" && ingest == "" {
		return "", "", fmt.Errorf("either --server or --registry-addr/--ingest-addr is required")
	}
	return registry, ingest, nil
}
