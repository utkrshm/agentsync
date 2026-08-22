// Device-local producer provenance for OpenCode write-back.
//
// The adapter remembers which opencode binary it last wrote back with
// (~/.config/agent-sync/opencode-producer.json) and compares every write-back
// against that baseline. A changed path or hash usually means an upgrade:
// non-strict mode warns loudly and proceeds because the exact export↔installed
// version pin still guards correctness; strict mode refuses.
//
// This file never leaves the device — cross-device fingerprints inside sync-repo
// sidecars are deliberately deferred (Workstream E scope decision).
package opencode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// producerRecord is the device-local memory of the last-seen opencode binary.
type producerRecord struct {
	BinaryPath string `json:"binary_path"`
	SHA256     string `json:"sha256"`
	Version    string `json:"version"`
	LastSeen   string `json:"last_seen"` // RFC3339 UTC
}

// defaultProducerStatePath returns ~/.config/agent-sync/opencode-producer.json
// (same base dir as config.Path and the watch watermark).
func defaultProducerStatePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agent-sync", "opencode-producer.json"), nil
}

// loadProducerRecord reads the stored record. A missing file means no prior
// observation (nil, nil). A present-but-corrupt or incomplete file is an error
// naming the path — fail-closed exactly like internal/deviceid: silently
// treating it as absent would reset the baseline and hide real binary drift.
func loadProducerRecord(path string) (*producerRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read opencode producer state %s: %w", path, err)
	}
	var rec producerRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf(
			"opencode producer state %s is corrupt (%v) — fix or remove the file manually; refusing to treat it as absent because a reset baseline would hide binary drift",
			path, err)
	}
	if rec.BinaryPath == "" || !isSHA256Hex(rec.SHA256) {
		return nil, fmt.Errorf(
			"opencode producer state %s is incomplete (binary_path=%q sha256=%q) — fix or remove the file manually; refusing to treat it as absent",
			path, rec.BinaryPath, rec.SHA256)
	}
	return &rec, nil
}

// saveProducerRecord writes rec atomically: temp file in the same directory,
// owner-only permissions, fsync, then rename over the target.
func saveProducerRecord(path string, rec producerRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".opencode-producer-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
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

// Fingerprint returns the lowercase hex sha256 of the resolved opencode
// binary. The file is streamed (io.Copy into the hasher), never slurped whole.
func Fingerprint() (string, error) {
	path, err := BinaryPath()
	if err != nil {
		return "", err
	}
	return fingerprintFile(path)
}

func fingerprintFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s for hashing: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9') && !('a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// observeProducer runs the device-local drift check during ValidateArtifact:
//
//   - no stored record → store the current binary, proceed silently;
//   - record matches → refresh last_seen only, proceed silently;
//   - record differs → warn via Loggerf explaining what changed and proceed
//     (the exact version pin has already passed), unless StrictCheck refuses.
//
// A drifted fingerprint is NOT persisted here: the new baseline is recorded
// only after an import actually succeeds (see WriteBack.persistNewFingerprint),
// so a failed import keeps warning instead of silently re-baselining.
func (a *Adapter) observeProducer(installedVersion string) error {
	if a.BinaryPath == nil || a.Fingerprint == nil || a.ProducerStateFile == nil {
		return fmt.Errorf("producer drift check is not configured; refusing write-back")
	}
	binPath, err := a.BinaryPath()
	if err != nil {
		return fmt.Errorf("resolve opencode binary for producer check: %w", err)
	}
	fp, err := a.Fingerprint()
	if err != nil {
		return fmt.Errorf("fingerprint opencode binary %s: %w", binPath, err)
	}
	statePath, err := a.ProducerStateFile()
	if err != nil {
		return fmt.Errorf("resolve opencode producer state path: %w", err)
	}
	stored, err := loadProducerRecord(statePath)
	if err != nil {
		return err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	current := producerRecord{BinaryPath: binPath, SHA256: fp, Version: normalizeVersion(installedVersion)}
	switch {
	case stored == nil:
		current.LastSeen = now
		if err := saveProducerRecord(statePath, current); err != nil {
			return fmt.Errorf("record first observation of opencode binary: %w", err)
		}
		return nil
	case stored.BinaryPath == current.BinaryPath && stored.SHA256 == current.SHA256:
		if stored.LastSeen == now {
			return nil
		}
		rec := *stored
		rec.LastSeen = now
		if err := saveProducerRecord(statePath, rec); err != nil {
			return fmt.Errorf("refresh opencode producer last_seen: %w", err)
		}
		return nil
	default:
		detail := describeDrift(stored.BinaryPath, stored.SHA256, current.BinaryPath, current.SHA256)
		if a.StrictCheck {
			return fmt.Errorf("opencode binary changed since last write-back (%s) and strict_producer_check is enabled — refusing write-back; update the baseline by removing %s once you have verified the change", detail, statePath)
		}
		a.logWarning("WARNING: opencode binary changed since last write-back (%s); continuing because the exact version pin already passed, but verify this is an intended upgrade — baseline updates only after a successful import (state: %s)", detail, statePath)
		return nil
	}
}

// persistNewFingerprint re-baselines the producer record after a successful
// import. Called from WriteBack post-VerifyImport (chosen over doing it inside
// BroadcastWriteBack per candidate because BroadcastWriteBack delegates each
// candidate to WriteBack anyway — one persistence point covers both paths,
// and a candidate whose import failed keeps its old baseline). A version
// lookup failure skips re-baselining: the next run then warns drift again,
// which is the safe direction.
func (a *Adapter) persistNewFingerprint() {
	if a.BinaryPath == nil || a.Fingerprint == nil || a.ProducerStateFile == nil {
		return
	}
	version := ""
	if a.ToolVersion != nil {
		v, err := a.ToolVersion()
		if err != nil {
			a.logWarning("producer re-baseline skipped: read opencode --version: %v", err)
			return
		}
		version = normalizeVersion(v)
	}
	binPath, err := a.BinaryPath()
	if err != nil {
		a.logWarning("producer re-baseline skipped: resolve binary: %v", err)
		return
	}
	fp, err := a.Fingerprint()
	if err != nil {
		a.logWarning("producer re-baseline skipped: fingerprint %s: %v", binPath, err)
		return
	}
	statePath, err := a.ProducerStateFile()
	if err != nil {
		a.logWarning("producer re-baseline skipped: resolve state path: %v", err)
		return
	}
	rec := producerRecord{
		BinaryPath: binPath,
		SHA256:     fp,
		Version:    version,
		LastSeen:   time.Now().UTC().Format(time.RFC3339),
	}
	if err := saveProducerRecord(statePath, rec); err != nil {
		a.logWarning("producer re-baseline failed (%s): %v", statePath, err)
	}
}

// logWarning emits a loud user-facing warning through the injectable logger
// (defaults to stderr). Warnings are prose, not structured events, because
// they target the human running receive.
func (a *Adapter) logWarning(format string, args ...any) {
	if a.Loggerf != nil {
		a.Loggerf(format+"\n", args...)
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// describeDrift names exactly what changed between the stored and current
// binary observations.
func describeDrift(oldPath, oldHash, newPath, newHash string) string {
	var changes []string
	if oldPath != newPath {
		changes = append(changes, fmt.Sprintf("path changed from %s to %s", oldPath, newPath))
	}
	if oldHash != newHash {
		changes = append(changes, fmt.Sprintf("binary hash changed from %s…%s to %s…%s",
			oldHash[:12], oldHash[len(oldHash)-8:], newHash[:12], newHash[len(newHash)-8:]))
	}
	return strings.Join(changes, ", ")
}
