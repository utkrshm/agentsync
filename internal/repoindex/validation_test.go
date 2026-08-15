package repoindex

import (
	"path/filepath"
	"testing"

	"agentsync/internal/session"
)

func TestValidateCandidateRechecksIdentity(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "good")
	bad := filepath.Join(root, "bad")
	initRepo(t, good, "git@github.com:user/good.git")
	initRepo(t, bad, "git@github.com:user/bad.git")

	if err := ValidateCandidate(session.CanonicalKey("github.com-user-good"), good); err != nil {
		t.Fatalf("good candidate rejected: %v", err)
	}
	if err := ValidateCandidate(session.CanonicalKey("github.com-user-good"), bad); err == nil {
		t.Fatal("mismatched candidate identity was accepted")
	}
}
