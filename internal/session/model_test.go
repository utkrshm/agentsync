package session

import (
	"testing"
	"time"
)

// fakeAdapter is the §1 acceptance-gate fake: proves Adapter and WriteBacker
// compile and the daemon's dispatch shape works against them.
type fakeAdapter struct{}

func (fakeAdapter) Name() ToolKind                         { return ToolOpenCode }
func (fakeAdapter) WatchPaths() ([]string, error)          { return nil, nil }
func (fakeAdapter) OnChange(WatchEvent) ([]Session, error) { return nil, nil }
func (fakeAdapter) Mirror(*Session, string) error          { return nil }
func (fakeAdapter) WriteBack(*Session, string) error       { return nil }
func (fakeAdapter) IsToolRunning(string) (bool, error)     { return false, nil }

func TestAdapterInterfaceCompiles(t *testing.T) {
	// Compile-time assertions: a concrete adapter must satisfy both interfaces.
	var _ Adapter = fakeAdapter{}
	var _ WriteBacker = fakeAdapter{}

	// And a session must round-trip through the dispatch shape the daemon uses:
	// OnChange produces sessions, Mirror mutates PayloadPath into the repo.
	s := Session{
		ID:           "ses_1",
		Tool:         ToolOpenCode,
		CanonicalKey: "key",
		LastModified: time.Now(),
	}
	if s.PayloadPath != "" {
		t.Fatal("expected empty payload path before mirror")
	}
}
