//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// childKiller 的 Unix 实现：PR_SET_PDEATHSIG(SIGKILL)。
// 父进程（agent）死亡时内核向子进程投递 SIGKILL，覆盖强杀/崩溃场景。
type childKiller struct{}

func newChildKiller() (*childKiller, error) { return &childKiller{}, nil }

// prepare 在 cmd.Start 前设置 Pdeathsig（必须在 fork 时生效）。
func (c *childKiller) prepare(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	return nil
}

// attach 在 cmd.Start 后调用（Unix 无需处理）。
func (c *childKiller) attach(cmd *exec.Cmd) error { return nil }

// close 释放资源（Unix 无）。
func (c *childKiller) close() {}
