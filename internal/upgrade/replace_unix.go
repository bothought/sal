//go:build !windows

package upgrade

import (
	"fmt"
	"os"
)

// replaceExecutable renames the verified file over the installed one. The
// running process keeps the old inode, so the replacement is atomic: the target
// is either the old binary or the new one, never a partial file.
func replaceExecutable(target, newFile string) error {
	if err := os.Rename(newFile, target); err != nil {
		return fmt.Errorf("cannot replace %s: %w", target, err)
	}
	return nil
}

// cleanupOldBinaries is a no-op where the running binary can be replaced in
// place.
func cleanupOldBinaries(string) {}
