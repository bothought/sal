//go:build windows

package tools

import (
	"os/exec"
)

// shellCommand returns the interpreter that runs a command line on Windows.
//
// There is no POSIX shell there, so the tool falls back to the system's own.
// Git for Windows is common on machines that use sal, and its bash understands
// the commands a model writes for the other platforms, so it is preferred when
// present; cmd.exe is the guaranteed fallback.
func shellCommand(command string) (string, []string) {
	if path, err := exec.LookPath("bash"); err == nil {
		return path, []string{"-c", command}
	}
	if comspec := comspecPath(); comspec != "" {
		return comspec, []string{"/c", command}
	}
	return "cmd.exe", []string{"/c", command}
}
