package main

// register.go：探针注册（AgentControl.RegisterProbe）。
// claim 启动码 → 换发长期凭证（probe_id + probe_token）→ 落盘 probe.json。
// 已有凭证直接复用；凭证被吊销（服务端拒绝）时清除本地凭证，等下次带用户 token 重接。

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"time"

	"gametrace/pkg/capture/agent/proto"
	"gametrace/pkg/version"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// ensureRegistered 确保探针已注册：有凭证直接返回；无凭证且有用户 token 时注册。
// 两者皆无（匿名单机）返回 false（远端控制禁用，本地控制面照常可用）。
func ensureRegistered(ctx context.Context, cfg *agentConfig, ingestAddr string) (bool, error) {
	if cfg.ProbeID != "" && cfg.ProbeToken != "" {
		return true, nil
	}
	if cfg.UserToken == "" {
		return false, nil
	}
	conn, err := grpc.NewClient("passthrough:///"+ingestAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return false, err
	}
	defer conn.Close()

	regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	regCtx = metadata.AppendToOutgoingContext(regCtx, "authorization", "Bearer "+cfg.UserToken)

	hostname, _ := os.Hostname()
	client := proto.NewAgentControlClient(conn)
	ack, err := client.RegisterProbe(regCtx, &proto.RegisterProbeRequest{
		Hostname:     hostname,
		Os:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Version:      version.String(),
		Capabilities: []string{"pcap", "plugin_host"},
		Name:         cfg.Name,
	})
	if err != nil {
		return false, fmt.Errorf("register probe: %w", err)
	}
	cfg.ProbeID = ack.GetProbeId()
	cfg.ProbeToken = ack.GetProbeToken()
	if err := saveAgentConfig(cfg); err != nil {
		return false, fmt.Errorf("save probe credentials: %w", err)
	}
	slog.Info("probe registered", "probe_id", cfg.ProbeID)
	return true, nil
}
