//go:build !unix

package Channel

import "os/exec"

// killProcessGroup 在非 unix 平台上不做额外处理：超时只结束 git 本身，WaitDelay 保证 Wait 不会无限等待子进程。
func killProcessGroup(cmd *exec.Cmd) {}
