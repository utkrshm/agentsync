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

// The four conflict-handling statuses persist and never gain retry
// scheduling: NextAttempt stays zero and Attempts are untouched.
func TestStoreTerminalStatusesPersistWithoutBackoff(t *testing.T) {
	statuses := []string{StatusPreserved, StatusConflicted, StatusDuplicated, StatusArchiveOnly}
	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "receive-state.json")
			s, err := Open(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Put(Outcome{
				ArtifactDigest: "d1",
				SessionID:      "ses_1",
				CandidatePath:  "/tmp/clone-a",
				Status:         status,
			}); err != nil {
				t.Fatal(err)
			}
			o, ok, err := s.Get("d1", "/tmp/clone-a")
			if err != nil || !ok {
				t.Fatalf("outcome missing: ok=%v err=%v", ok, err)
			}
			if o.Status != status {
				t.Errorf("status = %q, want %q", o.Status, status)
			}
			if !o.NextAttempt.IsZero() {
				t.Errorf("terminal status must not schedule a retry, got NextAttempt=%s", o.NextAttempt)
			}
			if o.Attempts != 0 {
				t.Errorf("terminal status must not count attempts, got Attempts=%d", o.Attempts)
			}
		})
	}
}

// A terminal status written over an earlier busy outcome must clear the
// scheduled retry (stale backoff may not survive a conflict verdict).
func TestStoreTerminalStatusClearsPendingRetry(t *testing.T) {
	p := filepath.Join(t.TempDir(), "receive-state.json")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Outcome{
		ArtifactDigest: "d1", SessionID: "ses_1",
		CandidatePath: "/tmp/clone-a", Status: StatusBusy,
	}); err != nil {
		t.Fatal(err)
	}
	busy, _, err := s.Get("d1", "/tmp/clone-a")
	if err != nil {
		t.Fatal(err)
	}
	if busy.Attempts != 1 || busy.NextAttempt.IsZero() {
		t.Fatalf("busy should schedule backoff, got %#v", busy)
	}

	if err := s.Put(Outcome{
		ArtifactDigest: "d1", SessionID: "ses_1",
		CandidatePath: "/tmp/clone-a", Status: StatusArchiveOnly,
	}); err != nil {
		t.Fatal(err)
	}
	o, ok, err := s.Get("d1", "/tmp/clone-a")
	if err != nil || !ok {
		t.Fatal(err)
	}
	if o.Status != StatusArchiveOnly {
		t.Errorf("status = %q, want archive-only", o.Status)
	}
	if !o.NextAttempt.IsZero() {
		t.Errorf("archive-only over busy must zero NextAttempt, got %s", o.NextAttempt)
	}
	if o.Attempts != 0 {
		t.Errorf("archive-only over busy must leave Attempts untouched by the new put, got %d", o.Attempts)
	}
}

// Lock the existing backoff behavior busy/failed keep.
func TestStoreBusyAndFailedKeepSchedulingRetries(t *testing.T) {
	for _, status := range []string{StatusBusy, StatusFailed} {
		p := filepath.Join(t.TempDir(), "receive-state.json")
		s, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Put(Outcome{
			ArtifactDigest: "d1", SessionID: "ses_1",
			CandidatePath: "/tmp/clone-a", Status: status,
		}); err != nil {
			t.Fatal(err)
		}
		o, ok, err := s.Get("d1", "/tmp/clone-a")
		if err != nil || !ok {
			t.Fatal(err)
		}
		if o.Status != status || o.Attempts != 1 || o.NextAttempt.IsZero() {
			t.Fatalf("%s outcome = %#v — want attempts=1 and a scheduled next attempt", status, o)
		}
	}
}
