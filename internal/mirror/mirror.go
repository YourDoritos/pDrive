// Package mirror brings the local sync folder into agreement with Proton Drive.
//
// Phase 1 is deliberately one-directional: it reads from Drive and writes
// locally, and calls nothing that mutates the account. Local changes are not
// propagated yet. That asymmetry is a feature while the reconciler is young —
// the worst a bug here can do is churn local files, all of which are
// recoverable from the trash directory.
package mirror

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/YourDoritos/pdrive/internal/config"
	"github.com/YourDoritos/pdrive/internal/drive"
	"github.com/YourDoritos/pdrive/internal/state"
)

// EventKind classifies a progress notification.
type EventKind int

const (
	// EventDir reports a directory created locally.
	EventDir EventKind = iota
	// EventDownload reports a file downloaded.
	EventDownload
	// EventStub reports a file left as a placeholder because of the size cap.
	EventStub
	// EventSkip reports a file already present and up to date.
	EventSkip
	// EventDelete reports a file removed locally after a remote deletion.
	EventDelete
	// EventWarn reports a non-fatal problem.
	EventWarn
	// EventUpload reports a file sent to Proton Drive.
	EventUpload
	// EventMkdirRemote reports a folder created remotely.
	EventMkdirRemote
	// EventMove reports a node moved or renamed remotely instead of
	// re-uploaded.
	EventMove
	// EventTrashRemote reports a node moved to Proton's trash after being
	// deleted locally.
	EventTrashRemote
	// EventConflict reports that both sides changed and both versions were
	// kept.
	EventConflict
)

// Event is a progress notification.
type Event struct {
	Kind EventKind
	Path string
	Size int64
	Err  error
}

// Result summarises a pass.
type Result struct {
	Dirs       int
	Downloaded int
	Stubbed    int
	Skipped    int
	Deleted    int
	Warnings   int
	Bytes      int64

	Uploaded      int
	UploadedBytes int64
	Moved         int
	TrashedRemote int
	Conflicts     int

	FullMirror  bool
	CursorMoved bool
}

// Mirror reconciles Proton Drive into a local folder.
type Mirror struct {
	d   Source
	db  *state.DB
	cfg *config.Config

	root     string
	trashDir string

	progress func(Event)
	result   Result

	// localBefore is the local scan taken before the pull stage, so a
	// download can recognise that it is about to overwrite a local edit.
	localBefore map[string]*LocalNode
	// moveTargets are local paths already accounted for as the destination
	// of a move, so the push stage does not also upload them.
	moveTargets map[string]bool

	// hostname names this machine in conflict copies.
	hostname string
}

// Options configures a Mirror.
type Options struct {
	Config   *config.Config
	Progress func(Event)
	// ConfirmDeletions bypasses the deletion cliff guard for one pass. Only
	// set it when a human has been shown what would be removed.
	ConfirmDeletions bool
}

// New creates a Mirror over a remote Source and an open state DB.
func New(d Source, db *state.DB, opts Options) (*Mirror, error) {
	cfg := opts.Config
	if cfg == nil {
		cfg = config.DefaultConfig()
		cfg.Validate()
	}
	if opts.ConfirmDeletions {
		cfg.Sync.DeletionGuardPercent = 100
	}

	m := &Mirror{
		d:        d,
		db:       db,
		cfg:      cfg,
		root:     cfg.SyncRoot(),
		trashDir: config.TrashDir(),
		progress: opts.Progress,
	}
	if err := os.MkdirAll(m.root, 0700); err != nil {
		return nil, fmt.Errorf("create sync root: %w", err)
	}
	if err := os.MkdirAll(m.trashDir, 0700); err != nil {
		return nil, fmt.Errorf("create trash directory: %w", err)
	}
	m.hostname, _ = os.Hostname()
	m.moveTargets = map[string]bool{}
	return m, nil
}

func (m *Mirror) emit(ev Event) {
	if ev.Kind == EventWarn {
		m.result.Warnings++
	}
	if m.progress != nil {
		m.progress(ev)
	}
}

