package opencode

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// exportTempPrefix matches the temp payload files created by exportOne and
// the send command.
const exportTempPrefix = "agentsync-export-"

// SweepStaleTemps removes agentsync-export-* temp payloads older than maxAge
// from os.TempDir() and returns how many were removed. These files leak when
// the daemon or CLI dies between export and mirror; they contain session
// transcripts, so they must not linger indefinitely. Per-file removal errors
// are logged to stderr and skipped — a stuck file must not abort the sweep.
func SweepStaleTemps(maxAge time.Duration) int {
	return sweepStaleTemps(os.TempDir(), maxAge, time.Now())
}

func sweepStaleTemps(dir string, maxAge time.Duration, now time.Time) int {
	cutoff := now.Add(-maxAge)
	removed := 0
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opencode: sweep stale export temps in %s: %v\n", dir, err)
		return 0
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), exportTempPrefix) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			fmt.Fprintf(os.Stderr, "opencode: stat stale temp %s: %v\n", path, err)
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(path); err != nil {
			fmt.Fprintf(os.Stderr, "opencode: remove stale temp %s: %v\n", path, err)
			continue
		}
		removed++
	}
	return removed
}
