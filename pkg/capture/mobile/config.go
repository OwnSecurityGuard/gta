package mobile

import (
	"errors"
	"fmt"
	"strings"
)

// FrameStyle 定义移动代理数据的输出粒度。
// 移动代理抓包拿到的是 TCP 字节流，不能一个 TCP read 当一个 packet，
// 需要按应用层协议分帧后再输出给解码器。
type FrameStyle string

const (
	// FrameRaw 不重组：每个数据块（一次 ConnData）直接作为一个帧输出。
	// 适用于解码器自己处理粘包/半包的场景（如插件内按长度前缀解析）。
	FrameRaw FrameStyle = "raw"

	// FrameLengthPrefix 长度前缀分帧：前 N 字节是长度（大端或小端），
	// 后面紧跟 payload。常见于游戏二进制协议（如 4 字节长度头）。
	FrameLengthPrefix FrameStyle = "length_prefix"
)

// MobileConfig 是移动代理抓包源的配置。
//
// gta-singbox-agent 通过 gRPC 客户端流把连接级数据推送给本 Source：
//
//	sing-box ── TCP/Unix Socket ──▶ gta-singbox-agent ── gRPC stream ──▶ mobile Source
type MobileConfig struct {
	// ListenAddr 是 gRPC server 监听地址：
	//   - TCP:   "127.0.0.1:9090"
	//   - Unix:  "unix:///tmp/gta-mobile.sock"（与 sing-box 同机部署时更安全）
	ListenAddr string

	// FrameStyle 分帧方式，默认 raw。
	FrameStyle FrameStyle

	// PrefixLen 长度前缀字节数（1|2|4），仅 FrameLengthPrefix 时有效，默认 4。
	PrefixLen int

	// LittleEndian 长度前缀字节序，默认大端。
	LittleEndian bool

	// MaxFrameSize 单帧最大字节数防御，默认 16MB；超过则丢弃并记错误。
	MaxFrameSize int

	// MaxConnSize 单连接缓冲上限，默认 16MB；超过则丢弃后续数据并记错误。
	MaxConnSize int
}

// validate 校验配置合法性。
func (c *MobileConfig) validate() error {
	if c == nil {
		return errors.New("mobile config is nil")
	}
	if strings.TrimSpace(c.ListenAddr) == "" {
		return errors.New("listen_addr is required")
	}
	switch c.FrameStyle {
	case "", FrameRaw, FrameLengthPrefix:
	default:
		return fmt.Errorf("unsupported frame_style %q (allowed: raw|length_prefix)", c.FrameStyle)
	}
	if c.FrameStyle == FrameLengthPrefix {
		switch c.PrefixLen {
		case 0:
			c.PrefixLen = 4
		case 1, 2, 4:
		default:
			return fmt.Errorf("prefix_len must be 1|2|4, got %d", c.PrefixLen)
		}
	}
	if c.MaxFrameSize < 0 || c.MaxConnSize < 0 {
		return errors.New("max_frame_size and max_conn_size must be non-negative")
	}
	return nil
}

// applyDefaults 填充未指定的默认值。
func (c *MobileConfig) applyDefaults() {
	if c.FrameStyle == "" {
		c.FrameStyle = FrameRaw
	}
	if c.FrameStyle == FrameLengthPrefix && c.PrefixLen == 0 {
		c.PrefixLen = 4
	}
	if c.MaxFrameSize == 0 {
		c.MaxFrameSize = 16 << 20
	}
	if c.MaxConnSize == 0 {
		c.MaxConnSize = 16 << 20
	}
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
