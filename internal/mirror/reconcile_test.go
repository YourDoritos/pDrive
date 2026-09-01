package mirror

import (
	"testing"
	"time"
)

// The reconciler is the one place where a wrong answer silently destroys
// data, so every combination is enumerated rather than sampled.
func TestReconcileTable(t *testing.T) {
	const (
		A = "aaaa" // one content
		B = "bbbb" // a different content
	)

	cases := []struct {
		name string
		f    Facts
		want Action
	}{
		// ---- nothing anywhere ----
		{"untracked and absent everywhere",
			Facts{}, ActNothing},
		{"tracked but gone from both sides",
			Facts{BaseExists: true, BaseHash: A}, ActDropBaseline},

		// ---- one side only, untracked ----
		{"new local file",
			Facts{LocalExists: true, LocalHash: A}, ActUpload},
		{"new remote file",
			Facts{RemoteExists: true, RemoteHash: A}, ActDownload},
		{"new local directory",
			Facts{LocalExists: true, LocalIsDir: true}, ActMkdirRemote},
		{"new remote directory",
			Facts{RemoteExists: true, RemoteIsDir: true}, ActMkdirLocal},

		// ---- deletions ----
		{"deleted locally, remote unchanged",
			Facts{BaseExists: true, BaseHash: A, RemoteExists: true, RemoteHash: A},
			ActTrashRemote},
		{"deleted remotely, local unchanged",
			Facts{BaseExists: true, BaseHash: A, LocalExists: true, LocalHash: A},
			ActTrashLocal},
		{"directory deleted remotely",
			Facts{BaseExists: true, BaseIsDir: true, LocalExists: true, LocalIsDir: true},
			ActTrashLocal},
		{"directory deleted locally",
			Facts{BaseExists: true, BaseIsDir: true, RemoteExists: true, RemoteIsDir: true},
			ActTrashRemote},

		// ---- principle 1: deletion never beats a modification ----
		{"deleted remotely but edited locally: the edit wins",
			Facts{BaseExists: true, BaseHash: A, LocalExists: true, LocalHash: B},
			ActUpload},
		{"deleted locally but edited remotely: the edit wins",
			Facts{BaseExists: true, BaseHash: A, RemoteExists: true, RemoteHash: B},
			ActDownload},

		// ---- ordinary edits ----
		{"unchanged on both sides",
			Facts{BaseExists: true, BaseHash: A, LocalExists: true, LocalHash: A,
				RemoteExists: true, RemoteHash: A}, ActNothing},
		{"edited locally only",
			Facts{BaseExists: true, BaseHash: A, LocalExists: true, LocalHash: B,
				RemoteExists: true, RemoteHash: A}, ActUpload},
		{"edited remotely only",
			Facts{BaseExists: true, BaseHash: A, LocalExists: true, LocalHash: A,
				RemoteExists: true, RemoteHash: B}, ActDownload},

		// ---- both changed ----
		{"both edited to different content",
			Facts{BaseExists: true, BaseHash: A, LocalExists: true, LocalHash: B,
				RemoteExists: true, RemoteHash: "cccc"}, ActConflict},
		{"both edited to identical content",
			Facts{BaseExists: true, BaseHash: A, LocalExists: true, LocalHash: B,
				RemoteExists: true, RemoteHash: B}, ActAdopt},

		// ---- untracked on both sides ----
		{"appeared on both sides with the same content",
			Facts{LocalExists: true, LocalHash: A, RemoteExists: true, RemoteHash: A},
			ActAdopt},
		{"appeared on both sides with different content",
			Facts{LocalExists: true, LocalHash: A, RemoteExists: true, RemoteHash: B},
			ActConflict},

		// ---- type changes are never guessed ----
		{"file locally, directory remotely",
			Facts{LocalExists: true, LocalHash: A, RemoteExists: true, RemoteIsDir: true},
			ActConflict},
		{"directory locally, file remotely",
			Facts{LocalExists: true, LocalIsDir: true, RemoteExists: true, RemoteHash: A},
			ActConflict},

		// ---- directories present on both sides ----
		{"directory on both sides",
			Facts{BaseExists: true, BaseIsDir: true, LocalExists: true, LocalIsDir: true,
				RemoteExists: true, RemoteIsDir: true}, ActNothing},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Reconcile(c.f)
			if got != c.want {
				t.Errorf("Reconcile() = %s, want %s\nfacts: %+v", got, c.want, c.f)
			}
		})
	}
}

