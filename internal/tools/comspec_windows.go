//go:build windows

package tools

import "os"

// comspecPath returns the command interpreter the system names, if any.
func comspecPath() string { return os.Getenv("COMSPEC") }
