package mobile

import (
	"errors"
	"fmt"
	"strings"
)

// MobileConfig 是移动代理抓包源的配置。
//
// gt-singbox-agent 通过 gRPC 客户端流把连接级数据推送给本 Source：
//
//	sing-box ── TCP/Unix Socket ──▶ gt-singbox-agent ── gRPC stream ──▶ mobile Source
//
// 分帧职责归属：本 Source 不做应用层分帧/重组，收到的每个数据块按原样
// 转发为一个事件（即 raw 语义）。TCP 粘包/半包的处理、协议帧边界的
// 判定属于协议语义，由绑定到会话的解码插件按连接自行重组——
// 平台不持有也不配置任何协议编码知识。
type MobileConfig struct {
	// ListenAddr 是 gRPC server 监听地址：
	//   - TCP:   "127.0.0.1:9090"
	//   - Unix:  "unix:///tmp/gt-mobile.sock"（与 sing-box 同机部署时更安全）
	ListenAddr string

	// Activity 可选的运行时活动追踪器：非 nil 时 source 在 open/data/close
	// 事件中更新它（活跃连接数/累计连接/最近数据时间/累计字节），供
	// 控制面（lease 快照）查询实时连接状态。
	Activity *Activity
}

// validate 校验配置合法性。
func (c *MobileConfig) validate() error {
	if c == nil {
		return errors.New("mobile config is nil")
	}
	if strings.TrimSpace(c.ListenAddr) == "" {
		return errors.New("listen_addr is required")
	}
	return nil
}

// listenNetwork 从地址推断监听网络类型。
func (c *MobileConfig) listenNetwork() string {
	if strings.HasPrefix(c.ListenAddr, "unix://") {
		return "unix"
	}
	return "tcp"
}

// listenPath 返回 net.Listen 实际使用的地址（去掉 unix:// 前缀）。
func (c *MobileConfig) listenPath() string {
	if strings.HasPrefix(c.ListenAddr, "unix://") {
		return strings.TrimPrefix(c.ListenAddr, "unix://")
	}
	return c.ListenAddr
}

func validateConfig(cfg any) error {
	c, ok := cfg.(MobileConfig)
	if !ok {
		return fmt.Errorf("invalid config type %T, expected MobileConfig", cfg)
	}
	return c.validate()
}