// An unknown hash means "not computed", never "changed". Treating it as a
// change would re-upload the whole account on every pass.
func TestUnknownHashIsNotAChange(t *testing.T) {
	f := Facts{
		BaseExists: true, BaseHash: "aaaa",
		LocalExists: true, LocalHash: "", // not hashed
		RemoteExists: true, RemoteHash: "aaaa",
	}
	if got := Reconcile(f); got != ActNothing {
		t.Errorf("Reconcile with an unhashed local file = %s, want nothing", got)
	}

	f2 := Facts{
		BaseExists: true, BaseHash: "aaaa",
		LocalExists: true, LocalHash: "aaaa",
		RemoteExists: true, RemoteHash: "", // remote digest absent
	}
	if got := Reconcile(f2); got != ActNothing {
		t.Errorf("Reconcile with no remote digest = %s, want nothing", got)
	}
}

func TestHashComparisonIsCaseInsensitive(t *testing.T) {
	// Proton returns SHA1 digests in mixed case in places; a case difference
	// must never read as a content change.
	f := Facts{
		BaseExists: true, BaseHash: "ABCDEF",
		LocalExists: true, LocalHash: "abcdef",
		RemoteExists: true, RemoteHash: "AbCdEf",
	}
	if got := Reconcile(f); got != ActNothing {
		t.Errorf("Reconcile() = %s, want nothing (case-only difference)", got)
	}
}

// No input may ever produce a decision that discards data unreviewed.
func TestNoInputProducesSilentDataLoss(t *testing.T) {
	hashes := []string{"", "aaaa", "bbbb"}
	bools := []bool{false, true}

	for _, baseExists := range bools {
		for _, baseHash := range hashes {
			for _, localExists := range bools {
				for _, localHash := range hashes {
					for _, localDir := range bools {
						for _, remoteExists := range bools {
							for _, remoteHash := range hashes {
								for _, remoteDir := range bools {
									f := Facts{
										BaseExists: baseExists, BaseHash: baseHash,
										LocalExists: localExists, LocalHash: localHash, LocalIsDir: localDir,
										RemoteExists: remoteExists, RemoteHash: remoteHash, RemoteIsDir: remoteDir,
									}
									act := Reconcile(f)

									// Deleting one side requires the other to
									// actually be absent. Anything else would
									// be destroying a file that still exists.
									if act == ActTrashLocal && f.RemoteExists && !f.RemoteIsDir {
										t.Fatalf("would trash a local file while the remote still holds one\n%+v", f)
									}
									if act == ActTrashRemote && f.LocalExists && !f.LocalIsDir {
										t.Fatalf("would trash a remote file while the local one still exists\n%+v", f)
									}
									// A transfer requires a source.
									if act == ActUpload && !f.LocalExists {
										t.Fatalf("would upload a file that does not exist locally\n%+v", f)
									}
									if act == ActDownload && !f.RemoteExists {
										t.Fatalf("would download a file that does not exist remotely\n%+v", f)
									}
								}
							}
						}
					}
				}
			}
		}
	}
}

func TestConflictName(t *testing.T) {
	when := time.Date(2026, 9, 1, 22, 15, 30, 0, time.UTC)

	cases := []struct{ in, want string }{
		{"report.pdf", "report (conflict 2026-09-01 22-15-30 laptop).pdf"},
		{"Docs/plan.txt", "Docs/plan (conflict 2026-09-01 22-15-30 laptop).txt"},
		{"archive.tar.gz", "archive.tar (conflict 2026-09-01 22-15-30 laptop).gz"},
		{"noextension", "noextension (conflict 2026-09-01 22-15-30 laptop)"},
		{".bashrc", ".bashrc (conflict 2026-09-01 22-15-30 laptop)"},
	}
	for _, c := range cases {
		if got := ConflictName(c.in, when, "laptop"); got != c.want {
			t.Errorf("ConflictName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A conflict copy must never itself look like a path that escapes the root.
func TestConflictNameStaysInTheSameDirectory(t *testing.T) {
	got := ConflictName("a/b/c.txt", time.Now(), "host")
	if !validPath(got) {
		t.Errorf("ConflictName produced an invalid path: %q", got)
	}
	if got[:4] != "a/b/" {
		t.Errorf("conflict copy left its directory: %q", got)
	}
}
