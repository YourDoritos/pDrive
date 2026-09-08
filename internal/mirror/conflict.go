package mirror

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// preserveLocalIfConflicting saves a locally-edited file before the pull
// stage overwrites it with the remote version.
//
// incomingHash is the SHA1 of the content already staged on disk, so this
// decision never has to guess: it compares actual bytes against actual bytes.
//
// This is the single most destructive moment in the whole engine: a download
// replaces a file in place, and if that file held unsaved local work, the
// work is gone with no trace. So before any overwrite, the local copy is
// checked against the baseline; if it has diverged from both the baseline and
// the incoming content, it is renamed aside and kept.
//
// The remote version then takes the canonical path and the local one lands
// beside it as "name (conflict <when> <host>).ext". The push stage picks the
// copy up as a new file and uploads it, so both versions end up on both
// sides. Nothing is ever discarded.
func (m *Mirror) preserveLocalIfConflicting(path, incomingHash string) error {
	before, seen := m.localBefore[path]
	if !seen || before == nil || before.IsDir {
		return nil
	}

	local := before.Hash
	if local == "" {
		// Not hashed during the pre-pull scan. Hash it now rather than guess:
		// an unhashed file must never be assumed unmodified at this point.
		sum, err := hashFile(m.localPath(path))
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("hash %s before overwrite: %w", path, err)
		}
		local = sum
	}

	base, err := m.db.GetNode(path)
	if err != nil {
		return err
	}

	// Unchanged since we last agreed: safe to overwrite.
	if base != nil && base.ContentHash != "" && strings.EqualFold(local, base.ContentHash) {
		return nil
	}
	// Identical to what is arriving: nothing is being lost, whoever wrote it.
	if incomingHash != "" && strings.EqualFold(local, incomingHash) {
		return nil
	}

	when := time.Now()
	keep := ConflictName(path, when, m.hostname)
	if err := os.Rename(m.localPath(path), m.localPath(keep)); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("preserve local copy of %s: %w", path, err)
	}

	if err := m.db.RecordConflict(path, keep, incomingHash); err != nil {
		// The file is already preserved on disk; failing to log it should not
		// undo that.
		m.emit(Event{Kind: EventWarn, Path: path, Err: err})
	}

	m.result.Conflicts++
	m.emit(Event{Kind: EventConflict, Path: path + " -> kept local copy as " + keep})
	return nil
}
