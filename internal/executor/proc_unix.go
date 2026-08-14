//go:build !windows

package executor

import (
	"os/exec"
	"syscall"
)

// configureProcAttr puts the child in its own process group.
//
// Without this, killing the shell leaves anything it started — a dev server, a
// test runner, a background `&` job — running as an orphan. With it, forge can
// signal the entire group and be confident nothing survives a timeout or a
// cancellation.
func configureProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateGroup asks the job's process group to shut down cleanly.
func terminateGroup(cmd *exec.Cmd) {
	signalGroup(cmd, syscall.SIGTERM)
}

// killGroup forcibly stops a process group that ignored SIGTERM.
func killGroup(cmd *exec.Cmd) {
	signalGroup(cmd, syscall.SIGKILL)
}

// signalGroup sends sig to the negated PID, which POSIX interprets as "the whole
// process group". It falls back to signalling just the process if the group is
// gone, and ignores errors because the process may already have exited.
func signalGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, sig); err != nil {
		_ = syscall.Kill(pid, sig)
	}
}
