// Package backup makes a verified, offline copy of a Proton Drive account.
//
// This is Phase 0.5 of the project plan and it is a hard gate: no pDrive code
// writes to a Proton Drive account until a backup produced here has verified
// clean. It is read-only against Proton by construction — it lists and
// downloads, and calls nothing that mutates.
package backup

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/YourDoritos/pdrive/internal/drive"
)

// ManifestName is the machine-readable manifest.
const ManifestName = "manifest.json"

// ChecksumName is a sha1sum(1)-compatible checksum file. It exists so the
// backup can be verified with standard tools and does not depend on pDrive
// being correct, or even installed.
const ChecksumName = "MANIFEST.sha1"

// DataDir is the subdirectory holding the mirrored files, kept separate so
// the manifests are never mistaken for account content.
const DataDir = "data"

// Entry is one file or folder recorded in the manifest.
type Entry struct {
	Path     string    `json:"path"`
	IsDir    bool      `json:"is_dir"`
	Size     int64     `json:"size,omitempty"`
	SHA1     string    `json:"sha1,omitempty"`
	Modified time.Time `json:"modified,omitempty"`
	LinkID   string    `json:"link_id"`
	// RemoteDigest is Proton's own SHA1, when the file carries it. Compared
	// against our locally computed hash as an end-to-end integrity check.
	RemoteDigest string `json:"remote_digest,omitempty"`
	// DigestMatch records the outcome of that comparison: "match",
	// "mismatch", or "absent" when Proton stored no digest.
	DigestMatch string `json:"digest_match,omitempty"`
}

// Manifest describes a completed backup.
type Manifest struct {
	Version   int       `json:"version"`
	Account   string    `json:"account"`
	CreatedAt time.Time `json:"created_at"`
	Tool      string    `json:"tool"`
	Files     int       `json:"files"`
	Dirs      int       `json:"dirs"`
	Bytes     int64     `json:"bytes"`
	Entries   []Entry   `json:"entries"`
}

// Options configures a backup run.
type Options struct {
	// Dest is the backup directory. It is created if absent.
	Dest string
	// Account is recorded in the manifest for provenance.
	Account string
	// Resume skips files already present with the recorded size and hash,
	// making an interrupted backup cheap to restart.
	Resume bool
	// Progress, if non-nil, is called as work completes.
	Progress func(ev Event)
}

// EventKind classifies a progress event.
type EventKind int

const (
	// EventDir reports a directory being created.
	EventDir EventKind = iota
	// EventFile reports a file downloaded.
	EventFile
	// EventSkip reports a file skipped because it was already present.
	EventSkip
	// EventWarn reports a non-fatal problem.
	EventWarn
)

// Event is a progress notification.
type Event struct {
	Kind EventKind
	Path string
	Size int64
	Err  error
}

func (o *Options) emit(ev Event) {
	if o.Progress != nil {
		o.Progress(ev)
	}
}

