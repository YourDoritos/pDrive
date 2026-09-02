package drive

import (
	"context"
	"fmt"
	"time"

	proton "github.com/rclone/go-proton-api"
)

// ChangeKind classifies a remote change.
type ChangeKind int

const (
	// ChangeUpsert means a node was created, moved, or had its content or
	// metadata updated.
	ChangeUpsert ChangeKind = iota
	// ChangeDelete means a node was removed or trashed.
	ChangeDelete
)

// Change is one remote change drawn from the event stream.
type Change struct {
	Kind     ChangeKind
	LinkID   string
	ParentID string
	IsDir    bool
}

// Delta is the result of one event-cursor poll.
type Delta struct {
	// Cursor is the new event ID to store and poll from next time.
	Cursor string
	// Refresh is set when Proton tells us its event history is no longer
	// sufficient and the client must resynchronise from scratch. Ignoring it
	// silently desynchronises the mirror.
	Refresh bool
	Changes []Change
}

// VolumeID returns the volume whose events describe this account's Drive.
func (d *Drive) VolumeID() string { return d.pd.VolumeID() }

// ShareID returns the main share ID.
func (d *Drive) ShareID() string { return d.pd.ShareID() }

// LatestEventID returns the current position of the event stream. Used to
// anchor the cursor after a full mirror, so the next poll returns only what
// changed afterwards.
func (d *Drive) LatestEventID(ctx context.Context) (string, error) {
	volumeID := d.pd.VolumeID()
	if volumeID == "" {
		return "", fmt.Errorf("no volume ID available")
	}
	id, err := d.pd.Client().GetLatestVolumeEventID(ctx, volumeID)
	if err != nil {
		return "", fmt.Errorf("get latest event id: %w", err)
	}
	return id, nil
}

// PollEvents fetches everything that happened after cursor.
//
// This is the only sanctioned way to stay in sync. The Proton Drive
// integration rules are explicit that clients must not poll the API or walk
// the file tree repeatedly, and that doing so may get the application and the
// account rate-limited. A full Walk belongs to first-run mirroring and to a
// server-requested refresh, nothing else.
func (d *Drive) PollEvents(ctx context.Context, cursor string) (*Delta, error) {
	volumeID := d.pd.VolumeID()
	if volumeID == "" {
		return nil, fmt.Errorf("no volume ID available")
	}
	if cursor == "" {
		return nil, fmt.Errorf("no event cursor — a full mirror must run first")
	}

	started := time.Now()
	ev, err := d.pd.Client().GetVolumeEvent(ctx, volumeID, cursor)
	d.metrics.add(&d.metrics.EventPolls, &d.metrics.EventTime, started)
	if err != nil {
		return nil, fmt.Errorf("poll events: %w", err)
	}

	delta := &Delta{Cursor: ev.EventID, Refresh: bool(ev.Refresh)}
	if delta.Cursor == "" {
		delta.Cursor = cursor
	}

	for _, e := range ev.Events {
		kind, ok := classifyEvent(e)
		if !ok {
			continue
		}
		delta.Changes = append(delta.Changes, Change{
			Kind:     kind,
			LinkID:   e.Link.LinkID,
			ParentID: e.Link.ParentLinkID,
			IsDir:    e.Link.Type == proton.LinkTypeFolder,
		})
	}

	return delta, nil
}

// ListDir lists one directory. prefix is the directory's own path, used to
// build the returned nodes' paths; pass "" for the Drive root.
//
// Unlike Walk this touches exactly one directory, which is what makes it safe
// to call in response to an event.
func (d *Drive) ListDir(ctx context.Context, linkID, prefix string) ([]Node, error) {
	started := time.Now()
	children, err := d.pd.ListDirectory(ctx, linkID)
	d.metrics.add(&d.metrics.ListCalls, &d.metrics.ListTime, started)
	if err != nil {
		return nil, fmt.Errorf("list %q: %w", pathOrRoot(prefix), err)
	}

	out := make([]Node, 0, len(children))
	for _, child := range children {
		path := child.Name
		if prefix != "" {
			path = prefix + "/" + child.Name
		}

		node := Node{
			LinkID:   child.Link.LinkID,
			ParentID: linkID,
			Name:     child.Name,
			Path:     path,
			IsDir:    child.IsFolder,
		}
		if !child.IsFolder {
			d.fillAttrs(ctx, child.Link, &node)
		}
		out = append(out, node)
	}
	return out, nil
}

// LinkParent resolves a node's parent folder.
//
// Used when an event does not carry ParentLinkID: without a parent there is
// no directory to re-list, and the change would be dropped.
func (d *Drive) LinkParent(ctx context.Context, linkID string) (string, error) {
	link, err := d.pd.GetLink(ctx, linkID)
	if err != nil {
		return "", fmt.Errorf("resolve parent of %s: %w", linkID, err)
	}
	return link.ParentLinkID, nil
}

// classifyEvent decides whether one Drive event is a creation/update or a
// removal. Pure, so the rule below can be tested directly against the payload
// shapes Proton actually sends.
//
// The rule: only an EXPLICIT trashed or deleted state counts as a removal.
//
// Testing for "not active" instead looks equivalent and is not.
// LinkStateDraft is the zero value, so an event whose State field is absent
// decodes as Draft — and every newly created remote file was classified as a
// deletion, looked up by an ID the database had never seen, and dropped
// without trace. Remote deletions kept working, so the symptom was a folder
// where things vanished on request but never appeared.
//
// A genuine draft is a file mid-upload with no committed content. Treating it
// as an upsert is harmless: ListDirectory returns active links only, so a
// draft does not show up in the re-listing that follows.
func classifyEvent(e proton.LinkEvent) (ChangeKind, bool) {
	switch e.EventType {
	case proton.LinkEventDelete:
		return ChangeDelete, true

	case proton.LinkEventCreate, proton.LinkEventUpdate, proton.LinkEventUpdateMetadata:
		// Trashing arrives as an Update, not a Delete, so the state still has
		// to be inspected or a trashed file would sit on disk forever.
		if e.Link.State == proton.LinkStateTrashed || e.Link.State == proton.LinkStateDeleted {
			return ChangeDelete, true
		}
		return ChangeUpsert, true
	}
	return 0, false
}
