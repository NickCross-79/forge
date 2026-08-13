//go:build windows

package executor

import "os/exec"

// Windows has no process groups in the POSIX sense. Killing the shell is the
// best available approximation; a job that backgrounds work on Windows may leave
// children behind, which is noted in the security documentation.

func configureProcAttr(cmd *exec.Cmd) {}

func terminateGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func killGroup(cmd *exec.Cmd) {
	terminateGroup(cmd)
}
