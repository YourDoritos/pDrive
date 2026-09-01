package mirror

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/YourDoritos/pdrive/internal/drive"
)

// fakeDrive is an in-memory Source. It exists so the reconciler can be tested
// against hostile inputs — mass deletions, unsafe names, download failures —
// without a network or an account.
type fakeDrive struct {
	files map[string]*fakeNode // keyed by path
	// events queued for the next PollEvents call
	delta   *drive.Delta
	cursor  string
	failOn  map[string]error // linkID -> error to return from Download
	walked  int
	listed  int
	polled  int
	refresh bool

	uploaded []string
	trashed  []string
	moved    []string
}

type fakeNode struct {
	linkID   string
	parentID string
	isDir    bool
	content  []byte
	modified time.Time
	digest   string
	// size overrides len(content), so a huge file can be simulated without
	// allocating it.
	size int64
}

func newFakeDrive() *fakeDrive {
	return &fakeDrive{
		files:  map[string]*fakeNode{},
		failOn: map[string]error{},
		cursor: "cursor-0",
	}
}

func (f *fakeDrive) addDir(path string) *fakeDrive {
	f.files[path] = &fakeNode{linkID: "link:" + path, parentID: f.parentIDOf(path), isDir: true}
	return f
}

// addFileWithDigest registers a file that publishes a SHA1 digest, as Proton
// does for every real file.
func (f *fakeDrive) addFileWithDigest(path, content string) *fakeDrive {
	f.addFile(path, content)
	sum := sha1.Sum([]byte(content))
	f.files[path].digest = hex.EncodeToString(sum[:])
	return f
}

func (f *fakeDrive) addFile(path, content string) *fakeDrive {
	f.files[path] = &fakeNode{
		linkID:   "link:" + path,
		parentID: f.parentIDOf(path),
		content:  []byte(content),
		modified: time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC),
	}
	return f
}

// addHugeFile registers a file that reports a large size without holding one.
func (f *fakeDrive) addHugeFile(path string, size int64) *fakeDrive {
	f.addFile(path, "sentinel")
	f.files[path].size = size
	return f
}

func (f *fakeDrive) parentIDOf(path string) string {
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return "link:root"
	}
	return "link:" + path[:idx]
}

func (f *fakeDrive) RootLinkID() string { return "link:root" }
func (f *fakeDrive) VolumeID() string   { return "vol-1" }
func (f *fakeDrive) ShareID() string    { return "share-1" }

func (f *fakeDrive) LatestEventID(context.Context) (string, error) { return f.cursor, nil }

func (f *fakeDrive) PollEvents(_ context.Context, cursor string) (*drive.Delta, error) {
	f.polled++
	if f.refresh {
		return &drive.Delta{Cursor: cursor, Refresh: true}, nil
	}
	if f.delta == nil {
		return &drive.Delta{Cursor: cursor}, nil
	}
	d := f.delta
	f.delta = nil
	return d, nil
}

func (f *fakeDrive) node(path string) drive.Node {
	n := f.files[path]
	size := int64(len(n.content))
	if n.size > 0 {
		size = n.size
	}
	name := path
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		name = path[idx+1:]
	}
	return drive.Node{
		LinkID: n.linkID, ParentID: n.parentID, Name: name, Path: path,
		IsDir: n.isDir, Size: size, Modified: n.modified, Digest: n.digest,
	}
}

func (f *fakeDrive) Walk(ctx context.Context, fn drive.WalkFunc) error {
	f.walked++
	paths := make([]string, 0, len(f.files))
	for p := range f.files {
		paths = append(paths, p)
	}
	// Parents before children.
	sort.Strings(paths)
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(f.node(p)); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeDrive) ListDir(_ context.Context, linkID, prefix string) ([]drive.Node, error) {
	f.listed++
	var out []drive.Node
	for p, n := range f.files {
		if n.parentID != linkID {
			continue
		}
		out = append(out, f.node(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (f *fakeDrive) Download(_ context.Context, linkID string) (io.ReadCloser, int64, error) {
	if err, ok := f.failOn[linkID]; ok {
		return nil, 0, err
	}
	for _, n := range f.files {
		if n.linkID == linkID {
			return io.NopCloser(strings.NewReader(string(n.content))), int64(len(n.content)), nil
		}
	}
	return nil, 0, fmt.Errorf("no such link %s", linkID)
}

var _ Source = (*fakeDrive)(nil)

func (f *fakeDrive) Upload(_ context.Context, parentLinkID, name string, modTime time.Time, r io.Reader) (string, error) {
	if err, ok := f.failOn["upload:"+name]; ok {
		return "", err
	}
	content, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}

	path := name
	if parentLinkID != "link:root" {
		parent := strings.TrimPrefix(parentLinkID, "link:")
		path = parent + "/" + name
	}
	sum := sha1.Sum(content)
	f.files[path] = &fakeNode{
		linkID: "link:" + path, parentID: parentLinkID,
		content: content, modified: modTime,
		digest: hex.EncodeToString(sum[:]),
	}
	f.uploaded = append(f.uploaded, path)
	return "link:" + path, nil
}

func (f *fakeDrive) Mkdir(_ context.Context, parentLinkID, name string) (string, error) {
	path := name
	if parentLinkID != "link:root" {
		path = strings.TrimPrefix(parentLinkID, "link:") + "/" + name
	}
	f.files[path] = &fakeNode{linkID: "link:" + path, parentID: parentLinkID, isDir: true}
	return "link:" + path, nil
}

func (f *fakeDrive) Trash(_ context.Context, linkID string, _ bool) error {
	for path, n := range f.files {
		if n.linkID == linkID {
			delete(f.files, path)
			f.trashed = append(f.trashed, path)
			return nil
		}
	}
	return fmt.Errorf("no such link %s", linkID)
}

func (f *fakeDrive) Move(_ context.Context, linkID, newParentID, newName string, _ bool) error {
	for path, n := range f.files {
		if n.linkID != linkID {
			continue
		}
		newPath := newName
		if newParentID != "link:root" {
			newPath = strings.TrimPrefix(newParentID, "link:") + "/" + newName
		}
		delete(f.files, path)
		n.parentID = newParentID
		n.linkID = "link:" + newPath
		f.files[newPath] = n
		f.moved = append(f.moved, path+" -> "+newPath)
		return nil
	}
	return fmt.Errorf("no such link %s", linkID)
}

// content returns a remote file's bytes, for assertions.
func (f *fakeDrive) content(path string) string {
	if n, ok := f.files[path]; ok {
		return string(n.content)
	}
	return ""
}

func (f *fakeDrive) has(path string) bool {
	_, ok := f.files[path]
	return ok
}
