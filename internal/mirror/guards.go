package mirror

import (
	"fmt"
	"os"

	"github.com/YourDoritos/pdrive/internal/state"
)

// ErrGuard is returned when a safety guard stops a sync pass. It is never a
// bug report — it means pDrive saw something that could destroy data and
// refused to continue.
type ErrGuard struct {
	Guard  string
	Detail string
}

func (e *ErrGuard) Error() string {
	return fmt.Sprintf("safety guard %q stopped this pass: %s", e.Guard, e.Detail)
}

// checkRoot enforces safety guard 2.
//
// If the sync root is missing, is not a directory, or is empty while the
// baseline says it should hold files, something is wrong with the *machine*
// (an unmounted disk, a renamed folder, a fresh home directory) rather than
// with the account. Interpreting that as "the user deleted everything" is how
// sync clients wipe accounts, so pDrive stops instead.
func (m *Mirror) checkRoot() error {
	info, err := os.Stat(m.root)
	if os.IsNotExist(err) {
		return &ErrGuard{"missing-root", fmt.Sprintf("%s does not exist", m.root)}
	}
	if err != nil {
		return &ErrGuard{"missing-root", fmt.Sprintf("cannot stat %s: %v", m.root, err)}
	}
	if !info.IsDir() {
		return &ErrGuard{"missing-root", fmt.Sprintf("%s is not a directory", m.root)}
	}

	tracked, err := m.db.CountNodes()
	if err != nil {
		return err
	}
	if tracked == 0 {
		return nil
	}

	entries, err := os.ReadDir(m.root)
	if err != nil {
		return &ErrGuard{"missing-root", fmt.Sprintf("cannot read %s: %v", m.root, err)}
	}
	if len(entries) == 0 {
		return &ErrGuard{"missing-root", fmt.Sprintf(
			"%s is empty but pDrive tracks %d nodes there — refusing to treat this as a mass deletion "+
				"(is the disk mounted? was the folder renamed?)", m.root, tracked)}
	}
	return nil
}

// minDeletionsForGuard is the absolute floor below which the deletion cliff
// never fires.
//
// A percentage alone is useless on a small account: deleting one file out of
// two is 50%, so the guard would trip on completely ordinary activity. Users
// would learn to pass --confirm-deletions reflexively, and the guard would
// then protect nothing at the moment it mattered. Requiring a meaningful
// absolute count keeps it silent during normal use and loud during a
// catastrophe, which is the only way a guard like this stays trusted.
//
// Small accounts are not left unprotected: every removal still goes to the
// local trash (safety guard 4), so nothing is destroyed either way.
const minDeletionsForGuard = 10

// checkDeletionCliff enforces safety guard 1.
//
// A single pass that would remove a large share of everything pDrive tracks
// is far more likely to be a bug, a bad event batch, or a hostile change than
// a real intention. It stops and asks.
func (m *Mirror) checkDeletionCliff(deletions int) error {
	if deletions < minDeletionsForGuard {
		return nil
	}

	limit := m.cfg.Sync.DeletionGuardPercent
	if limit >= 100 {
		// Explicitly disabled, e.g. by --confirm-deletions.
		return nil
	}

	tracked, err := m.db.CountNodes()
	if err != nil {
		return err
	}
	if tracked == 0 {
		return nil
	}

	if deletions > tracked {
		// Should be unreachable: callers count only tracked nodes. If it ever
		// happens the count is wrong, and a wrong count must not be dressed
		// up as a confident percentage.
		deletions = tracked
	}
	pct := deletions * 100 / tracked
	if pct < limit {
		return nil
	}

	return &ErrGuard{"deletion-cliff", fmt.Sprintf(
		"this pass would remove %d of %d tracked nodes (%d%%, limit %d%%); "+
			"nothing was deleted — re-run with `pdrive sync --confirm-deletions` if this is expected",
		deletions, tracked, pct, limit)}
}

// materializedFor reports whether a node of this size should be downloaded
// or left as a stub, per sync.max_auto_download_size.
func (m *Mirror) materializedFor(size int64) state.Materialization {
	cap := m.cfg.Sync.MaxAutoDownloadSize
	if cap <= 0 || size <= cap {
		return state.Materialized
	}
	return state.NotMaterialized
}
