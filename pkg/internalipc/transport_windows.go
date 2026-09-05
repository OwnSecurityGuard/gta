//go:build windows

package internalipc

import (
	"fmt"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

// SocketPath 返回 Windows 命名管道名：
//
//	\\.\pipe\gt-<name>
//
// Windows 命名管道不使用 workDir，保留参数以与 Unix 实现签名一致。
func SocketPath(workDir, name string) string {
	return fmt.Sprintf(`\\.\pipe\gt-%s`, name)
}

// Listen 在命名管道 \\.\pipe\gt-<name> 上监听。
func Listen(workDir, name string) (net.Listener, error) {
	return winio.ListenPipe(SocketPath(workDir, name), nil)
}

// Dial 拨号到命名管道 \\.\pipe\gt-<name>，超时 5 秒。
func Dial(workDir, name string) (net.Conn, error) {
	timeout := 5 * time.Second
	return winio.DialPipe(SocketPath(workDir, name), &timeout)
}
