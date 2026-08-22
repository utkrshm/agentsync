package opencode

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweepStaleTempsRemovesOnlyOldFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	oldPath := filepath.Join(dir, exportTempPrefix+"old.json")
	freshPath := filepath.Join(dir, exportTempPrefix+"fresh.json")
	unrelatedOld := filepath.Join(dir, "other-old.json")
	for _, p := range []string{oldPath, freshPath, unrelatedOld} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Age the two non-fresh fixtures beyond the cutoff.
	stale := now.Add(-2 * time.Hour)
	if err := os.Chtimes(oldPath, stale, stale); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(unrelatedOld, stale, stale); err != nil {
		t.Fatal(err)
	}

	got := sweepStaleTemps(dir, time.Hour, now)
	if got != 1 {
		t.Fatalf("removed %d files, want 1", got)
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Error("fresh temp must be kept")
	}
	if _, err := os.Stat(unrelatedOld); err != nil {
		t.Error("non-export files must never be touched")
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Error("stale export temp should have been removed")
	}
}

func TestSweepStaleTempsMissingDirIsNotFatal(t *testing.T) {
	if got := sweepStaleTemps(filepath.Join(t.TempDir(), "absent"), time.Hour, time.Now()); got != 0 {
		t.Fatalf("missing dir should sweep nothing, got %d", got)
	}
}
