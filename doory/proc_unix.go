//go:build linux || darwin

package doory

import (
	"os"
	"os/exec"
	"syscall"
)

func sysProcAttrSetsid() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup kills the worker AND its browser (renderer._kill_group:
// the browser lives in a process group of its own under the worker).
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}
}

var _ = os.Getpid
