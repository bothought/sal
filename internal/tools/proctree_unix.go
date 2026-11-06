//go:build unix

package tools

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group so that a timeout can
// kill the whole tree (shell + grandchildren).
func setProcessGroup(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	return nil
}

// afterStart is a no-op: a Unix process group is set before the process starts.
func afterStart(*exec.Cmd) error { return nil }

// releaseProcessGroup is a no-op where there is no handle to drop.
func releaseProcessGroup(*exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return
	}
	_ = cmd.Process.Kill()
}
