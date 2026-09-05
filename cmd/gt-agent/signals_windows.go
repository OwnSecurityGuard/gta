//go:build windows

package main

import "os"

// osKillSignal 是 Windows 上可投递的终止信号（SIGTERM 无对应，用 SIGINT 兜底）。
var osKillSignal os.Signal = os.Interrupt
