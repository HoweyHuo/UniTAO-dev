//go:build unix

package Channel

import (
	"os/exec"
	"syscall"
)

// killProcessGroup 让命令在自己的进程组里运行，被取消（超时）时结束整个进程组，
// 包括 git 启动的 ssh / git-remote-https。
func killProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
