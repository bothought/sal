//go:build !windows

package tools

import (
	"os/exec"
)

// shellCommand returns the interpreter that runs a command line on a Unix-like
// system: bash when it is installed, sh otherwise.
func shellCommand(command string) (string, []string) {
	shell := "sh"
	if _, err := exec.LookPath("bash"); err == nil {
		shell = "bash"
	}
	return shell, []string{"-c", command}
}