// Sync brings the folder up to date, choosing the cheapest correct strategy:
// a full mirror on first run or when Proton asks for a refresh, an event
// replay otherwise.
func (m *Mirror) Sync(ctx context.Context) (*Result, error) {
	if err := m.checkRoot(); err != nil {
		return nil, err
	}

	cursor, err := m.db.GetMeta(state.KeyEventCursor)
	if err != nil {
		return nil, err
	}
	if cursor == "" {
		return m.FullMirror(ctx)
	}

	res, err := m.applyEvents(ctx, cursor)
	if err != nil {
		return nil, err
	}
	if res != nil {
		return res, nil
	}
	// Proton asked for a refresh: its event history no longer covers our
	// position, so the only correct move is to rebuild from the tree.
	return m.FullMirror(ctx)
}

// FullMirror walks the entire Drive tree and reconciles it locally.
//
// Reserved for first run and for a server-requested refresh. Steady state
// uses the event cursor; the integration rules forbid habitual tree walks.
func (m *Mirror) FullMirror(ctx context.Context) (*Result, error) {
	m.result = Result{FullMirror: true}

	// Take the cursor BEFORE walking. Anything that changes during the walk
	// then shows up in the next poll. Taking it afterwards would silently
	// drop every change made while the walk was running.
	cursor, err := m.d.LatestEventID(ctx)
	if err != nil {
		return nil, err
	}

	if err := m.db.SetMeta(state.KeyVolumeID, m.d.VolumeID()); err != nil {
		return nil, err
	}
	if err := m.db.SetMeta(state.KeyShareID, m.d.ShareID()); err != nil {
		return nil, err
	}
	if err := m.db.SetMeta(state.KeyRootLinkID, m.d.RootLinkID()); err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	err = m.d.Walk(ctx, func(n drive.Node) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !validPath(n.Path) {
			m.emit(Event{Kind: EventWarn, Path: n.Path,
				Err: fmt.Errorf("unsafe name from server, skipped")})
			return nil
		}
		seen[n.Path] = true

		// One unreadable file must not abandon an otherwise good pass. Real
		// errors surface as warnings and in the result's warning count;
		// cancellation still stops everything.
		if err := m.applyNode(ctx, n); err != nil {
			if ctx.Err() != nil {
				return err
			}
			m.emit(Event{Kind: EventWarn, Path: n.Path, Err: err})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Anything still in the baseline that the walk did not see is gone
	// remotely. Route it through the same guard as event-driven deletions.
	if err := m.reapUnseen(seen); err != nil {
		return nil, err
	}

	if err := m.db.SetMeta(state.KeyEventCursor, cursor); err != nil {
		return nil, err
	}
	if err := m.db.SetMeta(state.KeyLastFullScan, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return nil, err
	}
	m.result.CursorMoved = true
	return &m.result, nil
}

// applyEvents replays the event stream. Returns (nil, nil) when Proton signals
// that a full refresh is required.
func (m *Mirror) applyEvents(ctx context.Context, cursor string) (*Result, error) {
	m.result = Result{}

	delta, err := m.d.PollEvents(ctx, cursor)
	if err != nil {
		return nil, err
	}
	if delta.Refresh {
		return nil, nil
	}

	// Group by parent directory. One event per changed file would mean one
	// API call per file; re-listing each affected directory once collapses a
	// busy batch into a handful of calls and gets correctly decrypted names
	// for free.
	dirsToRelist := map[string]bool{}
	var deletions []drive.Change

	for _, c := range delta.Changes {
		switch c.Kind {
		case ChangeDeleteKind:
			deletions = append(deletions, c)
		default:
			parent := c.ParentID
			if parent == "" && c.LinkID != "" {
				// No parent in the payload. Resolving it costs one call and
				// is the difference between the change arriving and being
				// dropped without trace.
				resolved, err := m.d.LinkParent(ctx, c.LinkID)
				if err != nil {
					m.emit(Event{Kind: EventWarn, Err: err})
					continue
				}
				parent = resolved
			}
			if parent != "" {
				dirsToRelist[parent] = true
			}
		}
	}

	if err := m.checkDeletionCliff(len(deletions)); err != nil {
		return nil, err
	}

	for _, c := range deletions {
		if err := m.deleteByNodeID(c.LinkID); err != nil {
			m.emit(Event{Kind: EventWarn, Err: err})
		}
	}

	for parentID := range dirsToRelist {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := m.relistDir(ctx, parentID); err != nil {
			m.emit(Event{Kind: EventWarn, Err: err})
		}
	}

	if delta.Cursor != "" && delta.Cursor != cursor {
		if err := m.db.SetMeta(state.KeyEventCursor, delta.Cursor); err != nil {
			return nil, err
		}
		m.result.CursorMoved = true
	}
	return &m.result, nil
}

// ChangeDeleteKind aliases drive.ChangeDelete for readability above.
const ChangeDeleteKind = drive.ChangeDelete

// relistDir re-reads one directory and reconciles it against the baseline.
func (m *Mirror) relistDir(ctx context.Context, linkID string) error {
	prefix := ""
	if linkID != m.d.RootLinkID() {
		node, err := m.db.GetNodeByID(linkID)
		if err != nil {
			return err
		}
		if node == nil {
			// A directory we have never seen: its own creation event will
			// have named its parent, so it gets picked up there.
			return nil
		}
		prefix = node.Path
	}

	children, err := m.d.ListDir(ctx, linkID, prefix)
	if err != nil {
		return err
	}

	seen := map[string]bool{}
	for _, n := range children {
		if !validPath(n.Path) {
			m.emit(Event{Kind: EventWarn, Path: n.Path,
				Err: fmt.Errorf("unsafe name from server, skipped")})
			continue
		}
		seen[n.Path] = true
		if err := m.applyNode(ctx, n); err != nil {
			m.emit(Event{Kind: EventWarn, Path: n.Path, Err: err})
		}
	}

	// Entries the baseline has under this directory that are no longer
	// present remotely.
	all, err := m.db.AllNodes()
	if err != nil {
		return err
	}
	var stale []state.Node
	for _, node := range all {
		if node.ParentID != linkID || seen[node.Path] {
			continue
		}
		stale = append(stale, node)
	}
	if err := m.checkDeletionCliff(len(stale)); err != nil {
		return err
	}
	for _, node := range stale {
		if err := m.deleteNode(node); err != nil {
			m.emit(Event{Kind: EventWarn, Path: node.Path, Err: err})
		}
	}
	return nil
}

// applyNode makes one remote node true locally.
func (m *Mirror) applyNode(ctx context.Context, n drive.Node) error {
	local := m.localPath(n.Path)

	if n.IsDir {
		if err := os.MkdirAll(local, 0700); err != nil {
			return fmt.Errorf("create %s: %w", n.Path, err)
		}
		m.result.Dirs++
		m.emit(Event{Kind: EventDir, Path: n.Path})
		return m.db.PutNode(state.Node{
			Path: n.Path, NodeID: n.LinkID, ParentID: n.ParentID, IsDir: true,
			Materialized: state.Materialized, SyncedAt: time.Now(),
		})
	}

	want := m.materializedFor(n.Size)
	base, err := m.db.GetNode(n.Path)
	if err != nil {
		return err
	}

	if want == state.NotMaterialized {
		return m.writeStubFor(n)
	}

	// Has the remote actually changed since we last agreed?
	//
	// This check is what makes the pull stage part of a three-way
	// reconciliation rather than a two-way one. Without it, a file edited
	// only locally looks "different from the remote" and gets pulled over,
	// which then presents as a conflict against the user's own edit. The
	// remote is the side that must have moved for a download to be correct;
	// a purely local difference belongs to the push stage.
	if base != nil && base.ContentHash != "" && n.Digest != "" &&
		strings.EqualFold(base.ContentHash, n.Digest) {
		m.result.Skipped++
		m.emit(Event{Kind: EventSkip, Path: n.Path})
		return nil
	}

	// Already correct on disk? Trust size plus mtime, and fall back to a hash
	// when the metadata is ambiguous. Proton's own digest, when present, is
	// the strongest signal available.
	if base != nil && base.Materialized == state.Materialized {
		if info, statErr := os.Stat(local); statErr == nil && !info.IsDir() {
			if m.upToDate(base, n, info, local) {
				m.result.Skipped++
				m.emit(Event{Kind: EventSkip, Path: n.Path, Size: info.Size()})
				return nil
			}
		}
	}

	rc, _, err := m.d.Download(ctx, n.LinkID)
	if err != nil {
		return err
	}
	defer rc.Close()

	// Stage first. Holding both versions at once turns conflict detection
	// into an exact hash comparison instead of an inference from metadata.
	tmp, sum, written, err := stageDownload(local, rc)
	if err != nil {
		return err
	}

	if n.Digest != "" && !strings.EqualFold(sum, n.Digest) {
		m.emit(Event{Kind: EventWarn, Path: n.Path,
			Err: fmt.Errorf("content hash %s disagrees with Proton's digest %s", sum, n.Digest)})
	}

	// Only now, knowing exactly what is arriving, decide whether the file
	// about to be replaced holds work that exists nowhere else.
	if err := m.preserveLocalIfConflicting(n.Path, sum); err != nil {
		os.Remove(tmp)
		return err
	}

	// A stub may be standing where the real file now belongs.
	_ = os.Remove(local + StubSuffix)

	if err := commitStaged(tmp, local, n.Modified); err != nil {
		return err
	}

	info, err := os.Stat(local)
	if err != nil {
		return err
	}

	m.result.Downloaded++
	m.result.Bytes += written
	m.emit(Event{Kind: EventDownload, Path: n.Path, Size: written})

	return m.db.PutNode(state.Node{
		Path: n.Path, NodeID: n.LinkID, ParentID: n.ParentID,
		ContentHash: sum, Size: written, LocalMtime: info.ModTime(),
		LocalInode: inodeOf(info), Materialized: state.Materialized, SyncedAt: time.Now(),
	})
}

// upToDate reports whether the local file already matches the remote node.
func (m *Mirror) upToDate(base *state.Node, n drive.Node, info os.FileInfo, local string) bool {
	if n.Size > 0 && info.Size() != n.Size {
		return false
	}
	// Proton preserves modification times, so a matching mtime and size is a
	// strong signal. Verified in Phase 0.5 against a live account.
	if !n.Modified.IsZero() && !info.ModTime().Truncate(time.Second).Equal(n.Modified.Truncate(time.Second)) {
		return false
	}
	if n.Digest != "" {
		sum, err := hashFile(local)
		if err != nil {
			return false
		}
		return strings.EqualFold(sum, n.Digest)
	}
	return base.ContentHash != "" && info.Size() == base.Size
}

// writeStubFor records a file that is intentionally not downloaded.
func (m *Mirror) writeStubFor(n drive.Node) error {
	local := m.localPath(n.Path)

	// If the real file is already here, leave it. Lowering the size cap must
	// never throw away content the user already has.
	if info, err := os.Stat(local); err == nil && !info.IsDir() {
		m.result.Skipped++
		m.emit(Event{Kind: EventSkip, Path: n.Path, Size: info.Size()})
		return nil
	}

	if err := writeStub(local+StubSuffix, stubInfo{
		Name: n.Name, Path: n.Path, Size: n.Size, Modified: n.Modified, LinkID: n.LinkID,
	}); err != nil {
		return err
	}

	m.result.Stubbed++
	m.emit(Event{Kind: EventStub, Path: n.Path, Size: n.Size})

	return m.db.PutNode(state.Node{
		Path: n.Path, NodeID: n.LinkID, ParentID: n.ParentID,
		Size: n.Size, LocalMtime: n.Modified,
		Materialized: state.NotMaterialized, SyncedAt: time.Now(),
	})
}

// Materialize downloads a file that is currently a stub.
func (m *Mirror) Materialize(ctx context.Context, path string) error {
	node, err := m.db.GetNode(path)
	if err != nil {
		return err
	}
	if node == nil {
		return fmt.Errorf("%s is not in the mirror", path)
	}
	if node.IsDir {
		return fmt.Errorf("%s is a directory", path)
	}
	if node.Materialized == state.Materialized {
		return fmt.Errorf("%s is already downloaded", path)
	}

	local := m.localPath(path)
	rc, _, err := m.d.Download(ctx, node.NodeID)
	if err != nil {
		return err
	}
	defer rc.Close()

	tmp, sum, written, err := stageDownload(local, rc)
	if err != nil {
		return err
	}
	if err := commitStaged(tmp, local, node.LocalMtime); err != nil {
		return err
	}
	_ = os.Remove(local + StubSuffix)

	info, err := os.Stat(local)
	if err != nil {
		return err
	}

	m.result.Downloaded++
	m.result.Bytes += written
	m.emit(Event{Kind: EventDownload, Path: path, Size: written})

	node.ContentHash = sum
	node.Size = written
	node.LocalMtime = info.ModTime()
	node.LocalInode = inodeOf(info)
	node.Materialized = state.Materialized
	node.SyncedAt = time.Now()
	return m.db.PutNode(*node)
}

// reapUnseen removes baseline entries a full walk did not encounter.
func (m *Mirror) reapUnseen(seen map[string]bool) error {
	all, err := m.db.AllNodes()
	if err != nil {
		return err
	}

	var gone []state.Node
	for _, n := range all {
		if !seen[n.Path] {
			gone = append(gone, n)
		}
	}
	if err := m.checkDeletionCliff(len(gone)); err != nil {
		return err
	}

	// Deepest first, so directories are empty by the time they are removed.
	sort.Slice(gone, func(i, j int) bool { return len(gone[i].Path) > len(gone[j].Path) })
	for _, n := range gone {
		if err := m.deleteNode(n); err != nil {
			m.emit(Event{Kind: EventWarn, Path: n.Path, Err: err})
		}
	}
	return nil
}

func (m *Mirror) deleteByNodeID(nodeID string) error {
	node, err := m.db.GetNodeByID(nodeID)
	if err != nil {
		return err
	}
	if node == nil {
		return nil // never mirrored; nothing to do
	}
	return m.deleteNode(*node)
}

// deleteNode removes a node locally after it disappeared remotely. It moves
// content to the local trash rather than unlinking it (safety guard 4).
func (m *Mirror) deleteNode(n state.Node) error {
	local := m.localPath(n.Path)

	for _, candidate := range []string{local, local + StubSuffix} {
		if _, err := os.Lstat(candidate); err != nil {
			continue
		}
		if err := trashLocal(m.trashDir, m.root, candidate); err != nil {
			return fmt.Errorf("trash %s: %w", n.Path, err)
		}
	}

	if n.IsDir {
		if err := m.db.DeleteSubtree(n.Path); err != nil {
			return err
		}
	} else if err := m.db.DeleteNode(n.Path); err != nil {
		return err
	}

	removeEmptyParents(m.root, local)
	m.result.Deleted++
	m.emit(Event{Kind: EventDelete, Path: n.Path})
	return nil
}

func (m *Mirror) localPath(p string) string {
	return filepath.Join(m.root, filepath.FromSlash(p))
}

// validPath rejects any server-supplied path that could escape the sync root.
// Names are chosen by whoever can write to the account, so they are validated
// rather than trusted.
func validPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsRune(part, 0) {
			return false
		}
		if strings.HasSuffix(part, StubSuffix) || strings.HasSuffix(part, TempSuffix) {
			// Would collide with pDrive's own bookkeeping files.
			return false
		}
	}
	return true
}
