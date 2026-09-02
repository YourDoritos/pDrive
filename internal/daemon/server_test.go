package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/YourDoritos/pdrive/internal/config"
	"github.com/YourDoritos/pdrive/internal/ipc"
	"github.com/YourDoritos/pdrive/internal/mirror"
	"github.com/YourDoritos/pdrive/internal/state"
)

// newTestDaemon builds a daemon with only the parts the IPC surface needs.
// No Proton session and no Drive: this exercises the server, not the sync.
func newTestDaemon(t *testing.T) (*Daemon, string) {
	t.Helper()

	dir := t.TempDir()
	t.Cleanup(config.UseTestDirs(dir))

	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := config.DefaultConfig()
	cfg.Sync.Root = filepath.Join(dir, "files")
	cfg.Validate()
	if err := os.MkdirAll(cfg.SyncRoot(), 0700); err != nil {
		t.Fatal(err)
	}

	d := &Daemon{
		cfg:       cfg,
		db:        db,
		log:       NewLogger(filepath.Join(dir, "test.log"), false),
		state:     StateIdle,
		started:   time.Now(),
		syncReq:   make(chan syncRequest, 4),
		wake:      make(chan Trigger, 8),
		subs:      map[chan *ipc.Event]struct{}{},
		transfers: map[string]*ipc.TransferData{},
	}
	return d, filepath.Join(dir, "pdrive.sock")
}

func serve(t *testing.T, d *Daemon, socket string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = d.Serve(ctx, socket) }()

	// Wait for the listener rather than sleeping a fixed amount.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := ipc.Dial(socket); err == nil {
			c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server never started listening")
}

func TestStatusOverIPC(t *testing.T) {
	d, socket := newTestDaemon(t)
	serve(t, d, socket)

	c, err := ipc.Dial(socket)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != string(StateIdle) {
		t.Errorf("state = %q, want %q", st.State, StateIdle)
	}
}

