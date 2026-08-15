package watch

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agentsync/internal/session"
)

// fakeAdapter implements session.Adapter, recording OnChange calls.
type fakeAdapter struct {
	paths    []string
	mu       sync.Mutex
	calls    []string
	handlers map[string]func(session.WatchEvent) ([]session.Session, error)
}

func (f *fakeAdapter) Name() session.ToolKind { return session.ToolOpenCode }

func (f *fakeAdapter) WatchPaths() ([]string, error) { return f.paths, nil }

func (f *fakeAdapter) OnChange(ev session.WatchEvent) ([]session.Session, error) {
	f.mu.Lock()
	f.calls = append(f.calls, ev.Path)
	f.mu.Unlock()
	if h, ok := f.handlers[ev.Path]; ok {
		return h(ev)
	}
	return nil, nil
}

func (f *fakeAdapter) Mirror(s *session.Session, repoRoot string) error { return nil }

func (f *fakeAdapter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func TestReconcileFiresForExistingPath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "data.db")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ad := &fakeAdapter{paths: []string{file}}
	w, err := New(Options{Adapter: ad, Debounce: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Use a channel to signal when OnChange ran.
	done := make(chan struct{})
	go func() {
		ad.mu.Lock()
		defer ad.mu.Unlock()
		// wait for at least one call
		for len(ad.calls) == 0 {
			ad.mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			ad.mu.Lock()
		}
		close(done)
	}()

	go w.Start(ctx)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconciliation pass did not fire OnChange")
	}
}

func TestDeniedPathNeverWatched(t *testing.T) {
	dir := t.TempDir()
	denied := filepath.Join(dir, "secret")
	if err := os.MkdirAll(denied, 0o700); err != nil {
		t.Fatal(err)
	}
	ad := &fakeAdapter{paths: []string{dir}}
	w, err := New(Options{Adapter: ad, Debounce: 10 * time.Millisecond, DenyGlobs: []string{"*secret*"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Start(ctx)

	// Give the watcher a moment to attach watches, then write into the denied
	// dir. It must not produce an OnChange for it.
	time.Sleep(100 * time.Millisecond)
	os.WriteFile(filepath.Join(denied, "f.txt"), []byte("nope"), 0o600)
	time.Sleep(300 * time.Millisecond)

	for _, c := range ad.calls {
		if filepath.Clean(c) == filepath.Clean(denied) {
			t.Errorf("denied path %q was fired", denied)
		}
	}
}

func TestDebounceCollapsesBurstOfEvents(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "data.db")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ad := &fakeAdapter{paths: []string{file}}
	w, err := New(Options{Adapter: ad, Debounce: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Start(ctx)

	// Reconciliation will fire once already. Wait for it to settle.
	time.Sleep(300 * time.Millisecond)
	before := ad.callCount()

	// Write a burst of events within the debounce window.
	for i := 0; i < 5; i++ {
		os.WriteFile(file, []byte("y"), 0o600)
		time.Sleep(20 * time.Millisecond)
	}
	// Wait for the debounce window to elapse.
	time.Sleep(400 * time.Millisecond)

	got := ad.callCount() - before
	if got > 1 {
		t.Errorf("expected the burst to collapse to <=1 OnChange, got %d", got)
	}
}
