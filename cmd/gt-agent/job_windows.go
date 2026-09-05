//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// childKiller 保证 agent 退出时插件子进程必死，即使 agent 被强杀
// （taskkill / Stop-Process / 崩溃），优雅停机路径（ctx 取消）不会执行。
//
// Windows 实现是 Job Object + JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE：
// agent 进程死 → 内核关闭 job 句柄 → OS 终止 job 内所有进程。
// 这正是孤儿插件问题的根修——此前强杀 agent 后插件存活，直连 registry
// 以同名新实例抢注，把 pipeline 的 dispatcher 绑到死隧道上。
type childKiller struct {
	job windows.Handle
}

func newChildKiller() (*childKiller, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("set job object info: %w", err)
	}
	return &childKiller{job: job}, nil
}

// prepare 在 cmd.Start 前调用（Windows 无需预处理）。
func (c *childKiller) prepare(cmd *exec.Cmd) error { return nil }

// attach 在 cmd.Start 成功后调用，把插件进程加入 job。
func (c *childKiller) attach(cmd *exec.Cmd) error {
	if c == nil || c.job == 0 {
		return nil
	}
	// os.Process 的内部句柄不导出：以 AssignProcessToJobObject 所需的
	// 权限自行打开一份（PROCESS_SET_QUOTA 是加入 job 的必要权限）。
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fmt.Errorf("open process %d: %w", cmd.Process.Pid, err)
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(c.job, h); err != nil {
		return fmt.Errorf("assign process to job object: %w", err)
	}
	return nil
}

// close 释放 job 句柄（触发 kill-on-close 杀掉所有子进程）。
func (c *childKiller) close() {
	if c != nil && c.job != 0 {
		_ = windows.CloseHandle(c.job)
		c.job = 0
	}
}
