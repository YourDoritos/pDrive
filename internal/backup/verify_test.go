package backup

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// buildBackup lays out a small backup on disk with a matching manifest.
func buildBackup(t *testing.T) (string, *Manifest) {
	t.Helper()
	dest := t.TempDir()
	dataRoot := filepath.Join(dest, DataDir)

	files := map[string]string{
		"notes.md":            "hello world\n",
		"Documents/plan.txt":  "phase 0.5\n",
		"Documents/empty.bin": "",
	}

	m := &Manifest{Version: 1, Account: "someone@proton.me", CreatedAt: time.Now().UTC()}
	m.Entries = append(m.Entries, Entry{Path: "Documents", IsDir: true, LinkID: "dir1"})
	m.Dirs = 1

	for path, content := range files {
		local := filepath.Join(dataRoot, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(local), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(local, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		sum, err := HashFile(local)
		if err != nil {
			t.Fatal(err)
		}
		m.Entries = append(m.Entries, Entry{
			Path: path, Size: int64(len(content)), SHA1: sum,
			LinkID: "link-" + path, DigestMatch: "match", RemoteDigest: sum,
		})
		m.Files++
		m.Bytes += int64(len(content))
	}

	if err := writeManifest(dest, m); err != nil {
		t.Fatal(err)
	}
	return dest, m
}

func TestVerifyCleanBackup(t *testing.T) {
	dest, m := buildBackup(t)

	res, err := Verify(dest, nil)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}
	if !res.Clean() {
		t.Errorf("clean backup did not verify: %+v", res)
	}
	if res.Checked != m.Files || res.OK != m.Files {
		t.Errorf("checked %d / ok %d, want %d each", res.Checked, res.OK, m.Files)
	}
}

func TestVerifyDetectsCorruption(t *testing.T) {
	dest, _ := buildBackup(t)

	// Flip the content of one file, keeping the manifest as it was.
	target := filepath.Join(dest, DataDir, "notes.md")
	if err := os.WriteFile(target, []byte("tampered\n"), 0600); err != nil {
		t.Fatal(err)
	}

	res, err := Verify(dest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Clean() {
		t.Fatal("corrupted backup verified clean")
	}
	if len(res.Mismatch) != 1 || res.Mismatch[0] != "notes.md" {
		t.Errorf("Mismatch = %v, want [notes.md]", res.Mismatch)
	}
}

func TestVerifyDetectsMissingFile(t *testing.T) {
	dest, _ := buildBackup(t)

	if err := os.Remove(filepath.Join(dest, DataDir, "Documents", "plan.txt")); err != nil {
		t.Fatal(err)
	}

	res, err := Verify(dest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Clean() {
		t.Fatal("backup with a missing file verified clean")
	}
	if len(res.Missing) != 1 || res.Missing[0] != "Documents/plan.txt" {
		t.Errorf("Missing = %v, want [Documents/plan.txt]", res.Missing)
	}
}

// A file recorded without a hash means its download failed during backup.
// That must surface as a gap, never be silently treated as fine.
func TestVerifyTreatsUnhashedEntryAsMissing(t *testing.T) {
	dest, m := buildBackup(t)
	m.Entries = append(m.Entries, Entry{Path: "failed-download.bin", LinkID: "x"})
	if err := writeManifest(dest, m); err != nil {
		t.Fatal(err)
	}

	res, err := Verify(dest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Clean() {
		t.Fatal("a failed download verified clean")
	}
	if len(res.Missing) != 1 || res.Missing[0] != "failed-download.bin" {
		t.Errorf("Missing = %v", res.Missing)
	}
}

// A disagreement with Proton's own digest is recorded at backup time and must
// still be reported by a later offline verify.
func TestVerifyReportsRemoteDigestMismatch(t *testing.T) {
	dest, m := buildBackup(t)
	for i := range m.Entries {
		if m.Entries[i].Path == "notes.md" {
			m.Entries[i].DigestMatch = "mismatch"
		}
	}
	if err := writeManifest(dest, m); err != nil {
		t.Fatal(err)
	}

	res, err := Verify(dest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Clean() {
		t.Fatal("a remote digest mismatch verified clean")
	}
	if len(res.RemoteMismatch) != 1 {
		t.Errorf("RemoteMismatch = %v", res.RemoteMismatch)
	}
}

func TestChecksumFileIsSha1sumCompatible(t *testing.T) {
	dest, _ := buildBackup(t)

	data, err := os.ReadFile(filepath.Join(dest, ChecksumName))
	if err != nil {
		t.Fatal(err)
	}
	lines := 0
	for _, line := range splitLines(string(data)) {
		if line == "" {
			continue
		}
		lines++
		// "<40 hex>  <path>" — exactly two spaces, as sha1sum writes.
		if len(line) < 43 || line[40:42] != "  " {
			t.Errorf("line is not sha1sum format: %q", line)
		}
	}
	if lines != 3 {
		t.Errorf("checksum file has %d lines, want 3 (directories excluded)", lines)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dest, want := buildBackup(t)

	got, err := LoadManifest(dest)
	if err != nil {
		t.Fatalf("LoadManifest() error: %v", err)
	}
	if got.Account != want.Account {
		t.Errorf("account = %q, want %q", got.Account, want.Account)
	}
	if got.Files != want.Files || got.Bytes != want.Bytes {
		t.Errorf("files/bytes = %d/%d, want %d/%d", got.Files, got.Bytes, want.Files, want.Bytes)
	}
	if len(got.Entries) != len(want.Entries) {
		t.Errorf("entries = %d, want %d", len(got.Entries), len(want.Entries))
	}
}

func TestLoadManifestMissing(t *testing.T) {
	if _, err := LoadManifest(t.TempDir()); err == nil {
		t.Error("expected an error loading a manifest that does not exist")
	}
}

func TestHashFileKnownValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "abc")
	if err := os.WriteFile(path, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	// Well-known SHA-1 of "abc".
	const want = "a9993e364706816aba3e25717850c26c9cd0d89d"
	got, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("HashFile = %s, want %s", got, want)
	}
}

func TestCompareDigest(t *testing.T) {
	if got := compareDigest("abc", ""); got != "absent" {
		t.Errorf("no remote digest = %q, want absent", got)
	}
	if got := compareDigest("ABC", "abc"); got != "match" {
		t.Errorf("case-insensitive compare = %q, want match", got)
	}
	if got := compareDigest("abc", "def"); got != "mismatch" {
		t.Errorf("differing digests = %q, want mismatch", got)
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
