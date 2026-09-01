package mirror

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/YourDoritos/pdrive/internal/state"
)

// SyncBoth runs a full bidirectional pass.
//
// Order is load-bearing:
//
//  1. **Scan locally first.** The pull stage writes to disk, so anything not
//     recorded beforehand is indistinguishable afterwards from content that
//     was always there. Scanning first is what lets a download recognise that
//     it is about to overwrite a local edit, and preserve it.
//  2. **Pull.** Remote changes come down; a file changed on both sides
//     becomes a conflict copy rather than an overwrite.
//  3. **Push.** Whatever the pull did not supersede goes up.
func (m *Mirror) SyncBoth(ctx context.Context) (*Result, error) {
	if err := m.checkRoot(); err != nil {
		return nil, err
	}

	local, err := m.Scan()
	if err != nil {
		return nil, err
	}
	// Hash anything whose size or mtime disagrees with the baseline, so the
	// pull stage can tell an edited file from an untouched one.
	for path, n := range local {
		base, err := m.db.GetNode(path)
		if err != nil {
			return nil, err
		}
		if err := m.hashIfNeeded(n, base); err != nil {
			m.emit(Event{Kind: EventWarn, Path: path, Err: err})
		}
	}
	m.localBefore = local

	pull, err := m.pull(ctx)
	if err != nil {
		return nil, err
	}
	m.result = *pull

	// Re-scan: the pull stage created, replaced and removed files.
	local, err = m.Scan()
	if err != nil {
		return nil, err
	}

	if err := m.push(ctx, local); err != nil {
		return nil, err
	}
	return &m.result, nil
}

// pull runs the download half, choosing event replay or a full walk.
func (m *Mirror) pull(ctx context.Context) (*Result, error) {
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
	return m.FullMirror(ctx)
}

// push uploads local changes and propagates local deletions.
func (m *Mirror) push(ctx context.Context, local map[string]*LocalNode) error {
	baseline, err := m.db.AllNodes()
	if err != nil {
		return err
	}
	byPath := make(map[string]state.Node, len(baseline))
	for _, n := range baseline {
		byPath[n.Path] = n
	}

	// Deletions first, so a rename shows up as a move rather than as an
	// upload followed by a delete.
	deleted := m.findLocalDeletions(byPath, local)
	moves := m.detectMoves(byPath, local, deleted)

	remaining := 0
	for _, n := range deleted {
		if _, moved := moves[n.Path]; !moved {
			remaining++
		}
	}
	if err := m.checkDeletionCliff(remaining); err != nil {
		return err
	}

	for from, to := range moves {
		if err := m.applyMove(ctx, byPath[from], to, local[to]); err != nil {
			m.emit(Event{Kind: EventWarn, Path: from, Err: err})
		}
	}
	for _, n := range deleted {
		if _, moved := moves[n.Path]; moved {
			continue
		}
		if err := m.trashRemote(ctx, n); err != nil {
			m.emit(Event{Kind: EventWarn, Path: n.Path, Err: err})
		}
	}

	// Uploads, shallowest first, so a parent folder exists before its
	// children are pushed into it.
	paths := make([]string, 0, len(local))
	for p := range local {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(i, j int) bool {
		di, dj := strings.Count(paths[i], "/"), strings.Count(paths[j], "/")
		if di != dj {
			return di < dj
		}
		return paths[i] < paths[j]
	})

	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, isMoveTarget := m.moveTargets[path]; isMoveTarget {
			continue
		}
		if err := m.pushNode(ctx, path, local[path], byPath); err != nil {
			if isGuard(err) {
				return err
			}
			m.emit(Event{Kind: EventWarn, Path: path, Err: err})
		}
	}
	return nil
}

// pushNode sends one local node up if it needs sending.
func (m *Mirror) pushNode(ctx context.Context, path string, n *LocalNode, byPath map[string]state.Node) error {
	base, tracked := byPath[path]

	if n.IsDir {
		if tracked {
			return nil
		}
		return m.createRemoteDir(ctx, path)
	}

	// A stub stands for content deliberately not downloaded. Uploading it
	// would replace the real file in the account with a placeholder.
	if tracked && base.Materialized == state.NotMaterialized {
		return nil
	}

	if tracked && n.Hash != "" && strings.EqualFold(n.Hash, base.ContentHash) {
		return nil // unchanged
	}
	if n.Hash == "" {
		if err := m.hashIfNeeded(n, nil); err != nil {
			return err
		}
		if tracked && strings.EqualFold(n.Hash, base.ContentHash) {
			return nil
		}
	}

	parentID, err := m.remoteParentID(ctx, path)
	if err != nil {
		return err
	}

	f, err := os.Open(m.localPath(path))
	if err != nil {
		return err
	}
	defer f.Close()

	name := path
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		name = path[idx+1:]
	}

	linkID, err := m.d.Upload(ctx, parentID, name, n.Mtime, f)
	if err != nil {
		return err
	}

	m.result.Uploaded++
	m.result.UploadedBytes += n.Size
	m.emit(Event{Kind: EventUpload, Path: path, Size: n.Size})

	return m.db.PutNode(state.Node{
		Path: path, NodeID: linkID, ParentID: parentID,
		ContentHash: n.Hash, Size: n.Size, LocalMtime: n.Mtime,
		LocalInode: n.Inode, Materialized: state.Materialized, SyncedAt: time.Now(),
	})
}

