package gate

import (
	"os"
	"path/filepath"
	"testing"
)

// authorizeRoot is the gate's security boundary. It runs as root and takes
// requests over a world-writable socket, so without this check any local user
// could ask it to intercept opens in directories belonging to someone else
// and stall them.
func TestAuthorizeRootRejectsOtherUsersDirectories(t *testing.T) {
	s := &Server{}
	dir := t.TempDir()

	uid := uint32(os.Getuid())
	if _, err := s.authorizeRoot(dir, uid); err != nil {
		t.Errorf("own directory rejected: %v", err)
	}

	// Any other uid must be refused for the same directory.
	if _, err := s.authorizeRoot(dir, uid+1); err == nil {
		t.Error("a directory owned by someone else was accepted")
	}
}

func TestAuthorizeRootRejectsDangerousPaths(t *testing.T) {
	s := &Server{}
	uid := uint32(os.Getuid())

	bad := []string{
		"",                 // empty
		"relative/path",    // not absolute
		"/",                // the whole filesystem
		"/nonexistent-xyz", // missing
	}
	for _, p := range bad {
		if _, err := s.authorizeRoot(p, uid); err == nil {
			t.Errorf("authorizeRoot(%q) accepted a path it should refuse", p)
		}
	}
}

func TestAuthorizeRootRejectsFiles(t *testing.T) {
	s := &Server{}
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.authorizeRoot(f, uint32(os.Getuid())); err == nil {
		t.Error("a regular file was accepted as a sync root")
	}
}

func TestAuthorizeRootCleansPath(t *testing.T) {
	s := &Server{}
	dir := t.TempDir()

	got, err := s.authorizeRoot(dir+"/./", uint32(os.Getuid()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != filepath.Clean(dir) {
		t.Errorf("root = %q, want it cleaned to %q", got, filepath.Clean(dir))
	}
}

// Only paths inside the registered root may ever be held.
func TestUnderRoot(t *testing.T) {
	cases := []struct {
		path, root string
		want       bool
	}{
		{"/home/u/pdrive", "/home/u/pdrive", true},
		{"/home/u/pdrive/sub", "/home/u/pdrive", true},
		{"/home/u/pdrive/a/b/c", "/home/u/pdrive", true},
		{"/home/u/pdrive-other", "/home/u/pdrive", false},
		{"/home/u", "/home/u/pdrive", false},
		{"/etc/passwd", "/home/u/pdrive", false},
		{"/home/u/pdrive", "", false},
	}
	for _, c := range cases {
		if got := underRoot(c.path, c.root); got != c.want {
			t.Errorf("underRoot(%q, %q) = %v, want %v", c.path, c.root, got, c.want)
		}
	}
}
