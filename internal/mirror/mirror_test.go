package mirror

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YourDoritos/pdrive/internal/config"
	"github.com/YourDoritos/pdrive/internal/drive"
	"github.com/YourDoritos/pdrive/internal/state"
)

func newTestMirror(t *testing.T, f *fakeDrive, tune func(*config.Config)) (*Mirror, *state.DB, string) {
	t.Helper()

	dir := t.TempDir()
	root := filepath.Join(dir, "pdrive")
	trash := filepath.Join(dir, "trash")

	cfg := config.DefaultConfig()
	cfg.Sync.Root = root
	cfg.Validate()
	if tune != nil {
		tune(cfg)
	}

	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	m, err := New(f, db, Options{Config: cfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.trashDir = trash
	if err := os.MkdirAll(trash, 0700); err != nil {
		t.Fatal(err)
	}
	return m, db, root
}

func mustSync(t *testing.T, m *Mirror) *Result {
	t.Helper()
	res, err := m.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return res
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestFullMirrorMaterializesTree(t *testing.T) {
	f := newFakeDrive()
	f.addDir("Documents")
	f.addFile("Documents/plan.txt", "phase one\n")
	f.addFile("notes.md", "hello\n")

	m, db, root := newTestMirror(t, f, nil)
	res := mustSync(t, m)

	if !res.FullMirror {
		t.Error("first sync should be a full mirror")
	}
	if res.Downloaded != 2 || res.Dirs != 1 {
		t.Errorf("downloaded=%d dirs=%d, want 2/1", res.Downloaded, res.Dirs)
	}
	if got := readFile(t, filepath.Join(root, "notes.md")); got != "hello\n" {
		t.Errorf("notes.md = %q", got)
	}
	if got := readFile(t, filepath.Join(root, "Documents", "plan.txt")); got != "phase one\n" {
		t.Errorf("plan.txt = %q", got)
	}

	// The baseline must know about everything it just wrote.
	n, err := db.GetNode("Documents/plan.txt")
	if err != nil || n == nil {
		t.Fatalf("baseline missing plan.txt: %v", err)
	}
	if n.Materialized != state.Materialized || n.ContentHash == "" {
		t.Errorf("baseline entry incomplete: %+v", n)
	}
}

// Proton stores modification times, so the mirror must preserve them rather
// than stamping every file with the download time.
func TestMirrorPreservesModificationTime(t *testing.T) {
	f := newFakeDrive()
	f.addFile("notes.md", "hello\n")
	want := f.files["notes.md"].modified

	m, _, root := newTestMirror(t, f, nil)
	mustSync(t, m)

	info, err := os.Stat(filepath.Join(root, "notes.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().UTC().Truncate(time.Second).Equal(want.Truncate(time.Second)) {
		t.Errorf("mtime = %v, want %v", info.ModTime().UTC(), want)
	}
}

func TestSecondSyncSkipsUnchangedFiles(t *testing.T) {
	f := newFakeDrive()
	f.addFile("notes.md", "hello\n")

	m, _, _ := newTestMirror(t, f, nil)
	mustSync(t, m)

	// No events pending: the second pass replays nothing and re-downloads
	// nothing.
	res := mustSync(t, m)
	if res.FullMirror {
		t.Error("second sync should replay events, not walk the tree")
	}
	if res.Downloaded != 0 {
		t.Errorf("downloaded %d files on an unchanged second pass", res.Downloaded)
	}
	if f.walked != 1 {
		t.Errorf("tree walked %d times, want exactly 1", f.walked)
	}
}

func TestSizeCapWritesStubInsteadOfDownloading(t *testing.T) {
	f := newFakeDrive()
	f.addFile("small.txt", "tiny\n")
	f.addHugeFile("huge.mp4", 20<<30) // 20 GiB

	m, db, root := newTestMirror(t, f, func(c *config.Config) {
		c.Sync.MaxAutoDownloadSize = 1 << 20 // 1 MiB
	})
	res := mustSync(t, m)

	if res.Stubbed != 1 || res.Downloaded != 1 {
		t.Errorf("stubbed=%d downloaded=%d, want 1/1", res.Stubbed, res.Downloaded)
	}

	// The real name must NOT exist — a stub can never be mistaken for content.
	if _, err := os.Stat(filepath.Join(root, "huge.mp4")); !os.IsNotExist(err) {
		t.Error("a stubbed file must not occupy its real filename")
	}

	stub := readFile(t, filepath.Join(root, "huge.mp4"+StubSuffix))
	for _, want := range []string{"pdrive placeholder", "huge.mp4", "pdrive get"} {
		if !strings.Contains(stub, want) {
			t.Errorf("stub does not mention %q:\n%s", want, stub)
		}
	}

	n, _ := db.GetNode("huge.mp4")
	if n == nil || n.Materialized != state.NotMaterialized {
		t.Errorf("baseline should record huge.mp4 as not materialized: %+v", n)
	}
}

func TestMaterializeFetchesStubbedFile(t *testing.T) {
	f := newFakeDrive()
	f.addHugeFile("huge.mp4", 20<<30)

	m, db, root := newTestMirror(t, f, func(c *config.Config) {
		c.Sync.MaxAutoDownloadSize = 1 << 20
	})
	mustSync(t, m)

	if err := m.Materialize(context.Background(), "huge.mp4"); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if got := readFile(t, filepath.Join(root, "huge.mp4")); got != "sentinel" {
		t.Errorf("content = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "huge.mp4"+StubSuffix)); !os.IsNotExist(err) {
		t.Error("the stub should be gone once the real file is downloaded")
	}

	n, _ := db.GetNode("huge.mp4")
	if n == nil || n.Materialized != state.Materialized {
		t.Errorf("baseline should now record huge.mp4 as materialized: %+v", n)
	}
}

// Lowering the size cap must never throw away content already on disk.
func TestLoweringSizeCapKeepsExistingContent(t *testing.T) {
	f := newFakeDrive()
	f.addFile("movie.mkv", "actual content")

	m, _, root := newTestMirror(t, f, nil)
	mustSync(t, m)

	m.cfg.Sync.MaxAutoDownloadSize = 1 // everything is now "too big"
	if _, err := m.FullMirror(context.Background()); err != nil {
		t.Fatalf("FullMirror: %v", err)
	}

	if got := readFile(t, filepath.Join(root, "movie.mkv")); got != "actual content" {
		t.Errorf("existing content was destroyed by a lower size cap: %q", got)
	}
}

// Safety guard 4: a remote deletion must move content to the trash, never
// unlink it.
func TestRemoteDeleteTrashesRatherThanUnlinks(t *testing.T) {
	f := newFakeDrive()
	f.addFile("a.txt", "keep me\n")
	f.addFile("b.txt", "also keep\n")

	m, db, root := newTestMirror(t, f, nil)
	mustSync(t, m)

	delete(f.files, "a.txt")
	f.delta = &drive.Delta{
		Cursor:  "cursor-1",
		Changes: []drive.Change{{Kind: drive.ChangeDelete, LinkID: "link:a.txt"}},
	}

	res := mustSync(t, m)
	if res.Deleted != 1 {
		t.Fatalf("deleted=%d, want 1", res.Deleted)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(err) {
		t.Error("a.txt should be gone from the sync folder")
	}
	if n, _ := db.GetNode("a.txt"); n != nil {
		t.Error("a.txt should be gone from the baseline")
	}

	// It must still exist somewhere under the trash.
	var found string
	filepath.Walk(m.trashDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() && filepath.Base(p) == "a.txt" {
			found = p
		}
		return nil
	})
	if found == "" {
		t.Fatal("deleted file was not preserved in the local trash")
	}
	if got := readFile(t, found); got != "keep me\n" {
		t.Errorf("trashed content = %q, want the original", got)
	}
	// The untouched file must be untouched.
	if got := readFile(t, filepath.Join(root, "b.txt")); got != "also keep\n" {
		t.Errorf("b.txt was disturbed: %q", got)
	}
}

// Safety guard 1: a pass that would delete a large share of everything
// tracked is stopped, and nothing is removed.
func TestDeletionCliffGuardStopsMassDeletion(t *testing.T) {
	f := newFakeDrive()
	var names []string
	for i := 0; i < 20; i++ {
		names = append(names, fmt.Sprintf("file%02d", i))
	}
	for _, name := range names {
		f.addFile(name+".txt", "content "+name)
	}

	m, _, root := newTestMirror(t, f, nil)
	mustSync(t, m)

	// Everything vanishes remotely at once.
	f.files = map[string]*fakeNode{}

	_, err := m.FullMirror(context.Background())
	if err == nil {
		t.Fatal("a total remote wipe should have tripped the deletion guard")
	}
	var guard *ErrGuard
	if !errors.As(err, &guard) || guard.Guard != "deletion-cliff" {
		t.Fatalf("error = %v, want a deletion-cliff guard", err)
	}

	// Crucially: nothing was actually removed.
	for _, name := range names {
		if _, statErr := os.Stat(filepath.Join(root, name+".txt")); statErr != nil {
			t.Errorf("%s.txt was deleted despite the guard firing", name)
		}
	}
}

func TestDeletionCliffCanBeConfirmed(t *testing.T) {
	f := newFakeDrive()
	for i := 0; i < 20; i++ {
		f.addFile(fmt.Sprintf("file%02d.txt", i), "x")
	}

	m, _, _ := newTestMirror(t, f, nil)
	mustSync(t, m)

	f.files = map[string]*fakeNode{}
	m.cfg.Sync.DeletionGuardPercent = 100 // what --confirm-deletions does

	res, err := m.FullMirror(context.Background())
	if err != nil {
		t.Fatalf("confirmed deletion should proceed: %v", err)
	}
	if res.Deleted != 20 {
		t.Errorf("deleted=%d, want 20", res.Deleted)
	}
}

// A handful of deletions on a small account is ordinary activity and must not
// trip the guard, or users learn to bypass it reflexively.
func TestSmallDeletionsDoNotTripTheGuard(t *testing.T) {
	f := newFakeDrive()
	f.addFile("a.txt", "one")
	f.addFile("b.txt", "two")

	m, _, root := newTestMirror(t, f, nil)
	mustSync(t, m)

	delete(f.files, "a.txt")
	if _, err := m.FullMirror(context.Background()); err != nil {
		t.Fatalf("deleting 1 of 2 files should not trip the guard: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(err) {
		t.Error("the deleted file should have been removed")
	}
}

// Safety guard 2: an empty sync root with a populated baseline means the disk
// is not mounted, not that the user deleted everything.
func TestMissingRootGuard(t *testing.T) {
	f := newFakeDrive()
	f.addFile("a.txt", "content")

	m, _, root := newTestMirror(t, f, nil)
	mustSync(t, m)

	// Simulate an unmounted or renamed folder.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}

	_, err := m.Sync(context.Background())
	var guard *ErrGuard
	if !errors.As(err, &guard) || guard.Guard != "missing-root" {
		t.Fatalf("error = %v, want a missing-root guard", err)
	}
}

func TestMissingRootGuardIgnoresEmptyBaseline(t *testing.T) {
	f := newFakeDrive()
	m, _, _ := newTestMirror(t, f, nil)

	// Empty folder, empty baseline: a legitimate first run.
	if _, err := m.Sync(context.Background()); err != nil {
		t.Errorf("first run on an empty account should succeed: %v", err)
	}
}

// Names come from the server. Nothing may escape the sync root.
func TestUnsafeRemoteNamesAreRejected(t *testing.T) {
	f := newFakeDrive()
	f.addFile("../escape.txt", "should never be written")
	f.addFile("ok.txt", "fine")
	f.files["evil"] = &fakeNode{linkID: "link:evil", parentID: "link:root", content: []byte("x")}
	// Rebuild the evil entry under a traversal path.
	delete(f.files, "evil")
	f.addFile("a/../../b.txt", "nope")

	m, _, root := newTestMirror(t, f, nil)
	res := mustSync(t, m)

	if res.Warnings < 2 {
		t.Errorf("warnings=%d, expected the unsafe names to be reported", res.Warnings)
	}
	parent := filepath.Dir(root)
	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); !os.IsNotExist(err) {
		t.Fatal("a traversal name escaped the sync root")
	}
	if _, err := os.Stat(filepath.Join(root, "ok.txt")); err != nil {
		t.Error("the legitimate file should still have synced")
	}
}

func TestValidPath(t *testing.T) {
	bad := []string{
		"", "/abs", "../up", "a/../b", "a//b", "a/./b", "a/\x00/b",
		"thing" + StubSuffix, "dir/thing" + TempSuffix,
	}
	for _, p := range bad {
		if validPath(p) {
			t.Errorf("validPath(%q) = true, want false", p)
		}
	}
	good := []string{"a.txt", "dir/file.txt", "a/b/c/d.bin", "..hidden", "with space.pdf"}
	for _, p := range good {
		if !validPath(p) {
			t.Errorf("validPath(%q) = false, want true", p)
		}
	}
}

// A single unreadable file must not abandon the whole pass.
func TestDownloadFailureIsIsolated(t *testing.T) {
	f := newFakeDrive()
	f.addFile("good.txt", "fine")
	f.addFile("bad.txt", "never read")
	f.failOn["link:bad.txt"] = errors.New("simulated network failure")

	m, _, root := newTestMirror(t, f, nil)
	res, err := m.FullMirror(context.Background())
	if err != nil {
		t.Fatalf("one bad file should not fail the pass: %v", err)
	}
	if res.Downloaded != 1 || res.Warnings != 1 {
		t.Errorf("downloaded=%d warnings=%d, want 1/1", res.Downloaded, res.Warnings)
	}
	if _, statErr := os.Stat(filepath.Join(root, "good.txt")); statErr != nil {
		t.Error("the good file should still have been written")
	}
	// No truncated file left at the real path.
	if _, statErr := os.Stat(filepath.Join(root, "bad.txt")); !os.IsNotExist(statErr) {
		t.Error("a failed download must leave nothing behind")
	}
}

// Proton can tell a client its event history no longer covers its position.
// Ignoring that silently desynchronises the mirror forever.
func TestRefreshFlagTriggersFullMirror(t *testing.T) {
	f := newFakeDrive()
	f.addFile("a.txt", "one")

	m, _, root := newTestMirror(t, f, nil)
	mustSync(t, m)
	if f.walked != 1 {
		t.Fatalf("walks=%d", f.walked)
	}

	f.addFile("b.txt", "two")
	f.refresh = true

	res := mustSync(t, m)
	if !res.FullMirror {
		t.Error("a refresh signal should force a full mirror")
	}
	if f.walked != 2 {
		t.Errorf("walks=%d, want 2", f.walked)
	}
	if _, err := os.Stat(filepath.Join(root, "b.txt")); err != nil {
		t.Error("the refresh should have picked up the new file")
	}
}

// The cursor is taken before the walk, so changes made during a long walk are
// replayed afterwards instead of being lost.
func TestCursorIsAnchoredBeforeWalk(t *testing.T) {
	f := newFakeDrive()
	f.addFile("a.txt", "one")
	f.cursor = "cursor-before"

	m, db, _ := newTestMirror(t, f, nil)
	mustSync(t, m)

	got, err := db.GetMeta(state.KeyEventCursor)
	if err != nil {
		t.Fatal(err)
	}
	if got != "cursor-before" {
		t.Errorf("stored cursor = %q, want the value read before the walk", got)
	}
}

// An event naming a newly created file must bring it down without a walk.
func TestEventCreatesNewFileWithoutWalking(t *testing.T) {
	f := newFakeDrive()
	f.addDir("Docs")
	f.addFile("Docs/one.txt", "first")

	m, _, root := newTestMirror(t, f, nil)
	mustSync(t, m)
	walksAfterFirst := f.walked

	f.addFile("Docs/two.txt", "second")
	f.delta = &drive.Delta{
		Cursor: "cursor-2",
		Changes: []drive.Change{{
			Kind: drive.ChangeUpsert, LinkID: "link:Docs/two.txt", ParentID: "link:Docs",
		}},
	}

	res := mustSync(t, m)
	if res.FullMirror {
		t.Error("an ordinary event must not trigger a tree walk")
	}
	if f.walked != walksAfterFirst {
		t.Errorf("tree was walked again (%d -> %d)", walksAfterFirst, f.walked)
	}
	if got := readFile(t, filepath.Join(root, "Docs", "two.txt")); got != "second" {
		t.Errorf("new file = %q, want %q", got, "second")
	}
}

func TestTrashedRemoteFileIsRemovedLocally(t *testing.T) {
	f := newFakeDrive()
	f.addFile("a.txt", "content")
	f.addFile("b.txt", "content")

	m, _, root := newTestMirror(t, f, nil)
	mustSync(t, m)

	// Proton reports a trashed file as an Update on a non-active link; the
	// drive layer maps that to a delete. Assert the mirror honours it.
	delete(f.files, "a.txt")
	f.delta = &drive.Delta{
		Cursor:  "cursor-3",
		Changes: []drive.Change{{Kind: drive.ChangeDelete, LinkID: "link:a.txt"}},
	}

	mustSync(t, m)
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(err) {
		t.Error("a trashed remote file should not remain in the sync folder")
	}
}

func TestStatsReflectStubsAndContent(t *testing.T) {
	f := newFakeDrive()
	f.addFile("small.txt", "12345")
	f.addHugeFile("huge.bin", 5<<30)

	m, db, _ := newTestMirror(t, f, func(c *config.Config) {
		c.Sync.MaxAutoDownloadSize = 1 << 20
	})
	mustSync(t, m)

	st, err := db.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 2 {
		t.Errorf("files=%d, want 2", st.Files)
	}
	if st.Stubs != 1 {
		t.Errorf("stubs=%d, want 1", st.Stubs)
	}
	if st.MaterialBytes != 5 {
		t.Errorf("material bytes=%d, want 5", st.MaterialBytes)
	}
}

// Regression: delete events for nodes this machine never mirrored must not
// count toward the deletion-cliff guard.
//
// Another device clearing a folder this one never had produced "would remove
// 16 of 5 tracked nodes (320%)" and tripped the guard on a pass that was
// going to delete nothing — blocking all syncing until someone passed
// --confirm-deletions.
func TestUntrackedDeletionsDoNotTripTheGuard(t *testing.T) {
	f := newFakeDrive()
	for i := 0; i < 5; i++ {
		f.addFileWithDigest(fileName(i), "mine")
	}

	m, _, root := newTestMirror(t, f, nil)
	mustSync(t, m)

	// A burst of deletions for links this machine has never seen.
	var changes []drive.Change
	for i := 0; i < 16; i++ {
		changes = append(changes, drive.Change{
			Kind:   drive.ChangeDelete,
			LinkID: fmt.Sprintf("link:never-mirrored-%d.txt", i),
		})
	}
	f.delta = &drive.Delta{Cursor: "cursor-foreign", Changes: changes}

	res, err := m.Sync(context.Background())
	if err != nil {
		t.Fatalf("deletions of untracked nodes tripped the guard: %v", err)
	}
	if res.Deleted != 0 {
		t.Errorf("deleted=%d, want 0 — none of those nodes were here", res.Deleted)
	}
	// Everything this machine does hold must be untouched.
	for i := 0; i < 5; i++ {
		if _, statErr := os.Stat(filepath.Join(root, fileName(i))); statErr != nil {
			t.Errorf("%s was removed: %v", fileName(i), statErr)
		}
	}
}

// A genuine mass deletion of tracked nodes must still be stopped.
func TestTrackedDeletionsStillTripTheGuard(t *testing.T) {
	f := newFakeDrive()
	for i := 0; i < 20; i++ {
		f.addFileWithDigest(fileName(i), "mine")
	}

	m, _, _ := newTestMirror(t, f, nil)
	mustSync(t, m)

	var changes []drive.Change
	for i := 0; i < 20; i++ {
		changes = append(changes, drive.Change{
			Kind: drive.ChangeDelete, LinkID: "link:" + fileName(i),
		})
	}
	f.delta = &drive.Delta{Cursor: "cursor-wipe", Changes: changes}

	if _, err := m.Sync(context.Background()); err == nil {
		t.Fatal("a mass deletion of tracked nodes should still trip the guard")
	}
}
