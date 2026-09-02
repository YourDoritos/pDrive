package drive

import (
	"testing"

	proton "github.com/rclone/go-proton-api"
)

// The bug this guards against: a remote create arrived with State absent from
// the payload, which decodes as LinkStateDraft (the zero value). Classifying
// "not active" as a deletion turned every new remote file into a delete for an
// ID the database had never seen, and it was dropped silently.
func TestClassifyEvent(t *testing.T) {
	cases := []struct {
		name  string
		event proton.LinkEvent
		want  ChangeKind
		ok    bool
	}{
		{
			name: "create with State absent (decodes as Draft, the zero value)",
			event: proton.LinkEvent{
				EventType: proton.LinkEventCreate,
				Link:      proton.Link{LinkID: "l1"}, // State unset
			},
			want: ChangeUpsert, ok: true,
		},
		{
			name: "create with an explicit active state",
			event: proton.LinkEvent{
				EventType: proton.LinkEventCreate,
				Link:      proton.Link{LinkID: "l1", State: proton.LinkStateActive},
			},
			want: ChangeUpsert, ok: true,
		},
		{
			name: "update of an active file",
			event: proton.LinkEvent{
				EventType: proton.LinkEventUpdate,
				Link:      proton.Link{LinkID: "l1", State: proton.LinkStateActive},
			},
			want: ChangeUpsert, ok: true,
		},
		{
			name: "metadata update with State absent",
			event: proton.LinkEvent{
				EventType: proton.LinkEventUpdateMetadata,
				Link:      proton.Link{LinkID: "l1"},
			},
			want: ChangeUpsert, ok: true,
		},
		{
			name: "an explicit delete event",
			event: proton.LinkEvent{
				EventType: proton.LinkEventDelete,
				Link:      proton.Link{LinkID: "l1"},
			},
			want: ChangeDelete, ok: true,
		},
		{
			name: "trashing, which arrives as an update",
			event: proton.LinkEvent{
				EventType: proton.LinkEventUpdate,
				Link:      proton.Link{LinkID: "l1", State: proton.LinkStateTrashed},
			},
			want: ChangeDelete, ok: true,
		},
		{
			name: "a deleted state on an update",
			event: proton.LinkEvent{
				EventType: proton.LinkEventUpdate,
				Link:      proton.Link{LinkID: "l1", State: proton.LinkStateDeleted},
			},
			want: ChangeDelete, ok: true,
		},
		{
			name: "a draft link, mid-upload, is not a deletion",
			event: proton.LinkEvent{
				EventType: proton.LinkEventCreate,
				Link:      proton.Link{LinkID: "l1", State: proton.LinkStateDraft},
			},
			want: ChangeUpsert, ok: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := classifyEvent(c.event)
			if ok != c.ok {
				t.Fatalf("handled = %v, want %v", ok, c.ok)
			}
			if ok && got != c.want {
				t.Errorf("kind = %v, want %v", got, c.want)
			}
		})
	}
}

// Pin the constant this all hinges on. If Draft ever stops being the zero
// value the reasoning above changes.
func TestLinkStateDraftIsTheZeroValue(t *testing.T) {
	var unset proton.LinkState
	if unset != proton.LinkStateDraft {
		t.Fatal("LinkStateDraft is no longer the zero value; revisit classifyEvent")
	}
	if proton.LinkStateActive == unset {
		t.Fatal("LinkStateActive is now the zero value; revisit classifyEvent")
	}
}
