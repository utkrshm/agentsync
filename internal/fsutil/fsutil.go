// Package fsutil provides crash-safe filesystem primitives shared by the
// sync tooling: writes that either fully land or leave the previous content
// intact, never a truncated mix.
package fsutil

import (
	"os"
	"path/filepath"
)

// AtomicWriteFile writes data to path via a temp file created in the SAME
// directory as the final file, then renames it over the target. The temp file
// must be on the same filesystem as the destination because rename(2) is only
// atomic within one filesystem — a temp file in os.TempDir() could sit on a
// different mount, where the rename degrades to copy+delete and a crash can
// expose a partial file. A reader therefore observes either the old file or
// the complete new one, never a partial write.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".agent-sync-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	// After a successful rename this remove is a harmless no-op (the temp
	// name no longer exists); on any failure it cleans up.
	defer os.Remove(name)

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
