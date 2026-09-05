//go:build !windows

package main

import (
	"os"
	"syscall"
)

var osKillSignal os.Signal = syscall.SIGTERM
