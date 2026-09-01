package mirror

import (
	"context"
	"io"
	"time"

	"github.com/YourDoritos/pdrive/internal/drive"
)

// Source is the remote side of the mirror.
//
// The reconciler talks to this interface and never to Proton directly, so the
// whole engine is testable with no network and no account — which is the only
// practical way to write the adversarial data-loss tests that Phase 2 needs.
// *drive.Drive is the production implementation.
type Source interface {
	// RootLinkID identifies the Drive root folder.
	RootLinkID() string
	// VolumeID identifies the volume whose events describe this Drive.
	VolumeID() string
	// ShareID identifies the main share.
	ShareID() string

	// LatestEventID returns the current position of the event stream.
	LatestEventID(ctx context.Context) (string, error)
	// PollEvents returns everything that happened after cursor.
	PollEvents(ctx context.Context, cursor string) (*drive.Delta, error)

	// Walk traverses the whole tree. First run and refresh only.
	Walk(ctx context.Context, fn drive.WalkFunc) error
	// ListDir lists exactly one directory.
	ListDir(ctx context.Context, linkID, prefix string) ([]drive.Node, error)

	// Download opens a file's active revision for reading.
	Download(ctx context.Context, linkID string) (io.ReadCloser, int64, error)

	// Upload writes a new file or a new revision, returning its link ID.
	Upload(ctx context.Context, parentLinkID, name string, modTime time.Time, r io.Reader) (string, error)
	// Mkdir creates a folder and returns its link ID.
	Mkdir(ctx context.Context, parentLinkID, name string) (string, error)
	// Trash moves a node to Proton's trash. pDrive never deletes permanently.
	Trash(ctx context.Context, linkID string, isDir bool) error
	// Move relocates or renames a node without re-uploading its content.
	Move(ctx context.Context, linkID, newParentID, newName string, isDir bool) error
}

// Compile-time proof that the real client satisfies the interface.
var _ Source = (*drive.Drive)(nil)
