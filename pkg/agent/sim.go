package agent

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
)

// simFramePrefixLen 是模拟游戏协议自身的长度前缀字节数（4 字节大端）。
// 注意：这只是模拟客户端/服务端的协议行为（真实游戏协议同样带自己的分帧，
// 由解码插件处理）；GameTrace 平台对字节流不做任何分帧假设。
const simFramePrefixLen = 4

// WriteFrame 按长度前缀协议写入一帧：4 字节大端长度 + payload。
// 用于模拟游戏客户端/服务端，便于在无 sing-box 的环境联调整条链路。
func WriteFrame(w io.Writer, payload []byte) error {
	header := make([]byte, simFramePrefixLen)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// ReadFrame 按长度前缀协议读取一帧，返回 payload。
// 长度头不完整或 payload 未收齐时阻塞等待；EOF 返回 io.EOF。
func ReadFrame(r io.Reader) ([]byte, error) {
	header := make([]byte, simFramePrefixLen)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(header)
	if n > 16<<20 {
		return nil, fmt.Errorf("frame length %d exceeds 16MB limit", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// EchoServer 启动一个极简"游戏服务端"：按帧读取请求，回一帧 JSON 应答。
// 返回 listener，调用方负责关闭。
func EchoServer(addr string, logger *slog.Logger) (net.Listener, error) {
	if logger == nil {
		logger = slog.Default()
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				for {
					payload, err := ReadFrame(c)
					if err != nil {
						return
					}
					resp := fmt.Sprintf(`{"ok":true,"echo":%d}`, len(payload))
					if err := WriteFrame(c, []byte(resp)); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return lis, nil
}

// RunSimClient 模拟游戏客户端：连接中继监听地址，依次发送各条消息并等待应答。
func RunSimClient(relayAddr string, messages [][]byte, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	conn, err := net.Dial("tcp", relayAddr)
	if err != nil {
		return fmt.Errorf("dial relay %s: %w", relayAddr, err)
	}
	defer conn.Close()
	for i, m := range messages {
		if err := WriteFrame(conn, m); err != nil {
			return fmt.Errorf("write msg %d: %w", i, err)
		}
		if _, err := ReadFrame(conn); err != nil {
			return fmt.Errorf("read resp %d: %w", i, err)
		}
	}
	return nil
}
