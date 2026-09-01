package drive

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Upload writes a new file, or a new revision of an existing one, into the
// given parent folder. It returns the link ID of the resulting node.
//
// modTime is stored in the node's encrypted extended attribute, so the local
// modification time survives the round trip — which is what lets the
// reconciler use mtime as a cheap change signal rather than hashing
// everything on every pass.
func (d *Drive) Upload(ctx context.Context, parentLinkID, name string, modTime time.Time, r io.Reader) (string, error) {
	started := time.Now()
	linkID, _, err := d.pd.UploadFileByReader(ctx, parentLinkID, name, modTime, r, 0)
	d.metrics.add(&d.metrics.UploadCalls, &d.metrics.UploadTime, started)
	if err != nil {
		return "", fmt.Errorf("upload %q: %w", name, err)
	}
	return linkID, nil
}

// Mkdir creates a folder and returns its link ID.
func (d *Drive) Mkdir(ctx context.Context, parentLinkID, name string) (string, error) {
	started := time.Now()
	linkID, err := d.pd.CreateNewFolderByID(ctx, parentLinkID, name)
	d.metrics.add(&d.metrics.MutateCalls, &d.metrics.MutateTime, started)
	if err != nil {
		return "", fmt.Errorf("create folder %q: %w", name, err)
	}
	return linkID, nil
}

// Trash moves a node to Proton's trash.
//
// pdrive never permanently deletes anything remotely. Proton's trash is the
// user's last line of recovery from a reconciliation mistake, and giving that
// up to save an API call would be a bad trade.
func (d *Drive) Trash(ctx context.Context, linkID string, isDir bool) error {
	started := time.Now()
	var err error
	if isDir {
		// onlyOnEmpty=false: a folder deleted locally is deleted with its
		// contents, and everything inside it goes to the trash too.
		err = d.pd.MoveFolderToTrashByID(ctx, linkID, false)
	} else {
		err = d.pd.MoveFileToTrashByID(ctx, linkID)
	}
	d.metrics.add(&d.metrics.MutateCalls, &d.metrics.MutateTime, started)
	if err != nil {
		return fmt.Errorf("trash %s: %w", linkID, err)
	}
	return nil
}

// Move relocates or renames a node.
//
// Used when a local file turns out to have been moved rather than deleted and
// recreated, which saves re-uploading its entire content.
func (d *Drive) Move(ctx context.Context, linkID, newParentID, newName string, isDir bool) error {
	started := time.Now()
	var err error
	if isDir {
		err = d.pd.MoveFolderByID(ctx, linkID, newParentID, newName)
	} else {
		err = d.pd.MoveFileByID(ctx, linkID, newParentID, newName)
	}
	d.metrics.add(&d.metrics.MutateCalls, &d.metrics.MutateTime, started)
	if err != nil {
		return fmt.Errorf("move %s: %w", linkID, err)
	}
	return nil
}