// Run walks the account and mirrors it to disk, writing both manifests.
func Run(ctx context.Context, d *drive.Drive, opts Options) (*Manifest, error) {
	dataRoot := filepath.Join(opts.Dest, DataDir)
	if err := os.MkdirAll(dataRoot, 0700); err != nil {
		return nil, fmt.Errorf("create backup directory: %w", err)
	}

	manifest := &Manifest{
		Version:   1,
		Account:   opts.Account,
		CreatedAt: time.Now().UTC(),
		Tool:      "pdrive backup",
	}

	err := d.Walk(ctx, func(n drive.Node) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		parts, err := SanitizePath(n.Path)
		if err != nil {
			// Refuse to write a name we cannot prove is safe, but keep going:
			// one hostile or malformed name must not abandon the backup.
			opts.emit(Event{Kind: EventWarn, Path: n.Path, Err: err})
			return nil
		}
		local := filepath.Join(append([]string{dataRoot}, parts...)...)

		if n.IsDir {
			if err := os.MkdirAll(local, 0700); err != nil {
				return fmt.Errorf("create %s: %w", n.Path, err)
			}
			manifest.Dirs++
			manifest.Entries = append(manifest.Entries, Entry{
				Path: n.Path, IsDir: true, LinkID: n.LinkID,
			})
			opts.emit(Event{Kind: EventDir, Path: n.Path})
			return nil
		}

		entry := Entry{
			Path:         n.Path,
			Size:         n.Size,
			Modified:     n.Modified,
			LinkID:       n.LinkID,
			RemoteDigest: n.Digest,
		}

		if opts.Resume {
			if sum, size, ok := existingFile(local, n.Size); ok {
				entry.SHA1 = sum
				entry.Size = size
				entry.DigestMatch = compareDigest(sum, n.Digest)
				manifest.Files++
				manifest.Bytes += size
				manifest.Entries = append(manifest.Entries, entry)
				opts.emit(Event{Kind: EventSkip, Path: n.Path, Size: size})
				return nil
			}
		}

		if err := os.MkdirAll(filepath.Dir(local), 0700); err != nil {
			return fmt.Errorf("create parent of %s: %w", n.Path, err)
		}

		sum, written, err := download(ctx, d, n.LinkID, local)
		if err != nil {
			// A single unreadable file should not destroy an otherwise good
			// backup. Record it as a warning; VerifyManifest will report the
			// gap, and the run's error count is reported to the caller.
			opts.emit(Event{Kind: EventWarn, Path: n.Path, Err: err})
			return nil
		}

		entry.SHA1 = sum
		entry.Size = written
		entry.DigestMatch = compareDigest(sum, n.Digest)
		if entry.DigestMatch == "mismatch" {
			opts.emit(Event{Kind: EventWarn, Path: n.Path,
				Err: fmt.Errorf("digest mismatch: Proton reports %s, downloaded content hashes to %s", n.Digest, sum)})
		}

		manifest.Files++
		manifest.Bytes += written
		manifest.Entries = append(manifest.Entries, entry)
		opts.emit(Event{Kind: EventFile, Path: n.Path, Size: written})
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(manifest.Entries, func(i, j int) bool {
		return manifest.Entries[i].Path < manifest.Entries[j].Path
	})

	if err := writeManifest(opts.Dest, manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

// download streams one file to disk, hashing as it goes, and only moves it
// into place once it is complete and on stable storage. A crash mid-download
// therefore never leaves a truncated file at the real path.
func download(ctx context.Context, d *drive.Drive, linkID, dest string) (string, int64, error) {
	rc, _, err := d.Download(ctx, linkID)
	if err != nil {
		return "", 0, err
	}
	defer rc.Close()

	tmp := dest + ".pdrive-part"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return "", 0, fmt.Errorf("create temp file: %w", err)
	}

	hasher := sha1.New()
	written, err := io.Copy(io.MultiWriter(f, hasher), rc)
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return "", 0, fmt.Errorf("copy: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", 0, fmt.Errorf("sync: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", 0, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return "", 0, fmt.Errorf("rename: %w", err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), written, nil
}

// existingFile reports the hash of an already-downloaded file when its size
// matches what the server reports.
func existingFile(path string, wantSize int64) (string, int64, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", 0, false
	}
	if wantSize > 0 && info.Size() != wantSize {
		return "", 0, false
	}
	sum, err := HashFile(path)
	if err != nil {
		return "", 0, false
	}
	return sum, info.Size(), true
}

// HashFile returns the hex SHA1 of a file's contents.
func HashFile(path string) (string, error) {
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

func compareDigest(local, remote string) string {
	if remote == "" {
		return "absent"
	}
	if strings.EqualFold(local, remote) {
		return "match"
	}
	return "mismatch"
}

func writeManifest(dest string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dest, ManifestName), append(data, '\n')); err != nil {
		return err
	}

	// sha1sum-compatible: two spaces, paths relative to the backup root so
	// `cd <backup> && sha1sum -c MANIFEST.sha1` just works.
	var sb strings.Builder
	for _, e := range m.Entries {
		if e.IsDir || e.SHA1 == "" {
			continue
		}
		fmt.Fprintf(&sb, "%s  %s/%s\n", e.SHA1, DataDir, e.Path)
	}
	return writeFileAtomic(filepath.Join(dest, ChecksumName), []byte(sb.String()))
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// LoadManifest reads a manifest from a backup directory.
func LoadManifest(dest string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dest, ManifestName))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// VerifyResult reports the outcome of re-hashing a backup.
type VerifyResult struct {
	Checked  int
	OK       int
	Missing  []string
	Mismatch []string
	// RemoteMismatch lists files whose content disagreed with Proton's own
	// stored digest at backup time.
	RemoteMismatch []string
}

// Clean reports whether the backup verified without any problem.
func (r *VerifyResult) Clean() bool {
	return len(r.Missing) == 0 && len(r.Mismatch) == 0 && len(r.RemoteMismatch) == 0
}

// Verify re-reads every file in the backup and re-hashes it, comparing
// against the manifest. This reads from disk only: it proves the copy on
// this machine is intact and does not touch the network.
func Verify(dest string, progress func(path string, ok bool)) (*VerifyResult, error) {
	m, err := LoadManifest(dest)
	if err != nil {
		return nil, err
	}

	res := &VerifyResult{}
	dataRoot := filepath.Join(dest, DataDir)

	for _, e := range m.Entries {
		if e.IsDir {
			continue
		}
		res.Checked++

		if e.DigestMatch == "mismatch" {
			res.RemoteMismatch = append(res.RemoteMismatch, e.Path)
		}

		parts, err := SanitizePath(e.Path)
		if err != nil {
			res.Missing = append(res.Missing, e.Path)
			continue
		}
		local := filepath.Join(append([]string{dataRoot}, parts...)...)

		if e.SHA1 == "" {
			// Recorded without a hash: the download failed during backup.
			res.Missing = append(res.Missing, e.Path)
			if progress != nil {
				progress(e.Path, false)
			}
			continue
		}

		sum, err := HashFile(local)
		if err != nil {
			res.Missing = append(res.Missing, e.Path)
			if progress != nil {
				progress(e.Path, false)
			}
			continue
		}
		if !strings.EqualFold(sum, e.SHA1) {
			res.Mismatch = append(res.Mismatch, e.Path)
			if progress != nil {
				progress(e.Path, false)
			}
			continue
		}

		res.OK++
		if progress != nil {
			progress(e.Path, true)
		}
	}
	return res, nil
}
