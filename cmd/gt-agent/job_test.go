package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestChildKillerKillsOnClose 验证绑杀机制：关闭 killer（等价于 agent 进程
// 死亡，job 句柄被内核关闭）后，已 attach 的插件子进程必须被 OS 终止。
// 这是孤儿插件问题的回归测试——强杀 agent 后插件存活会直连 registry
// 抢注实例，把 dispatcher 绑到死隧道上。
func TestChildKillerKillsOnClose(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("behavioral kill-on-close test is Windows Job Object specific; Unix uses Pdeathsig")
	}
	root := t.TempDir()
	outPath := filepath.Join(t.TempDir(), "env.txt")
	buildFakePlugin(t, root, "killtest", outPath, "stay")
	bin := filepath.Join(root, "killtest", "killtest"+exeSuffix())

	killer, err := newChildKiller()
	if err != nil {
		t.Fatalf("newChildKiller: %v", err)
	}
	cmd := exec.Command(bin)
	if err := killer.prepare(cmd); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := killer.attach(cmd); err != nil {
		t.Fatalf("attach: %v", err)
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	// 模拟 agent 死亡：关闭 job 句柄触发 KILL_ON_JOB_CLOSE。
	killer.close()

	select {
	case <-waitCh:
		// 子进程已终止。
	case <-time.After(10 * time.Second):
		t.Fatalf("plugin process still alive 10s after job close (pid %d)", cmd.Process.Pid)
	}
}
