package backup

import "testing"

// Names come from the server. A backup must never be able to write outside
// its destination directory, whatever the account contains.
func TestSanitizeComponentRejectsEscapes(t *testing.T) {
	bad := []string{
		"",
		".",
		"..",
		"../etc/passwd",
		"sub/dir",
		"/absolute",
		"with\x00nul",
	}
	for _, name := range bad {
		if _, err := SanitizeComponent(name); err == nil {
			t.Errorf("SanitizeComponent(%q) accepted a dangerous name", name)
		}
	}
}

func TestSanitizeComponentAcceptsRealNames(t *testing.T) {
	good := []string{
		"report.pdf",
		"holiday photo (1).jpg",
		"..hidden-but-fine",
		"...",
		"Ordner mit Umlauten äöü",
		"emoji 🎉.txt",
		"file with  spaces .tar.gz",
		"-leading-dash",
	}
	for _, name := range good {
		got, err := SanitizeComponent(name)
		if err != nil {
			t.Errorf("SanitizeComponent(%q) rejected a legitimate name: %v", name, err)
			continue
		}
		if got != name {
			t.Errorf("SanitizeComponent(%q) = %q, want it unchanged", name, got)
		}
	}
}

func TestSanitizeComponentLength(t *testing.T) {
	long := make([]byte, 256)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := SanitizeComponent(string(long)); err == nil {
		t.Error("a 256-byte name should be rejected")
	}

	ok := long[:255]
	if _, err := SanitizeComponent(string(ok)); err != nil {
		t.Errorf("a 255-byte name should be accepted: %v", err)
	}
}

func TestSanitizePath(t *testing.T) {
	parts, err := SanitizePath("Documents/2026/notes.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(parts) != 3 || parts[0] != "Documents" || parts[2] != "notes.md" {
		t.Errorf("SanitizePath = %v", parts)
	}
}

func TestSanitizePathRejectsTraversal(t *testing.T) {
	bad := []string{
		"",
		"/etc/passwd",
		"../../.bashrc",
		"Documents/../../../etc/shadow",
		"Documents//notes.md", // empty component
		"Documents/./notes.md",
		"a/b/../c",
	}
	for _, p := range bad {
		if parts, err := SanitizePath(p); err == nil {
			t.Errorf("SanitizePath(%q) accepted traversal, got %v", p, parts)
		}
	}
}
