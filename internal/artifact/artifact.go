// Package artifact is the storage seam between session payloads and their
// durable location: it persists opaque byte blobs atomically and records the
// sha256 digest of exactly what was written. It validates nothing. Future
// layers (validation gates, metadata that references digests) wrap this type;
// they must not replace its plain-IO behavior.
package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"os"

	"agentsync/internal/fsutil"
)

// Store persists artifacts to disk. The zero value is ready to use.
type Store struct{}

// Write stores data at path (0600, atomic replace) and returns the sha256 hex
// digest of the written bytes.
func (Store) Write(path string, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	if err := fsutil.AtomicWriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return digest, nil
}

// Read returns the stored bytes at path.
func (Store) Read(path string) ([]byte, error) {
	return os.ReadFile(path)
}
