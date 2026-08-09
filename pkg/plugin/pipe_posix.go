//go:build !windows

package plugin

import (
	"fmt"
	"net"
	"runtime"
)

// dialNamedPipe 在非 Windows 平台不支持命名管道。
func dialNamedPipe(name string) (net.Conn, error) {
	return nil, fmt.Errorf("named pipes not supported on %s: %s", runtime.GOOS, name)
}
