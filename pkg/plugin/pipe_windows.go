//go:build windows

package plugin

import (
	"net"

	"github.com/Microsoft/go-winio"
)

// dialNamedPipe 拨号 Windows 命名管道。
func dialNamedPipe(name string) (net.Conn, error) {
	return winio.DialPipe(name, nil)
}
