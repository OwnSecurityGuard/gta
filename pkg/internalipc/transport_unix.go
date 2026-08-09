//go:build !windows

package internalipc

import (
	"net"
	"os"
	"path/filepath"
)

// SocketPath 返回指定 workDir 下 socket name 的 Unix Socket 路径：
//
//	<workDir>/run/<name>.sock
func SocketPath(workDir, name string) string {
	return filepath.Join(workDir, "run", name+".sock")
}

// Listen 在 <workDir>/run/<name>.sock 上监听 Unix Socket。
// 若父目录不存在会创建；若残留旧 socket 文件会被移除。
func Listen(workDir, name string) (net.Listener, error) {
	path := SocketPath(workDir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// 移除可能残留的旧 socket 文件。
	_ = os.Remove(path)
	return net.Listen("unix", path)
}

// Dial 拨号到 <workDir>/run/<name>.sock 上的 Unix Socket。
func Dial(workDir, name string) (net.Conn, error) {
	return net.Dial("unix", SocketPath(workDir, name))
}
