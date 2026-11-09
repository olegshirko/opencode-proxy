// Package atomicio writes files that must never be observed half-written:
// state the proxy re-reads after a relaunch.
package atomicio

import (
	"os"
	"path/filepath"
)

// WriteFile replaces path's content atomically: the data lands in a sibling
// temp file that is renamed over the target. A crash mid-write leaves the old
// content intact and, at worst, one *.tmp-* file behind.
func WriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
