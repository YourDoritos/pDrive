package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenCreatesSecureFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0077 != 0 {
		t.Errorf("state db mode = %o, want no group/other access", perm)
	}
}

func TestSchemaVersionRecorded(t *testing.T) {
	db := newTestDB(t)
	v, err := db.GetMetaInt(KeySchemaVersion)
	if err != nil {
		t.Fatal(err)
	}
	if v != SchemaVersion {
		t.Errorf("schema version = %d, want %d", v, SchemaVersion)
	}
}

// Opening a database written by a newer pdrive must fail loudly rather than
// operate on a schema it does not understand.
func TestRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetMetaInt(KeySchemaVersion, SchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if _, err := Open(path); err == nil {
		t.Fatal("expected Open to refuse a newer schema version")
	}
}

func TestMetaRoundTrip(t *testing.T) {
	db := newTestDB(t)

	if v, err := db.GetMeta("nope"); err != nil || v != "" {
		t.Errorf("missing key = (%q, %v), want empty and no error", v, err)
	}
	if err := db.SetMeta(KeyEventCursor, "cursor-abc"); err != nil {
		t.Fatal(err)
	}
	if v, _ := db.GetMeta(KeyEventCursor); v != "cursor-abc" {
		t.Errorf("cursor = %q", v)
	}
	// Overwrite.
	if err := db.SetMeta(KeyEventCursor, "cursor-def"); err != nil {
		t.Fatal(err)
	}
	if v, _ := db.GetMeta(KeyEventCursor); v != "cursor-def" {
		t.Errorf("cursor after overwrite = %q", v)
	}
}

func TestNodeRoundTrip(t *testing.T) {
	db := newTestDB(t)

	mtime := time.Unix(1770000000, 0)
	want := Node{
		Path: "Documents/plan.txt", NodeID: "link-1", ParentID: "link-dir",
		RevisionID: "rev-1", ContentHash: "abc123", Size: 4096,
		LocalMtime: mtime, LocalInode: 987654, Materialized: Materialized,
	}
	if err := db.PutNode(want); err != nil {
		t.Fatalf("PutNode: %v", err)
	}

	got, err := db.GetNode(want.Path)
	if err != nil || got == nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got.NodeID != want.NodeID || got.ContentHash != want.ContentHash ||
		got.Size != want.Size || got.LocalInode != want.LocalInode {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", *got, want)
	}
	if !got.LocalMtime.Equal(mtime) {
		t.Errorf("mtime = %v, want %v", got.LocalMtime, mtime)
	}

	byID, err := db.GetNodeByID("link-1")
	if err != nil || byID == nil || byID.Path != want.Path {
		t.Errorf("GetNodeByID = %v, %v", byID, err)
	}
}

func TestPutNodeUpserts(t *testing.T) {
	db := newTestDB(t)

	if err := db.PutNode(Node{Path: "a.txt", NodeID: "l1", Size: 10}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutNode(Node{Path: "a.txt", NodeID: "l1", Size: 20, ContentHash: "new"}); err != nil {
		t.Fatal(err)
	}

	n, _ := db.GetNode("a.txt")
	if n == nil || n.Size != 20 || n.ContentHash != "new" {
		t.Errorf("upsert did not replace the row: %+v", n)
	}
	if count, _ := db.CountNodes(); count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
}

func TestGetMissingNode(t *testing.T) {
	db := newTestDB(t)
	n, err := db.GetNode("nothing")
	if err != nil {
		t.Errorf("GetNode on a missing path returned an error: %v", err)
	}
	if n != nil {
		t.Errorf("GetNode on a missing path = %+v, want nil", n)
	}
}

func TestDeleteSubtree(t *testing.T) {
	db := newTestDB(t)

	for _, p := range []string{"Docs", "Docs/a.txt", "Docs/sub", "Docs/sub/b.txt", "Other", "Other/c.txt"} {
		if err := db.PutNode(Node{Path: p, NodeID: "l:" + p}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.DeleteSubtree("Docs"); err != nil {
		t.Fatal(err)
	}

	all, err := db.AllNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("remaining = %d, want 2", len(all))
	}
	for _, n := range all {
		if n.Path != "Other" && n.Path != "Other/c.txt" {
			t.Errorf("unexpected surviving node %q", n.Path)
		}
	}
}

// A path containing LIKE wildcards must not sweep away unrelated rows.
func TestDeleteSubtreeEscapesWildcards(t *testing.T) {
	db := newTestDB(t)

	for _, p := range []string{"100%", "100%/inside.txt", "100X", "100X/keep.txt", "a_b", "axb"} {
		if err := db.PutNode(Node{Path: p, NodeID: "l:" + p}); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.DeleteSubtree("100%"); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.GetNode("100X"); n == nil {
		t.Error("deleting \"100%\" also removed \"100X\" — LIKE wildcard leaked")
	}
	if n, _ := db.GetNode("100X/keep.txt"); n == nil {
		t.Error("deleting \"100%\" also removed \"100X/keep.txt\"")
	}
	if n, _ := db.GetNode("100%/inside.txt"); n != nil {
		t.Error("the real subtree was not removed")
	}

	if err := db.DeleteSubtree("a_b"); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.GetNode("axb"); n == nil {
		t.Error("deleting \"a_b\" also removed \"axb\" — underscore wildcard leaked")
	}
}

func TestStats(t *testing.T) {
	db := newTestDB(t)

	if err := db.PutNode(Node{Path: "d", NodeID: "l1", IsDir: true, Materialized: Materialized}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutNode(Node{Path: "d/a.txt", NodeID: "l2", Size: 100, Materialized: Materialized}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutNode(Node{Path: "big.bin", NodeID: "l3", Size: 900, Materialized: NotMaterialized}); err != nil {
		t.Fatal(err)
	}

	st, err := db.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 2 || st.Dirs != 1 || st.Stubs != 1 {
		t.Errorf("files/dirs/stubs = %d/%d/%d, want 2/1/1", st.Files, st.Dirs, st.Stubs)
	}
	if st.Bytes != 1000 || st.MaterialBytes != 100 {
		t.Errorf("bytes/material = %d/%d, want 1000/100", st.Bytes, st.MaterialBytes)
	}
}

func TestStatsOnEmptyDB(t *testing.T) {
	db := newTestDB(t)
	st, err := db.Stats()
	if err != nil {
		t.Fatalf("Stats on an empty database: %v", err)
	}
	if st.Files != 0 || st.Bytes != 0 {
		t.Errorf("empty stats = %+v", st)
	}
}

func TestReopenPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutNode(Node{Path: "a.txt", NodeID: "l1", ContentHash: "hash"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta(KeyEventCursor, "c1"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	n, _ := db2.GetNode("a.txt")
	if n == nil || n.ContentHash != "hash" {
		t.Errorf("node did not survive reopen: %+v", n)
	}
	if v, _ := db2.GetMeta(KeyEventCursor); v != "c1" {
		t.Errorf("cursor did not survive reopen: %q", v)
	}
}
