// Package revision defines the immutable per-revision artifact layout that
// replaces the overwritable flat export/<session-id>.json storage
// (docs/hardening-plan.md WS-B, docs/session-conflict-handling-plan.md §2):
//
//	opencode/<key>/sessions/<session-id>/revisions/<digest>.json        payload
//	opencode/<key>/sessions/<session-id>/revisions/<digest>.meta.json   sidecar
//
// The payload holds the exact validated export bytes; its filename is the
// lowercase hex SHA-256 of those bytes, which makes every revision
// self-verifying and structurally impossible to overwrite with different
// content. The sidecar records source metadata (device, capture time,
// producer version) as cheap, committed JSON.
package revision

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agentsync/internal/artifact"
	"agentsync/internal/fsutil"
)

// SchemaVersion is the sidecar schema version written by this package.
const SchemaVersion = 1

// Revision status values (Meta.Status).
const (
	StatusCaptured = "captured"
	StatusMigrated = "migrated"
)

// ErrNoMeta is returned by ReadMeta when the sidecar file does not exist.
// Detect it with errors.Is.
var ErrNoMeta = errors.New("revision metadata file not found")

// ErrDigestCollision is returned when a payload already exists at a digest
// path with DIFFERENT bytes. Two distinct byte streams cannot share a SHA-256
// digest, so reaching this means corruption or a hand-placed file — fail
// loudly rather than overwrite (docs/session-conflict-handling-plan.md:
// "never overwrite an immutable artifact").
var ErrDigestCollision = errors.New("digest collision")

// ErrSidecarConflict is returned when an existing sidecar at the target path
// records a different digest or original session id than the write being
// attempted. Sidecars are last-write-wins only among writes for the SAME
// revision; a mismatch means two revisions were mapped to one path.
var ErrSidecarConflict = errors.New("sidecar conflict")

// Meta is the per-revision sidecar committed alongside each payload. It is
// the receive-side patch source (project_id/directory) and the provenance
// record consumed by conflict detection and recovery tooling.
type Meta struct {
	SchemaVersion     int       `json:"schema_version"` // always 1
	OriginalSessionID string    `json:"original_session_id"`
	Digest            string    `json:"digest"` // 64 lowercase hex
	SourceDeviceID    string    `json:"source_device_id"`
	DeviceAlias       string    `json:"device_alias,omitempty"`
	CapturedAt        time.Time `json:"captured_at"` // UTC RFC3339
	ProducerVersion   string    `json:"producer_version"`
	Status            string    `json:"status"`               // captured | migrated
	ProjectID         string    `json:"project_id,omitempty"` // from export info; receive-side patch needs these
	Directory         string    `json:"directory,omitempty"`
	Title             string    `json:"title,omitempty"`
}

// Path returns the slash-relative payload location inside the sync repo:
// opencode/<key>/sessions/<sid>/revisions/<digest>.json
func Path(key, sessionID, digest string) string {
	return filepath.ToSlash(filepath.Join(
		"opencode", key, "sessions", sessionID, "revisions", digest+".json"))
}

// MetaPath returns the slash-relative sidecar location for a revision:
// opencode/<key>/sessions/<sid>/revisions/<digest>.meta.json
func MetaPath(key, sessionID, digest string) string {
	return filepath.ToSlash(filepath.Join(
		"opencode", key, "sessions", sessionID, "revisions", digest+".meta.json"))
}

