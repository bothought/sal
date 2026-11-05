//go:build windows

package upgrade

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// staleSuffix is the marker of a previous binary waiting to be deleted.
const staleSuffix = ".sal-old-"

// replaceExecutable swaps the running binary out of the way and the verified
// file into its place. Windows cannot overwrite a running executable, but it
// can rename it, so the old file is moved aside and deleted on the next start.
func replaceExecutable(target, newFile string) error {
	old := target + staleSuffix + fmt.Sprint(time.Now().UnixNano())
	var err error
	// A virus scanner or an indexer can hold the file for a moment.
	for i := 0; i < 3; i++ {
		if err = os.Rename(target, old); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("cannot move %s aside: %w", target, err)
	}
	if err := os.Rename(newFile, target); err != nil {
		// Put the working binary back where it was.
		os.Rename(old, target)
		return fmt.Errorf("cannot replace %s: %w", target, err)
	}
	return nil
}

// cleanupOldBinaries deletes binaries left by earlier upgrades. It runs at
// startup because the running process that created them could not.
func cleanupOldBinaries(exe string) {
	matches, err := filepath.Glob(exe + staleSuffix + "*")
	if err != nil {
		return
	}
	for _, m := range matches {
		os.Remove(m)
	}
}
