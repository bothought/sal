//go:build !unix && !windows

package tools

import (
	"os/exec"
)

// Platforms with neither process groups nor job objects: kill just the child.
func setProcessGroup(cmd *exec.Cmd) error { return nil }

// afterStart is a no-op where nothing needs assigning once the process exists.
func afterStart(*exec.Cmd) error { return nil }

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// releaseProcessGroup is a no-op where there is no handle to drop.
func releaseProcessGroup(*exec.Cmd) {}
