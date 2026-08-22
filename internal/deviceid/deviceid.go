// Package deviceid manages the per-device identity persisted at
// ~/.config/agent-sync/device-id.
//
// The identifier is a random UUIDv4 generated once and reused forever:
// .sync-meta.json attributes sessions to devices by this id, so regenerating
// it would make the same physical device look like a brand-new one and
// orphan its previous history in the shared sync repo.
//
// LoadFrom is deliberately fail-closed: only a missing file triggers
// regeneration. A present-but-corrupt file is an error, never silently
// replaced — silent rotation is exactly the identity instability this
// package exists to fix, and an error tells the user to fix or delete the
// file explicitly instead.
package deviceid

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Path returns the device-id file location: ~/.config/agent-sync/device-id
// (same base dir as config.Path).
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agent-sync", "device-id"), nil
}

// LoadOrCreate returns this device's stable identifier, creating and
// persisting one on first use. File-not-found is the only regeneration
// trigger; anything else fails loudly.
func LoadOrCreate() (string, error) {
	p, err := Path()
	if err != nil {
		return "", err
	}
	return LoadFrom(p)
}

// LoadFrom is LoadOrCreate against an explicit path (injectable for tests).
func LoadFrom(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("read device id %s: %w", path, err)
		}
		id, err := newUUIDv4()
		if err != nil {
			return "", fmt.Errorf("generate device id: %w", err)
		}
		if err := persist(path, id); err != nil {
			return "", fmt.Errorf("persist device id %s: %w", path, err)
		}
		return id, nil
	}
	id := strings.TrimSpace(string(data))
	if !isValidUUID(id) {
		return "", fmt.Errorf(
			"device id file %s contains invalid UUID %q — fix or remove the file manually; refusing to regenerate silently because a new identity would fork this device's session history",
			path, id)
	}
	return id, nil
}

// persist writes id to path atomically: temp file in the same directory,
// owner-only permissions, fsync, then rename over the target.
func persist(path, id string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".device-id-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(id); err != nil {
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

// newUUIDv4 builds a random RFC 4122 version-4 UUID from crypto/rand:
// 16 random bytes with the version nibble set to 4 and the variant bits set
// to 10x, formatted 8-4-4-4-12 hex.
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant (10)
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, v := range b {
		switch i {
		case 4, 6, 8, 10:
			out = append(out, '-')
		}
		out = append(out, hexDigits[v>>4], hexDigits[v&0x0f])
	}
	return string(out), nil
}

// isValidUUID reports whether s is a canonical 8-4-4-4-12 hexadecimal UUID.
func isValidUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHexDigit(byte(c)) {
				return false
			}
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}
