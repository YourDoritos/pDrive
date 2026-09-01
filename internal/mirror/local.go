package mirror

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// StubSuffix marks a placeholder standing in for a file that was not
// downloaded because it exceeds max_auto_download_size.
//
// The suffix is deliberately part of the *filename*. A stub can therefore
// never be mistaken for the real file by any program, at any time, whether or
// not pDrive is running. Phase 5 replaces this with a sparse file at the real
// name populated on open via fanotify, which is nicer but only safe while a
// populator is guaranteed to be alive.
const StubSuffix = ".pdrive-stub"

// TempSuffix marks an in-progress download.
const TempSuffix = ".pdrive-part"

// stageDownload streams r into a temporary file beside its destination and
// returns the temp path, the content's SHA1 and the byte count.
//
// Staging before deciding anything is what makes conflict detection exact: we
// end up holding the incoming content and the existing local file at the same
// time, so "did this actually change?" is a hash comparison rather than an
// inference from metadata we may not have. It also means a crash mid-download
// can never leave a truncated file at the real path.
func stageDownload(path string, r io.Reader) (string, string, int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", "", 0, fmt.Errorf("create parent directory: %w", err)
	}

	tmp := path + TempSuffix
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return "", "", 0, fmt.Errorf("create temp file: %w", err)
	}

	hasher := sha1.New()
	written, err := io.Copy(io.MultiWriter(f, hasher), r)
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return "", "", 0, fmt.Errorf("write: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", "", 0, fmt.Errorf("sync: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", "", 0, err
	}
	return tmp, hex.EncodeToString(hasher.Sum(nil)), written, nil
}

// commitStaged moves a staged download into place, stamping the modification
// time Proton recorded for it.
func commitStaged(tmp, path string, mtime time.Time) error {
	if !mtime.IsZero() {
		// Not worth failing a download over.
		_ = os.Chtimes(tmp, mtime, mtime)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename into place: %w", err)
	}
	return nil
}

// writeStub writes the placeholder for a file that was not downloaded.
func writeStub(path string, n stubInfo) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}

	var b strings.Builder
	b.WriteString("pdrive placeholder\n\n")
	fmt.Fprintf(&b, "name:     %s\n", n.Name)
	fmt.Fprintf(&b, "size:     %d\n", n.Size)
	if !n.Modified.IsZero() {
		fmt.Fprintf(&b, "modified: %s\n", n.Modified.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "link:     %s\n\n", n.LinkID)
	b.WriteString("The content was not downloaded because it is larger than\n")
	b.WriteString("sync.max_auto_download_size. Fetch it with:\n\n")
	fmt.Fprintf(&b, "    pdrive get %q\n", n.Path)

	tmp := path + TempSuffix
	if err := os.WriteFile(tmp, []byte(b.String()), 0600); err != nil {
		return fmt.Errorf("write stub: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename stub: %w", err)
	}
	return nil
}

type stubInfo struct {
	Name     string
	Path     string
	Size     int64
	Modified time.Time
	LinkID   string
}

// trashLocal moves a path into the local trash instead of unlinking it.
//
// Safety guard 4: a remote deletion must never destroy the only copy of
// something on this machine. A bad event, a bug in reconciliation, or someone
// else clearing a shared folder should all be recoverable.
func trashLocal(trashDir, root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = filepath.Base(path)
	}

	stamp := time.Now().Format("2006-01-02T15-04-05")
	dest := filepath.Join(trashDir, stamp, rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return fmt.Errorf("create trash directory: %w", err)
	}

	if err := os.Rename(path, dest); err != nil {
		// Rename fails across filesystems. Fall back to copy-then-remove so
		// a trash directory on another mount still works.
		if copyErr := copyTree(path, dest); copyErr != nil {
			return fmt.Errorf("move to trash: %w", err)
		}
		return os.RemoveAll(path)
	}
	return nil
}

func copyTree(src, dest string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.MkdirAll(dest, 0700); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dest, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// hashFile returns the hex SHA1 of a file's contents.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// inodeOf returns the inode number, used later for move detection.
func inodeOf(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

// removeEmptyParents prunes directories left empty after a deletion, stopping
// at root so the sync folder itself always survives.
func removeEmptyParents(root, path string) {
	dir := filepath.Dir(path)
	for strings.HasPrefix(dir, root) && dir != root {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
