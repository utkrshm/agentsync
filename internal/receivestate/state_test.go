package receivestate

import (
	"path/filepath"
	"testing"
)

func TestStoreIsLocalAndPersistsPerCandidate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "receive-state.json")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Outcome{
		ArtifactDigest: "abc",
		SessionID:      "ses_1",
		CandidatePath:  "/tmp/clone-a",
		Status:         StatusVerified,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Get("abc", "/tmp/clone-b"); err != nil || ok {
		t.Fatalf("different candidate must not inherit acknowledgement: ok=%v err=%v", ok, err)
	}
	o, ok, err := s.Get("abc", "/tmp/clone-a")
	if err != nil || !ok || o.Status != StatusVerified {
		t.Fatalf("stored outcome = %#v, ok=%v, err=%v", o, ok, err)
	}
}
