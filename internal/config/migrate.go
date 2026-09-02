package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MigrateSyncRoot moves an existing synced folder to a new location.
//
// The sync state database stores paths *relative* to the root, so moving the
// tree keeps every baseline entry valid: content hashes, revisions and inodes
// all still describe the same files. Nothing is re-downloaded and nothing is
// re-uploaded — which matters when the folder is hundreds of gigabytes.
//
// The daemon must not be running. A move it does not know about looks exactly
// like the user deleting everything, and the next pass would propagate that.
func MigrateSyncRoot(oldRoot, newRoot string) error {
	oldRoot = filepath.Clean(ExpandPath(oldRoot))
	newRoot = filepath.Clean(ExpandPath(newRoot))

	if err := ValidateSyncRoot(oldRoot, newRoot); err != nil {
		return err
	}

	// Nothing to move: the old folder was never created.
	if _, err := os.Stat(oldRoot); os.IsNotExist(err) {
		return os.MkdirAll(newRoot, 0700)
	}

	if err := os.MkdirAll(filepath.Dir(newRoot), 0700); err != nil {
		return fmt.Errorf("create parent of %s: %w", newRoot, err)
	}

	// Same filesystem: one atomic rename, no window where the files exist
	// twice or not at all.
	if err := os.Rename(oldRoot, newRoot); err == nil {
		return nil
	}

	// Across filesystems rename cannot work, so copy and only then remove.
	// Removing first would risk losing everything to a failed copy.
	if err := copyTree(oldRoot, newRoot); err != nil {
		return fmt.Errorf("copy to %s: %w", newRoot, err)
	}
	if err := verifyTree(oldRoot, newRoot); err != nil {
		return fmt.Errorf("the copy is incomplete, the original is untouched: %w", err)
	}
	if err := os.RemoveAll(oldRoot); err != nil {
		return fmt.Errorf("copied to %s but could not remove %s: %w", newRoot, oldRoot, err)
	}
	return nil
}

// ValidateSyncRoot reports whether newRoot is a usable destination.
func ValidateSyncRoot(oldRoot, newRoot string) error {
	if newRoot == "" {
		return fmt.Errorf("the folder cannot be empty")
	}
	if !filepath.IsAbs(newRoot) {
		return fmt.Errorf("the folder must be an absolute path (~ is fine)")
	}
	if newRoot == "/" {
		return fmt.Errorf("refusing to sync the whole filesystem")
	}
	if home, err := os.UserHomeDir(); err == nil && newRoot == filepath.Clean(home) {
		return fmt.Errorf("refusing to sync your entire home directory")
	}
	if newRoot == oldRoot {
		return fmt.Errorf("that is already the sync folder")
	}

	// Nesting either way leaves the mirror inside itself, which recurses.
	if isUnder(newRoot, oldRoot) {
		return fmt.Errorf("the new folder is inside the current one")
	}
	if isUnder(oldRoot, newRoot) {
		return fmt.Errorf("the new folder contains the current one")
	}

	info, err := os.Stat(newRoot)
	if os.IsNotExist(err) {
		return nil // it will be created
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is a file", newRoot)
	}

	// Merging into someone else's files would make their contents
	// indistinguishable from synced content, and the next pass would upload
	// all of it.
	entries, err := os.ReadDir(newRoot)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s is not empty; choose an empty or new folder", newRoot)
	}
	return nil
}

func isUnder(path, parent string) bool {
	return path == parent || strings.HasPrefix(path, parent+string(os.PathSeparator))
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		switch {
		case info.IsDir():
			return os.MkdirAll(target, 0700)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case !info.Mode().IsRegular():
			return nil // sockets, fifos, devices: nothing to carry over
		}

		if err := copyFile(path, target, info); err != nil {
			return err
		}
		// Modification times are load-bearing: the reconciler uses them to
		// decide what changed, so losing them would rehash the whole tree.
		return os.Chtimes(target, info.ModTime(), info.ModTime())
	})
}

func copyFile(src, dst string, info os.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// verifyTree checks every regular file arrived with the right size before the
// original is removed.
func verifyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !info.Mode().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target, err := os.Stat(filepath.Join(dst, rel))
		if err != nil {
			return fmt.Errorf("%s did not copy: %w", rel, err)
		}
		if target.Size() != info.Size() {
			return fmt.Errorf("%s copied as %d bytes, expected %d",
				rel, target.Size(), info.Size())
		}
		return nil
	})
}