// DigestBytes returns the lowercase hex sha256 of b — the revision identity
// used in payload filenames and sidecars.
func DigestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// validate enforces the meta contract against the content actually being
// written: non-empty matching session id, digest equal to the payload's real
// hash, and a known status.
func (m Meta) validate(sessionID, digest string) error {
	if sessionID == "" {
		return fmt.Errorf("revision write: empty session id")
	}
	if m.OriginalSessionID == "" {
		return fmt.Errorf("revision %s: metadata has empty original_session_id", sessionID)
	}
	if m.OriginalSessionID != sessionID {
		return fmt.Errorf("revision %s: metadata records original_session_id %q — refusing to store under another session",
			sessionID, m.OriginalSessionID)
	}
	if m.Digest == "" {
		return fmt.Errorf("revision %s: metadata has empty digest", sessionID)
	}
	if m.Digest != digest {
		return fmt.Errorf("revision %s: metadata digest %s does not match payload digest %s",
			sessionID, m.Digest, digest)
	}
	if m.Status != StatusCaptured && m.Status != StatusMigrated {
		return fmt.Errorf("revision %s: unknown status %q — want %q or %q",
			sessionID, m.Status, StatusCaptured, StatusMigrated)
	}
	return nil
}

// Write stores one revision under root (the sync repo working tree). It is
// idempotent for identical content and fails loudly on any attempt to put
// different bytes where a digest says they cannot be:
//
//   - payload missing → written atomically via artifact.Store.Write (0600);
//     the store-reported digest must equal the computed one;
//   - payload present with identical bytes → no-op for the payload
//     (written=false), sidecar still written if missing or different;
//   - payload present with different bytes → hard ErrDigestCollision error;
//   - existing sidecar recording another digest/session → hard
//     ErrSidecarConflict error. A compatible sidecar is overwritten
//     (last-write-wins) because only its cheap descriptive fields may drift.
//
// written reports whether the payload was newly created.
func Write(root, key, sessionID string, data []byte, meta Meta) (bool, error) {
	digest := DigestBytes(data)
	if err := meta.validate(sessionID, digest); err != nil {
		return false, err
	}
	payloadAbs := filepath.Join(root, filepath.FromSlash(Path(key, sessionID, digest)))
	metaAbs := filepath.Join(root, filepath.FromSlash(MetaPath(key, sessionID, digest)))

	written := false
	existing, err := os.ReadFile(payloadAbs)
	switch {
	case err == nil:
		if !bytes.Equal(existing, data) {
			return false, fmt.Errorf("%w: %s exists with different content — refusing to overwrite an immutable artifact",
				ErrDigestCollision, Path(key, sessionID, digest))
		}
	case os.IsNotExist(err):
		var store artifact.Store
		got, werr := store.Write(payloadAbs, data)
		if werr != nil {
			return false, werr
		}
		if got != digest {
			return false, fmt.Errorf("artifact store recorded digest %s but payload digests to %s", got, digest)
		}
		written = true
	default:
		return false, err
	}

	// Sidecar: overwrite when compatible (or corrupt/unreadable-as-JSON),
	// hard-fail when it belongs to another revision.
	if old, rerr := os.ReadFile(metaAbs); rerr == nil {
		var parsed Meta
		if jerr := json.Unmarshal(old, &parsed); jerr == nil &&
			(parsed.Digest != digest || parsed.OriginalSessionID != sessionID) {
			return written, fmt.Errorf("%w: %s records digest %q/session %q, refusing overwrite for digest %q/session %q",
				ErrSidecarConflict, MetaPath(key, sessionID, digest),
				parsed.Digest, parsed.OriginalSessionID, digest, sessionID)
		}
	} else if !os.IsNotExist(rerr) {
		return written, rerr
	}

	meta.SchemaVersion = SchemaVersion
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return written, err
	}
	return written, fsutil.AtomicWriteFile(metaAbs, append(encoded, '\n'), 0o600)
}

// ReadMeta decodes the sidecar at path. A missing file yields an error
// wrapping ErrNoMeta; present-but-invalid JSON or sidecars missing their core
// identity fields also fail rather than decode to a silent zero value.
func ReadMeta(path string) (Meta, error) {
	var m Meta
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return m, fmt.Errorf("%w: %s", ErrNoMeta, path)
		}
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("parse revision metadata %s: %w", path, err)
	}
	if m.OriginalSessionID == "" || m.Digest == "" || m.Status == "" {
		return m, fmt.Errorf("revision metadata %s: missing required field(s) original_session_id/digest/status", path)
	}
	return m, nil
}
