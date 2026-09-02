package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func makeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrateMovesTheWholeTree(t *testing.T) {
	base := t.TempDir()
	old := filepath.Join(base, "old")
	dst := filepath.Join(base, "new")

	files := map[string]string{
		"a.txt":           "one",
		"Docs/b.txt":      "two",
		"Docs/deep/c.bin": "three",
	}
	makeTree(t, old, files)

	when := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(old, "a.txt"), when, when); err != nil {
		t.Fatal(err)
	}

	if err := MigrateSyncRoot(old, dst); err != nil {
		t.Fatalf("MigrateSyncRoot: %v", err)
	}

	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("%s did not arrive: %v", rel, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("the old folder was left behind")
	}

	// Modification times are what the reconciler uses to decide what changed;
	// losing them would rehash the whole tree after a move.
	info, err := os.Stat(filepath.Join(dst, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().UTC().Truncate(time.Second).Equal(when) {
		t.Errorf("mtime = %v, want %v", info.ModTime().UTC(), when)
	}
}

func TestMigrateCreatesTheFolderWhenNothingToMove(t *testing.T) {
	base := t.TempDir()
	dst := filepath.Join(base, "new")

	if err := MigrateSyncRoot(filepath.Join(base, "never-existed"), dst); err != nil {
		t.Fatalf("MigrateSyncRoot: %v", err)
	}
	if info, err := os.Stat(dst); err != nil || !info.IsDir() {
		t.Errorf("the destination was not created: %v", err)
	}
}

func TestValidateSyncRootRefusesDangerousDestinations(t *testing.T) {
	base := t.TempDir()
	old := filepath.Join(base, "old")
	if err := os.MkdirAll(old, 0700); err != nil {
		t.Fatal(err)
	}

	home, _ := os.UserHomeDir()

	cases := map[string]string{
		"empty":                      "",
		"relative":                   "some/where",
		"the filesystem root":        "/",
		"the home directory":         home,
		"the current root":           old,
		"inside the current root":    filepath.Join(old, "inner"),
		"containing the current one": base,
	}
	for name, dst := range cases {
		if dst == "" && name != "empty" {
			continue
		}
		if err := ValidateSyncRoot(old, dst); err == nil {
			t.Errorf("%s (%q) was accepted", name, dst)
		}
	}
}

// Merging into a folder that already holds files would make them
// indistinguishable from synced content, and the next pass would upload them.
func TestValidateSyncRootRefusesNonEmptyDestination(t *testing.T) {
	base := t.TempDir()
	old := filepath.Join(base, "old")
	dst := filepath.Join(base, "occupied")
	makeTree(t, old, map[string]string{"a.txt": "x"})
	makeTree(t, dst, map[string]string{"someone-elses.txt": "important"})

	if err := ValidateSyncRoot(old, dst); err == nil {
		t.Error("a non-empty destination was accepted")
	}

	// And the migration itself must refuse, leaving both sides intact.
	if err := MigrateSyncRoot(old, dst); err == nil {
		t.Error("MigrateSyncRoot proceeded into a non-empty folder")
	}
	if _, err := os.Stat(filepath.Join(dst, "someone-elses.txt")); err != nil {
		t.Error("the destination's own files were disturbed")
	}
	if _, err := os.Stat(filepath.Join(old, "a.txt")); err != nil {
		t.Error("the source was disturbed by a refused migration")
	}
}

func TestValidateSyncRootAcceptsAnEmptyOrNewFolder(t *testing.T) {
	base := t.TempDir()
	old := filepath.Join(base, "old")
	empty := filepath.Join(base, "empty")
	if err := os.MkdirAll(empty, 0700); err != nil {
		t.Fatal(err)
	}

	if err := ValidateSyncRoot(old, empty); err != nil {
		t.Errorf("an empty folder was refused: %v", err)
	}
	if err := ValidateSyncRoot(old, filepath.Join(base, "brand-new")); err != nil {
		t.Errorf("a new folder was refused: %v", err)
	}
}

func TestMigrateRefusesAFileAsDestination(t *testing.T) {
	base := t.TempDir()
	old := filepath.Join(base, "old")
	makeTree(t, old, map[string]string{"a.txt": "x"})

	dst := filepath.Join(base, "afile")
	if err := os.WriteFile(dst, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := MigrateSyncRoot(old, dst); err == nil {
		t.Error("a regular file was accepted as the sync folder")
	}
}