// Regression: subscribing turned the connection into an event stream, but
// handleConn then closed it on the way out of its loop. The stream died
// within milliseconds, and because the TUI treated a closed stream as a dead
// daemon, a perfectly healthy daemon rendered as "not running".
func TestSubscribeKeepsTheConnectionOpen(t *testing.T) {
	d, socket := newTestDaemon(t)
	serve(t, d, socket)

	events, stop, err := ipc.Subscribe(socket)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer stop()

	// The daemon opens with a status frame.
	select {
	case evt, ok := <-events:
		if !ok {
			t.Fatal("the stream closed immediately after subscribing")
		}
		if evt == nil {
			t.Fatal("nil opening event")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no opening event")
	}

	// And must still be open long enough to carry a later one.
	time.Sleep(200 * time.Millisecond)
	d.publish(ipc.EventActivity, ipc.ActivityData{Kind: "upload", Path: "a.txt", Size: 7})

	select {
	case evt, ok := <-events:
		if !ok {
			t.Fatal("the stream closed before a later event could arrive")
		}
		if evt.Type != ipc.EventActivity {
			t.Fatalf("event type = %q, want %q", evt.Type, ipc.EventActivity)
		}
		var ad ipc.ActivityData
		if err := json.Unmarshal(evt.Data, &ad); err != nil {
			t.Fatal(err)
		}
		if ad.Path != "a.txt" || ad.Size != 7 {
			t.Errorf("activity = %+v", ad)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a published event never reached the subscriber")
	}
}

// Subscribing must not disturb ordinary request/response traffic.
func TestStatusStillWorksWhileSubscribed(t *testing.T) {
	d, socket := newTestDaemon(t)
	serve(t, d, socket)

	events, stop, err := ipc.Subscribe(socket)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer stop()
	<-events // opening frame

	c, err := ipc.Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	for i := 0; i < 3; i++ {
		if _, err := c.Status(); err != nil {
			t.Fatalf("status call %d while subscribed: %v", i, err)
		}
	}
}

func TestPauseAndResumeOverIPC(t *testing.T) {
	d, socket := newTestDaemon(t)
	serve(t, d, socket)

	c, err := ipc.Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if !d.IsPaused() {
		t.Error("daemon did not pause")
	}
	st, _ := c.Status()
	if st.State != string(StatePaused) {
		t.Errorf("status = %q, want paused", st.State)
	}

	if err := c.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if d.IsPaused() {
		t.Error("daemon did not resume")
	}
}

func TestUnknownCommandIsRejected(t *testing.T) {
	d, socket := newTestDaemon(t)
	serve(t, d, socket)

	raw, err := ipc.Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	// An unknown command must fail cleanly and leave the connection usable.
	if err := raw.Get("nope-not-a-path"); err == nil {
		t.Error("expected an error for a path the daemon does not track")
	}
	if _, err := raw.Status(); err != nil {
		t.Errorf("connection unusable after an error: %v", err)
	}
}

// A second daemon must not silently steal a live socket.
func TestServeRefusesWhenAlreadyRunning(t *testing.T) {
	d, socket := newTestDaemon(t)
	serve(t, d, socket)

	other, _ := newTestDaemon(t)
	err := other.Serve(context.Background(), socket)
	if err == nil {
		t.Fatal("a second daemon bound a socket that was already in use")
	}
}

// Regression: the polling cadence keyed off lastSync, which every successful
// pass updated — including a poll that found nothing. The "recently active"
// window therefore never expired and the daemon polled at the active interval
// (5 s) forever, syncing constantly on an idle account.
func TestPollIntervalDecaysWhenNothingHappens(t *testing.T) {
	d, _ := newTestDaemon(t)

	// A pass that moved something: fast cadence is correct here.
	d.finishSync(&mirror.Result{Uploaded: 1}, nil)
	if got := d.pollInterval(); got != d.cfg.Freshness.ActiveInterval.D() {
		t.Errorf("after a real change, interval = %v, want the active interval %v",
			got, d.cfg.Freshness.ActiveInterval.D())
	}

	// Passes that find nothing must not keep it there.
	for i := 0; i < 5; i++ {
		d.finishSync(&mirror.Result{}, nil)
	}

	d.mu.Lock()
	// Age the last real change past the activity window.
	d.lastChange = time.Now().Add(-2 * d.cfg.Freshness.ActiveWindow.D())
	d.mu.Unlock()

	if got := d.pollInterval(); got != d.cfg.Freshness.IdleInterval.D() {
		t.Errorf("with no changes for a while, interval = %v, want the idle interval %v",
			got, d.cfg.Freshness.IdleInterval.D())
	}
}

func TestChangedRecognisesRealWork(t *testing.T) {
	if changed(&mirror.Result{}) {
		t.Error("an empty result counted as activity")
	}
	if changed(&mirror.Result{Skipped: 100, Warnings: 2}) {
		t.Error("skips and warnings are not activity")
	}
	for name, res := range map[string]*mirror.Result{
		"downloaded": {Downloaded: 1},
		"uploaded":   {Uploaded: 1},
		"moved":      {Moved: 1},
		"trashed":    {TrashedRemote: 1},
		"deleted":    {Deleted: 1},
		"stubbed":    {Stubbed: 1},
		"conflict":   {Conflicts: 1},
	} {
		if !changed(res) {
			t.Errorf("%s did not count as activity", name)
		}
	}
}

// Engine events must reach the stored log, and transfers must be visible
// while they are in flight rather than only once they land.
func TestMirrorEventsBecomeHistoryAndTransfers(t *testing.T) {
	d, _ := newTestDaemon(t)

	// A transfer starting, moving, and finishing.
	d.onMirrorEvent(mirror.Event{Kind: mirror.EventTransferStart, Path: "big.bin", Size: 1000})
	log, err := d.Activity(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(log.Transfers) != 1 || log.Transfers[0].Path != "big.bin" {
		t.Fatalf("transfer not tracked: %+v", log.Transfers)
	}

	d.onMirrorEvent(mirror.Event{
		Kind: mirror.EventTransferProgress, Path: "big.bin", Size: 1000, Done: 400,
	})
	log, _ = d.Activity(0)
	if len(log.Transfers) != 1 || log.Transfers[0].Done != 400 {
		t.Errorf("progress not applied: %+v", log.Transfers)
	}

	d.onMirrorEvent(mirror.Event{Kind: mirror.EventTransferDone, Path: "big.bin"})
	log, _ = d.Activity(0)
	if len(log.Transfers) != 0 {
		t.Errorf("a finished transfer is still in flight: %+v", log.Transfers)
	}

	// A completed change is recorded.
	d.onMirrorEvent(mirror.Event{Kind: mirror.EventUpload, Path: "big.bin", Size: 1000})
	log, _ = d.Activity(0)
	if len(log.Entries) != 1 {
		t.Fatalf("history has %d entries, want 1", len(log.Entries))
	}
	if log.Entries[0].Kind != "upload" || log.Entries[0].Path != "big.bin" {
		t.Errorf("entry = %+v", log.Entries[0])
	}
}

// "skip" says a file was already correct. On a steady tree it is the
// overwhelming majority of events, and storing it would push everything
// meaningful out of a bounded log.
func TestSkipEventsAreNotRecorded(t *testing.T) {
	d, _ := newTestDaemon(t)

	for i := 0; i < 20; i++ {
		d.onMirrorEvent(mirror.Event{Kind: mirror.EventSkip, Path: "unchanged.txt"})
	}
	log, err := d.Activity(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(log.Entries) != 0 {
		t.Errorf("skips were recorded: %d entries", len(log.Entries))
	}
}

// An upload and a download of the same path can overlap during a conflict, so
// they must be tracked separately.
func TestUploadAndDownloadOfOnePathTrackSeparately(t *testing.T) {
	d, _ := newTestDaemon(t)

	d.onMirrorEvent(mirror.Event{Kind: mirror.EventTransferStart, Path: "notes.md", Size: 10})
	d.onMirrorEvent(mirror.Event{Kind: mirror.EventTransferStart, Path: "notes.md", Size: 20, Up: true})

	log, _ := d.Activity(0)
	if len(log.Transfers) != 2 {
		t.Fatalf("tracked %d transfers, want 2", len(log.Transfers))
	}

	d.onMirrorEvent(mirror.Event{Kind: mirror.EventTransferDone, Path: "notes.md"})
	log, _ = d.Activity(0)
	if len(log.Transfers) != 1 || !log.Transfers[0].Up {
		t.Errorf("finishing the download disturbed the upload: %+v", log.Transfers)
	}
}