// createRemoteDir creates a folder remotely for a locally-created directory.
func (m *Mirror) createRemoteDir(ctx context.Context, path string) error {
	parentID, err := m.remoteParentID(ctx, path)
	if err != nil {
		return err
	}
	name := path
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		name = path[idx+1:]
	}

	linkID, err := m.d.Mkdir(ctx, parentID, name)
	if err != nil {
		return err
	}
	m.result.Dirs++
	m.emit(Event{Kind: EventMkdirRemote, Path: path})

	return m.db.PutNode(state.Node{
		Path: path, NodeID: linkID, ParentID: parentID, IsDir: true,
		Materialized: state.Materialized, SyncedAt: time.Now(),
	})
}

// remoteParentID resolves the link ID of a path's parent folder, creating
// intermediate folders remotely when they do not exist yet.
func (m *Mirror) remoteParentID(ctx context.Context, path string) (string, error) {
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return m.d.RootLinkID(), nil
	}
	parent := path[:idx]

	if n, err := m.db.GetNode(parent); err != nil {
		return "", err
	} else if n != nil && n.IsDir {
		return n.NodeID, nil
	}

	// Parent not tracked yet: create it (and its own parents) first.
	if err := m.createRemoteDir(ctx, parent); err != nil {
		return "", err
	}
	n, err := m.db.GetNode(parent)
	if err != nil {
		return "", err
	}
	if n == nil {
		return "", fmt.Errorf("could not resolve remote parent for %q", path)
	}
	return n.NodeID, nil
}

// findLocalDeletions returns baseline entries with no local counterpart.
func (m *Mirror) findLocalDeletions(byPath map[string]state.Node, local map[string]*LocalNode) []state.Node {
	var gone []state.Node
	for path, n := range byPath {
		if _, ok := local[path]; ok {
			continue
		}
		// A stub is the on-disk representation of a non-materialized file.
		// Its absence at the real path is expected, not a deletion.
		if n.Materialized == state.NotMaterialized {
			if _, ok := local[path+StubSuffix]; ok {
				continue
			}
			if _, err := os.Lstat(m.localPath(path) + StubSuffix); err == nil {
				continue
			}
		}
		gone = append(gone, n)
	}
	sort.Slice(gone, func(i, j int) bool { return len(gone[i].Path) > len(gone[j].Path) })
	return gone
}

// detectMoves matches disappeared baseline entries against new untracked
// local files, by inode first and content hash second.
//
// Without this, moving a 4 GB file to another folder would re-upload all of
// it and trash the original.
func (m *Mirror) detectMoves(byPath map[string]state.Node, local map[string]*LocalNode, deleted []state.Node) map[string]string {
	moves := map[string]string{}
	m.moveTargets = map[string]bool{}

	// Candidate destinations: local files with no baseline entry.
	var candidates []*LocalNode
	for path, n := range local {
		if _, tracked := byPath[path]; tracked {
			continue
		}
		if n.IsDir {
			continue
		}
		candidates = append(candidates, n)
	}
	if len(candidates) == 0 {
		return moves
	}

	claimed := map[string]bool{}
	for _, old := range deleted {
		if old.IsDir {
			continue
		}
		for _, cand := range candidates {
			if claimed[cand.Path] {
				continue
			}
			match := false
			switch {
			case old.LocalInode != 0 && cand.Inode == old.LocalInode:
				match = true
			case old.ContentHash != "" && cand.Size == old.Size:
				if cand.Hash == "" {
					_ = m.hashIfNeeded(cand, nil)
				}
				match = strings.EqualFold(cand.Hash, old.ContentHash)
			}
			if match {
				moves[old.Path] = cand.Path
				claimed[cand.Path] = true
				m.moveTargets[cand.Path] = true
				break
			}
		}
	}
	return moves
}

// applyMove renames or relocates a node remotely instead of re-uploading it.
func (m *Mirror) applyMove(ctx context.Context, old state.Node, newPath string, n *LocalNode) error {
	parentID, err := m.remoteParentID(ctx, newPath)
	if err != nil {
		return err
	}
	name := newPath
	if idx := strings.LastIndex(newPath, "/"); idx >= 0 {
		name = newPath[idx+1:]
	}

	if err := m.d.Move(ctx, old.NodeID, parentID, name, old.IsDir); err != nil {
		return err
	}
	if err := m.db.DeleteNode(old.Path); err != nil {
		return err
	}

	m.result.Moved++
	m.emit(Event{Kind: EventMove, Path: old.Path + " -> " + newPath})

	moved := old
	moved.Path = newPath
	moved.ParentID = parentID
	moved.SyncedAt = time.Now()
	if n != nil {
		moved.LocalInode = n.Inode
		moved.LocalMtime = n.Mtime
		moved.Size = n.Size
		if n.Hash != "" {
			moved.ContentHash = n.Hash
		}
	}
	return m.db.PutNode(moved)
}

// trashRemote propagates a local deletion to Proton's trash.
func (m *Mirror) trashRemote(ctx context.Context, n state.Node) error {
	if err := m.d.Trash(ctx, n.NodeID, n.IsDir); err != nil {
		return err
	}
	if n.IsDir {
		if err := m.db.DeleteSubtree(n.Path); err != nil {
			return err
		}
	} else if err := m.db.DeleteNode(n.Path); err != nil {
		return err
	}

	m.result.TrashedRemote++
	m.emit(Event{Kind: EventTrashRemote, Path: n.Path})
	return nil
}

func isGuard(err error) bool {
	var g *ErrGuard
	return err != nil && asGuard(err, &g)
}

func asGuard(err error, target **ErrGuard) bool {
	if g, ok := err.(*ErrGuard); ok {
		*target = g
		return true
	}
	return false
}
