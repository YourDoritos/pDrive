package mirror

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YourDoritos/pdrive/internal/config"
	"github.com/YourDoritos/pdrive/internal/drive"
)

// upsertDelta builds an event delta announcing a change to one link.
func upsertDelta(linkID, parentID string) *drive.Delta {
	return &drive.Delta{
		Cursor: "cursor-" + linkID,
		Changes: []drive.Change{{
			Kind: drive.ChangeUpsert, LinkID: linkID, ParentID: parentID,
		}},
	}
}

func mustSyncBoth(t *testing.T, m *Mirror) *Result {
	t.Helper()
	res, err := m.SyncBoth(context.Background())
	if err != nil {
		t.Fatalf("SyncBoth: %v", err)
	}
	return res
}

func writeLocal(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestUploadsNewLocalFile(t *testing.T) {
	f := newFakeDrive()
	m, _, root := newTestMirror(t, f, nil)
	mustSyncBoth(t, m)

	writeLocal(t, root, "notes.md", "written locally\n")
	res := mustSyncBoth(t, m)

	if res.Uploaded != 1 {
		t.Errorf("uploaded=%d, want 1", res.Uploaded)
	}
	if got := f.content("notes.md"); got != "written locally\n" {
		t.Errorf("remote content = %q", got)
	}
}

func TestUploadsIntoNewLocalDirectory(t *testing.T) {
	f := newFakeDrive()
	m, _, root := newTestMirror(t, f, nil)
	mustSyncBoth(t, m)

	writeLocal(t, root, "Projects/app/main.go", "package main\n")
	mustSyncBoth(t, m)

	if !f.has("Projects") || !f.has("Projects/app") {
		t.Fatalf("parent folders were not created remotely: %v", f.files)
	}
	if got := f.content("Projects/app/main.go"); got != "package main\n" {
		t.Errorf("remote content = %q", got)
	}
}

func TestLocalEditUploadsNewRevision(t *testing.T) {
	f := newFakeDrive()
	f.addFile("notes.md", "original\n")

	m, _, root := newTestMirror(t, f, nil)
	mustSyncBoth(t, m)

	writeLocal(t, root, "notes.md", "edited locally\n")
	res := mustSyncBoth(t, m)

	if res.Uploaded != 1 {
		t.Errorf("uploaded=%d, want 1", res.Uploaded)
	}
	if got := f.content("notes.md"); got != "edited locally\n" {
		t.Errorf("remote content = %q, want the local edit", got)
	}
}

func TestUnchangedFilesAreNotReuploaded(t *testing.T) {
	f := newFakeDrive()
	f.addFile("a.txt", "one")
	f.addFile("b.txt", "two")

	m, _, _ := newTestMirror(t, f, nil)
	mustSyncBoth(t, m)
	f.uploaded = nil

	res := mustSyncBoth(t, m)
	if res.Uploaded != 0 || len(f.uploaded) != 0 {
		t.Errorf("re-uploaded unchanged files: %v", f.uploaded)
	}
}

// Local deletion goes to Proton's trash, never a permanent delete.
func TestLocalDeletionTrashesRemotely(t *testing.T) {
	f := newFakeDrive()
	f.addFile("gone.txt", "delete me")
	f.addFile("stay.txt", "keep me")

	m, _, root := newTestMirror(t, f, nil)
	mustSyncBoth(t, m)

	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	res := mustSyncBoth(t, m)

	if res.TrashedRemote != 1 {
		t.Errorf("trashedRemote=%d, want 1", res.TrashedRemote)
	}
	if f.has("gone.txt") {
		t.Error("the file should be gone from the account")
	}
	if len(f.trashed) != 1 || f.trashed[0] != "gone.txt" {
		t.Errorf("trashed = %v, want [gone.txt]", f.trashed)
	}
	if !f.has("stay.txt") {
		t.Error("an unrelated file was removed")
	}
}

// A rename must move the node, not re-upload it and trash the original.
func TestLocalRenameBecomesRemoteMove(t *testing.T) {
	f := newFakeDrive()
	f.addFile("old-name.txt", "same content here")

	m, _, root := newTestMirror(t, f, nil)
	mustSyncBoth(t, m)
	f.uploaded, f.trashed, f.moved = nil, nil, nil

	if err := os.Rename(filepath.Join(root, "old-name.txt"), filepath.Join(root, "new-name.txt")); err != nil {
		t.Fatal(err)
	}
	res := mustSyncBoth(t, m)

	if res.Moved != 1 {
		t.Errorf("moved=%d, want 1", res.Moved)
	}
	if len(f.uploaded) != 0 {
		t.Errorf("content was re-uploaded instead of moved: %v", f.uploaded)
	}
	if len(f.trashed) != 0 {
		t.Errorf("original was trashed instead of moved: %v", f.trashed)
	}
	if !f.has("new-name.txt") || f.has("old-name.txt") {
		t.Errorf("remote paths after move: %v", f.files)
	}
	if got := f.content("new-name.txt"); got != "same content here" {
		t.Errorf("content changed through the move: %q", got)
	}
}

func TestMoveIntoSubdirectory(t *testing.T) {
	f := newFakeDrive()
	f.addDir("Archive")
	f.addFile("doc.txt", "archive me")

	m, _, root := newTestMirror(t, f, nil)
	mustSyncBoth(t, m)
	f.uploaded = nil

	if err := os.Rename(filepath.Join(root, "doc.txt"), filepath.Join(root, "Archive", "doc.txt")); err != nil {
		t.Fatal(err)
	}
	res := mustSyncBoth(t, m)

	if res.Moved != 1 {
		t.Errorf("moved=%d, want 1", res.Moved)
	}
	if len(f.uploaded) != 0 {
		t.Errorf("re-uploaded instead of moving: %v", f.uploaded)
	}
	if !f.has("Archive/doc.txt") {
		t.Errorf("file not at its new remote path: %v", f.files)
	}
}

// The most destructive moment in the engine: a download must never silently
// replace local work.
func TestConcurrentEditKeepsBothVersions(t *testing.T) {
	f := newFakeDrive()
	f.addFile("shared.txt", "original\n")

	m, db, root := newTestMirror(t, f, nil)
	mustSyncBoth(t, m)

	// Edit locally.
	writeLocal(t, root, "shared.txt", "MY local edit\n")
	// And remotely, differently.
	f.files["shared.txt"].content = []byte("THEIR remote edit\n")
	f.delta = upsertDelta("link:shared.txt", "link:root")

	res := mustSyncBoth(t, m)

	if res.Conflicts != 1 {
		t.Fatalf("conflicts=%d, want 1", res.Conflicts)
	}

	// The remote version holds the canonical path.
	got, err := os.ReadFile(filepath.Join(root, "shared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "THEIR remote edit\n" {
		t.Errorf("canonical path = %q, want the remote version", got)
	}

	// The local version survives beside it.
	entries, _ := os.ReadDir(root)
	var kept string
	for _, e := range entries {
		if strings.Contains(e.Name(), "conflict") {
			kept = e.Name()
		}
	}
	if kept == "" {
		t.Fatalf("local edit was destroyed; directory holds %v", names(entries))
	}
	keptContent, err := os.ReadFile(filepath.Join(root, kept))
	if err != nil {
		t.Fatal(err)
	}
	if string(keptContent) != "MY local edit\n" {
		t.Errorf("preserved copy = %q, want the local edit", keptContent)
	}

	// Both versions end up in the account.
	if f.content("shared.txt") != "THEIR remote edit\n" {
		t.Errorf("remote canonical = %q", f.content("shared.txt"))
	}
	if !f.has(kept) {
		t.Errorf("the preserved copy was not uploaded; remote holds %v", keys(f.files))
	}

	// And it is recorded for the user to find.
	conflicts, err := db.Conflicts()
	if err != nil || len(conflicts) != 1 {
		t.Errorf("Conflicts() = %v, %v", conflicts, err)
	}
}

// Both sides edited to the same content is agreement, not conflict.
func TestIdenticalEditsOnBothSidesAreNotAConflict(t *testing.T) {
	f := newFakeDrive()
	f.addFile("shared.txt", "original\n")

	m, _, root := newTestMirror(t, f, nil)
	mustSyncBoth(t, m)

	writeLocal(t, root, "shared.txt", "same new content\n")
	f.files["shared.txt"].content = []byte("same new content\n")
	f.delta = upsertDelta("link:shared.txt", "link:root")

	res := mustSyncBoth(t, m)
	if res.Conflicts != 0 {
		t.Errorf("conflicts=%d, want 0 — both sides agree", res.Conflicts)
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.Contains(e.Name(), "conflict") {
			t.Errorf("a conflict copy was created for identical content: %s", e.Name())
		}
	}
}

// Editor scratch files and pDrive's own bookkeeping must never be uploaded.
func TestIgnoredFilesAreNotUploaded(t *testing.T) {
	f := newFakeDrive()
	m, _, root := newTestMirror(t, f, nil)
	mustSyncBoth(t, m)

	for _, name := range []string{
		"real.txt", ".DS_Store", "doc.txt.swp", "download.crdownload",
		".goutputstream-ABC123", "~$report.docx", "backup~",
	} {
		writeLocal(t, root, name, "x")
	}
	mustSyncBoth(t, m)

	if !f.has("real.txt") {
		t.Error("the real file was not uploaded")
	}
	for _, name := range []string{".DS_Store", "doc.txt.swp", "download.crdownload",
		".goutputstream-ABC123", "~$report.docx", "backup~"} {
		if f.has(name) {
			t.Errorf("ignored file %q was uploaded", name)
		}
	}
}

// A symlink must never pull files from outside the sync folder into the
// account.
func TestSymlinksAreNotFollowed(t *testing.T) {
	f := newFakeDrive()
	m, _, root := newTestMirror(t, f, nil)
	mustSyncBoth(t, m)

	secret := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "innocent.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	mustSyncBoth(t, m)

	if f.has("innocent.txt") {
		t.Fatal("a symlink was followed and its target uploaded")
	}
	for path := range f.files {
		if strings.Contains(f.content(path), "PRIVATE KEY") {
			t.Fatalf("content from outside the sync folder reached the account at %q", path)
		}
	}
}

// A stub represents content deliberately not downloaded. Uploading it would
// replace the real file in the account with a placeholder.
func TestStubsAreNeverUploadedOverRealContent(t *testing.T) {
	f := newFakeDrive()
	f.addHugeFile("big.bin", 20<<30)

	m, _, _ := newTestMirror(t, f, func(c *config.Config) { c.Sync.MaxAutoDownloadSize = 1 << 10 })
	res := mustSyncBoth(t, m)

	if res.Stubbed != 1 {
		t.Fatalf("stubbed=%d, want 1", res.Stubbed)
	}
	if res.Uploaded != 0 {
		t.Errorf("uploaded=%d, want 0 — a stub must never be pushed", res.Uploaded)
	}
	if got := f.content("big.bin"); got != "sentinel" {
		t.Errorf("remote content was replaced by a stub: %q", got)
	}
	if f.has("big.bin" + StubSuffix) {
		t.Error("the stub file itself was uploaded")
	}
}

// Deleting most of the folder is stopped before it reaches the account.
func TestMassLocalDeletionIsGuarded(t *testing.T) {
	f := newFakeDrive()
	for i := 0; i < 20; i++ {
		f.addFile(fileName(i), "content")
	}

	m, _, root := newTestMirror(t, f, nil)
	mustSyncBoth(t, m)

	// Remove all but one, so the folder is not empty and the missing-root
	// guard (tested separately below) does not mask this one.
	entries, _ := os.ReadDir(root)
	for i, e := range entries {
		if i == 0 {
			continue
		}
		os.Remove(filepath.Join(root, e.Name()))
	}

	_, err := m.SyncBoth(context.Background())
	if err == nil {
		t.Fatal("deleting 19 of 20 files should have tripped the deletion guard")
	}
	var guard *ErrGuard
	if !asGuard(err, &guard) || guard.Guard != "deletion-cliff" {
		t.Fatalf("error = %v, want a deletion-cliff guard", err)
	}
	if len(f.trashed) != 0 {
		t.Errorf("files were trashed remotely despite the guard: %v", f.trashed)
	}
	if len(f.files) != 20 {
		t.Errorf("account holds %d files, want all 20 intact", len(f.files))
	}
}

// Wiping the folder entirely trips the missing-root guard first: an empty
// sync folder means an unmounted disk far more often than it means the user
// deleted their whole account.
func TestWipedFolderTripsMissingRootBeforeDeleting(t *testing.T) {
	f := newFakeDrive()
	for i := 0; i < 20; i++ {
		f.addFile(fileName(i), "content")
	}

	m, _, root := newTestMirror(t, f, nil)
	mustSyncBoth(t, m)

	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		os.RemoveAll(filepath.Join(root, e.Name()))
	}

	_, err := m.SyncBoth(context.Background())
	var guard *ErrGuard
	if !asGuard(err, &guard) || guard.Guard != "missing-root" {
		t.Fatalf("error = %v, want a missing-root guard", err)
	}
	if len(f.trashed) != 0 || len(f.files) != 20 {
		t.Errorf("the account was modified despite the guard: trashed=%v files=%d",
			f.trashed, len(f.files))
	}
}

func TestRemoteMtimeSurvivesUpload(t *testing.T) {
	f := newFakeDrive()
	m, _, root := newTestMirror(t, f, nil)
	mustSyncBoth(t, m)

	writeLocal(t, root, "dated.txt", "content")
	when := time.Date(2024, 5, 17, 8, 30, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(root, "dated.txt"), when, when); err != nil {
		t.Fatal(err)
	}
	mustSyncBoth(t, m)

	got := f.files["dated.txt"].modified
	if !got.UTC().Truncate(time.Second).Equal(when) {
		t.Errorf("uploaded mtime = %v, want %v", got.UTC(), when)
	}
}

func names(entries []os.DirEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func keys(m map[string]*fakeNode) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func fileName(i int) string {
	return "file" + string(rune('a'+i%26)) + string(rune('0'+i/26)) + ".txt"
}

// A file changed only locally must be uploaded, never pulled over and
// presented as a conflict.
//
// Regression test: the pull stage originally compared local against remote
// and downloaded on any difference, which is a two-way comparison. A local
// edit then looked like a remote change, the user's own edit was "preserved"
// against a version nobody had touched, and every local edit produced a
// spurious conflict copy.
func TestLocalOnlyEditIsNotAConflict(t *testing.T) {
	f := newFakeDrive()
	f.addFileWithDigest("notes.md", "original\n")

	m, _, root := newTestMirror(t, f, nil)
	mustSyncBoth(t, m)

	writeLocal(t, root, "notes.md", "my local edit\n")
	// The remote announces an event for the path (as it does after our own
	// upload) but its content has not changed.
	f.delta = upsertDelta("link:notes.md", "link:root")

	res := mustSyncBoth(t, m)

	if res.Conflicts != 0 {
		t.Errorf("conflicts=%d, want 0 — only the local side changed", res.Conflicts)
	}
	if res.Downloaded != 0 {
		t.Errorf("downloaded=%d, want 0 — the remote did not change", res.Downloaded)
	}
	if res.Uploaded != 1 {
		t.Errorf("uploaded=%d, want 1", res.Uploaded)
	}
	if got := f.content("notes.md"); got != "my local edit\n" {
		t.Errorf("remote content = %q, want the local edit", got)
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.Contains(e.Name(), "conflict") {
			t.Errorf("a conflict copy was created for a local-only edit: %s", e.Name())
		}
	}
}
