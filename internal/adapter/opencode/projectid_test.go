package opencode

import "testing"

// deriveProjectID's remote branch used to FNV-hash the RAW url, so one origin
// cloned via different transports got different surrogate project ids. It now
// hashes canonicalkey.NormalizeRemote output via deriveSurrogateFromRemote;
// the pure helper carries the transport-invariance guarantee.
func TestDeriveSurrogateFromRemoteIsTransportInvariant(t *testing.T) {
	want := deriveSurrogateFromRemote("git@github.com:user/repo.git")
	if want == "" {
		t.Fatal("expected non-empty surrogate")
	}
	for _, remote := range []string{
		"https://github.com/user/repo.git",
		"ssh://git@github.com/user/repo.git",
		"https://Utkarsh@github.com/user/repo.git/",
		"ssh://git@github.com:22/user/repo.git",
	} {
		t.Run(remote, func(t *testing.T) {
			if got := deriveSurrogateFromRemote(remote); got != want {
				t.Errorf("deriveSurrogateFromRemote(%q) = %q, want %q", remote, got, want)
			}
		})
	}
}

func TestDeriveSurrogateFromRemoteKeepsDistinctProjectsDistinct(t *testing.T) {
	a := deriveSurrogateFromRemote("git@github.com:user/repo.git")
	b := deriveSurrogateFromRemote("git@github.com:other/api.git")
	c := deriveSurrogateFromRemote("https://gitlab.com/g/sub/r.git")
	d := deriveSurrogateFromRemote("https://gitlab.com/g/r.git")
	if a == b || c == d {
		t.Errorf("distinct origins must hash distinctly: a=%s b=%s c=%s d=%s", a, b, c, d)
	}
}
