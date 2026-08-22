package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
)

func TestStoreWriteDigestMatchesSHA256(t *testing.T) {
	var s Store
	data := []byte("session export bytes")
	digest, err := s.Write(filepath.Join(t.TempDir(), "export.json"), data)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if want := hex.EncodeToString(sum[:]); digest != want {
		t.Errorf("digest = %s, want %s", digest, want)
	}
}

func TestStoreReadRoundTrip(t *testing.T) {
	var s Store
	p := filepath.Join(t.TempDir(), "export.json")
	want := []byte(`{"info":{}}`)
	if _, err := s.Write(p, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("roundtrip = %q, want %q", got, want)
	}
}
