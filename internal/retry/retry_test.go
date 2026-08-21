package retry

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRetryPersistsBackoffAndCompletes(t *testing.T) {
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	s, err := Open(filepath.Join(t.TempDir(), "retry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ScheduleAt(now, OperationPush, "sync-repo", "network down"); err != nil {
		t.Fatal(err)
	}
	items, err := s.Due(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 || items == nil {
		t.Fatalf("first retry should be delayed, got %#v", items)
	}
	items, err = s.Due(now.Add(time.Second))
	if err != nil || len(items) != 1 || items[0].Attempts != 1 {
		t.Fatalf("retry did not become due: items=%#v err=%v", items, err)
	}
	if err := s.ScheduleAt(now.Add(time.Second), OperationPush, "sync-repo", "still down"); err != nil {
		t.Fatal(err)
	}
	items, err = s.Due(now.Add(2 * time.Second))
	if err != nil || len(items) != 0 {
		t.Fatalf("second retry should have exponential delay: items=%#v err=%v", items, err)
	}
	if err := s.Complete(OperationPush, "sync-repo"); err != nil {
		t.Fatal(err)
	}
	items, err = s.Due(now.Add(time.Hour))
	if err != nil || len(items) != 0 {
		t.Fatalf("completed retry remained queued: items=%#v err=%v", items, err)
	}
}

func TestRetrySurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retry.json")
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ScheduleAt(now, OperationImport, "artifact\x00clone", "busy"); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.Due(now.Add(time.Second))
	if err != nil || len(items) != 1 || items[0].Operation != OperationImport {
		t.Fatalf("reopened store lost retry: items=%#v err=%v", items, err)
	}
}
