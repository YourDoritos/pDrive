package mirror

import (
	"strings"
	"time"
)

// Action is what reconciliation decided to do about one path.
type Action int

const (
	// ActNothing means both sides already agree.
	ActNothing Action = iota
	// ActDownload brings the remote version down.
	ActDownload
	// ActUpload sends the local version up as a new revision.
	ActUpload
	// ActAdopt means both sides changed to the *same* content, so only the
	// baseline needs updating. No transfer.
	ActAdopt
	// ActTrashLocal removes a locally-present file that was deleted remotely.
	ActTrashLocal
	// ActTrashRemote trashes a remote file that was deleted locally.
	ActTrashRemote
	// ActConflict means both sides changed to different content.
	ActConflict
	// ActMkdirLocal creates a directory locally.
	ActMkdirLocal
	// ActMkdirRemote creates a directory remotely.
	ActMkdirRemote
	// ActDropBaseline removes a baseline row for something gone from both
	// sides.
	ActDropBaseline
)

func (a Action) String() string {
	switch a {
	case ActNothing:
		return "nothing"
	case ActDownload:
		return "download"
	case ActUpload:
		return "upload"
	case ActAdopt:
		return "adopt"
	case ActTrashLocal:
		return "trash-local"
	case ActTrashRemote:
		return "trash-remote"
	case ActConflict:
		return "conflict"
	case ActMkdirLocal:
		return "mkdir-local"
	case ActMkdirRemote:
		return "mkdir-remote"
	case ActDropBaseline:
		return "drop-baseline"
	}
	return "unknown"
}

// Facts is everything reconciliation knows about one path.
//
// Each side is described by whether it exists, whether it is a directory, and
// a content identity. Deliberately free of *state.Node, os.FileInfo and
// drive.Node so the decision logic is a pure function over plain data and can
// be exhaustively table-tested.
type Facts struct {
	Path string

	BaseExists bool
	BaseHash   string
	BaseIsDir  bool

	LocalExists bool
	LocalHash   string
	LocalIsDir  bool
	LocalMtime  time.Time

	RemoteExists bool
	RemoteHash   string
	RemoteIsDir  bool
	RemoteMtime  time.Time
}

// changedLocally reports whether the local side differs from the baseline.
func (f Facts) changedLocally() bool {
	if !f.BaseExists {
		return f.LocalExists
	}
	if !f.LocalExists {
		return true
	}
	if f.LocalIsDir || f.BaseIsDir {
		return f.LocalIsDir != f.BaseIsDir
	}
	// An unknown hash on either side cannot prove a change. Callers hash
	// lazily, so an empty value means "not computed", not "empty file".
	if f.LocalHash == "" || f.BaseHash == "" {
		return false
	}
	return !strings.EqualFold(f.LocalHash, f.BaseHash)
}

// changedRemotely reports whether the remote side differs from the baseline.
func (f Facts) changedRemotely() bool {
	if !f.BaseExists {
		return f.RemoteExists
	}
	if !f.RemoteExists {
		return true
	}
	if f.RemoteIsDir || f.BaseIsDir {
		return f.RemoteIsDir != f.BaseIsDir
	}
	if f.RemoteHash == "" || f.BaseHash == "" {
		return false
	}
	return !strings.EqualFold(f.RemoteHash, f.BaseHash)
}

// sameContent reports whether both sides hold identical content.
func (f Facts) sameContent() bool {
	if f.LocalIsDir != f.RemoteIsDir {
		return false
	}
	if f.LocalIsDir {
		return true
	}
	if f.LocalHash == "" || f.RemoteHash == "" {
		return false
	}
	return strings.EqualFold(f.LocalHash, f.RemoteHash)
}

// Reconcile decides what to do about one path.
//
// The whole engine's correctness sits in this function, so it is pure: no
// filesystem, no network, no clock. Every branch is covered by a table test.
//
// Two principles govern the hard cases:
//
//  1. **Deletion never beats a modification.** If one side deleted a file
//     while the other edited it, the edit wins and is propagated. Losing work
//     someone did is far worse than resurrecting a file someone can delete
//     again.
//
//  2. **A conflict never discards either version.** Both are kept; the caller
//     writes the remote copy to the canonical path and preserves the local one
//     beside it.
func Reconcile(f Facts) Action {
	switch {
	case !f.LocalExists && !f.RemoteExists:
		if f.BaseExists {
			return ActDropBaseline
		}
		return ActNothing

	case f.LocalExists && !f.RemoteExists:
		if f.LocalIsDir {
			// A directory present locally but not remotely: either it is new
			// and must be created, or it was removed remotely.
			if f.BaseExists {
				return ActTrashLocal
			}
			return ActMkdirRemote
		}
		if !f.BaseExists {
			return ActUpload // new local file
		}
		if f.changedLocally() {
			return ActUpload // principle 1: the edit beats the remote delete
		}
		return ActTrashLocal

	case !f.LocalExists && f.RemoteExists:
		if f.RemoteIsDir {
			if f.BaseExists {
				return ActTrashRemote
			}
			return ActMkdirLocal
		}
		if !f.BaseExists {
			return ActDownload // new remote file
		}
		if f.changedRemotely() {
			return ActDownload // principle 1, mirrored
		}
		return ActTrashRemote

	default: // present on both sides
		if f.LocalIsDir && f.RemoteIsDir {
			return ActNothing
		}
		if f.LocalIsDir != f.RemoteIsDir {
			// A file on one side, a directory on the other. Never guess.
			return ActConflict
		}

		localChanged := f.changedLocally()
		remoteChanged := f.changedRemotely()

		switch {
		case !localChanged && !remoteChanged:
			if !f.BaseExists {
				// Untracked on both sides but identical: adopt rather than
				// transfer.
				if f.sameContent() {
					return ActAdopt
				}
				return ActConflict
			}
			return ActNothing
		case localChanged && !remoteChanged:
			return ActUpload
		case !localChanged && remoteChanged:
			return ActDownload
		default:
			if f.sameContent() {
				return ActAdopt // principle 2 is moot: they agree
			}
			return ActConflict
		}
	}
}

// ConflictName builds the name given to a preserved local copy.
//
// It keeps the original extension so the file still opens in the right
// application, and names the host so a conflict between two machines is
// self-explanatory.
func ConflictName(path string, when time.Time, host string) string {
	dir := ""
	name := path
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		dir, name = path[:idx+1], path[idx+1:]
	}

	ext := ""
	if idx := strings.LastIndex(name, "."); idx > 0 {
		ext = name[idx:]
		name = name[:idx]
	}

	if host == "" {
		host = "unknown-host"
	}
	return dir + name + " (conflict " + when.Format("2006-01-02 15-04-05") + " " + host + ")" + ext
}
