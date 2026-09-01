package mirror

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/YourDoritos/pdrive/internal/state"
)

// LocalNode is one entry found by scanning the sync folder.
type LocalNode struct {
	Path  string
	IsDir bool
	Size  int64
	Mtime time.Time
	Inode uint64

	// Hash is computed lazily: only when size or mtime disagree with the
	// baseline. Empty means "not computed", never "empty file".
	Hash string
}

// ignoredNames are pDrive's own bookkeeping and the scratch files editors and
// browsers leave lying around. Uploading these produces conflicts and churn
// and never anything the user wanted.
var ignoredNames = []string{
	".pdrive-tmp",
	".DS_Store",
	"Thumbs.db",
}

var ignoredSuffixes = []string{
	StubSuffix,
	TempSuffix,
	".part",
	".crdownload",
	".swp",
	".swx",
	"~",
}

var ignoredPrefixes = []string{
	".goutputstream-",
	"~$",
	".~lock.",
}

// Ignored reports whether a path component should be skipped entirely.
func Ignored(name string) bool {
	for _, n := range ignoredNames {
		if name == n {
			return true
		}
	}
	for _, s := range ignoredSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	for _, p := range ignoredPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Scan walks the sync folder and returns everything in it, keyed by
// slash-separated path relative to the root.
//
// Hashing is deferred: a file is only read when its size or modification time
// disagrees with the baseline. Proton preserves modification times (verified
// in Phase 0.5), so this fast path is reliable and keeps a steady-state scan
// off the disk almost entirely.
func (m *Mirror) Scan() (map[string]*LocalNode, error) {
	out := map[string]*LocalNode{}

	err := filepath.WalkDir(m.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory is worth reporting, but it must not
			// abort the scan and thereby make everything below it look
			// deleted.
			m.emit(Event{Kind: EventWarn, Path: path, Err: err})
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path == m.root {
			return nil
		}
		if Ignored(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		rel, relErr := filepath.Rel(m.root, path)
		if relErr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if !validPath(relSlash) {
			return nil
		}
		if m.excluded(relSlash) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Symlinks are not followed. Following them would let a link inside
		// the sync folder pull arbitrary files from the rest of the disk into
		// the account.
		if d.Type()&os.ModeSymlink != 0 {
			m.emit(Event{Kind: EventWarn, Path: relSlash,
				Err: fmt.Errorf("symlink skipped: pDrive does not follow links out of the sync folder")})
			return nil
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return nil // sockets, fifos, devices
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			m.emit(Event{Kind: EventWarn, Path: relSlash, Err: infoErr})
			return nil
		}

		out[relSlash] = &LocalNode{
			Path:  relSlash,
			IsDir: d.IsDir(),
			Size:  info.Size(),
			Mtime: info.ModTime(),
			Inode: inodeOf(info),
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", m.root, err)
	}
	return out, nil
}

// excluded reports whether a path is under a selective-sync exclusion.
func (m *Mirror) excluded(path string) bool {
	for _, ex := range m.cfg.Selective.Exclude {
		ex = strings.Trim(ex, "/")
		if ex == "" {
			continue
		}
		if path == ex || strings.HasPrefix(path, ex+"/") {
			return true
		}
	}
	return false
}

// hashIfNeeded fills in a local node's hash, but only when the baseline's
// cheap identity (size plus modification time) fails to prove it unchanged.
func (m *Mirror) hashIfNeeded(n *LocalNode, base *state.Node) error {
	if n == nil || n.IsDir {
		return nil
	}
	if base != nil && base.ContentHash != "" &&
		base.Size == n.Size && sameSecond(base.LocalMtime, n.Mtime) {
		n.Hash = base.ContentHash
		return nil
	}

	sum, err := hashFile(m.localPath(n.Path))
	if err != nil {
		return fmt.Errorf("hash %s: %w", n.Path, err)
	}
	n.Hash = sum
	return nil
}

// sameSecond compares timestamps at one-second resolution.
//
// Not every filesystem, archive format or transfer preserves sub-second
// precision, so demanding exact equality would rehash files constantly.
func sameSecond(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return false
	}
	return a.Truncate(time.Second).Equal(b.Truncate(time.Second))
}
