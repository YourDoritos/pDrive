// Package daemon is pdrived: the long-lived process that keeps the sync
// folder and Proton Drive in agreement.
//
// Its reason to exist is latency. A cold `pdrive sync` spends most of its time
// in session bootstrap — six API calls before the one that does any work — and
// Proton's authenticated round trip is 55 ms warm against 140 ms cold. The
// daemon pays bootstrap once and holds a warm connection, which is what brings
// the steady-state cost inside the gate's blocking budget.
package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/YourDoritos/pdrive/internal/api"
	"github.com/YourDoritos/pdrive/internal/config"
	"github.com/YourDoritos/pdrive/internal/drive"
	"github.com/YourDoritos/pdrive/internal/gate"
	"github.com/YourDoritos/pdrive/internal/ipc"
	"github.com/YourDoritos/pdrive/internal/mirror"
	"github.com/YourDoritos/pdrive/internal/state"
)

// State is the daemon's current condition.
type State string

const (
	StateIdle    State = "idle"
	StateSyncing State = "syncing"
	StatePaused  State = "paused"
	StateError   State = "error"
)

// Trigger says why a sync was requested. Kept for logging: when syncs are
// firing more often than expected, the reason is the first thing to look at.
type Trigger string

const (
	TriggerStartup   Trigger = "startup"
	TriggerLocal     Trigger = "local-change"
	TriggerPoll      Trigger = "poll"
	TriggerListing   Trigger = "listing"
	TriggerResume    Trigger = "resume"
	TriggerManual    Trigger = "manual"
	TriggerNetworkUp Trigger = "network-up"
)

// Daemon owns the Drive session, the state database and the sync loop.
type Daemon struct {
	cfg   *config.Config
	db    *state.DB
	drive *drive.Drive
	store *api.SessionStore

	log *Logger

	mu        sync.RWMutex
	state     State
	lastSync  time.Time
	lastError string
	started   time.Time
	gate      bool
	lastPoll  time.Time

	// syncReq carries sync requests into the single loop that runs them.
	// Passes are serialised: two reconcilers on one tree would race each
	// other into conflicts of their own making.
	syncReq chan syncRequest
	// wake collapses redundant triggers. A burst of file writes should
	// produce one sync, not one per file.
	wake chan Trigger

	// gateConn is the live connection to pdrive-gate, when one exists.
	gateConn *gate.Conn

	subs   map[chan *ipc.Event]struct{}
	subsMu sync.Mutex
}

type syncRequest struct {
	trigger  Trigger
	opts     mirror.Options
	full     bool
	downOnly bool
	result   chan syncOutcome
}

type syncOutcome struct {
	res *mirror.Result
	err error
}

// New builds a daemon from an authenticated session.
func New(ctx context.Context, log *Logger) (*Daemon, error) {
	if err := config.EnsureDirs(); err != nil {
		return nil, err
	}

	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}

	store, err := api.NewSessionStore(config.SessionFile())
	if err != nil {
		return nil, err
	}
	session, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("read session: %w", err)
	}
	if session == nil || session.AccessToken == "" {
		return nil, fmt.Errorf("not logged in — run `pdrive` first")
	}
	if session.SaltedKeyPass == "" {
		return nil, fmt.Errorf("session has no key passphrase — log in again with `pdrive`")
	}

	db, err := state.Open(config.StateDB())
	if err != nil {
		return nil, err
	}

	d := &Daemon{
		cfg:     cfg,
		db:      db,
		store:   store,
		log:     log,
		state:   StateIdle,
		started: time.Now(),
		syncReq: make(chan syncRequest, 16),
		wake:    make(chan Trigger, 64),
		subs:    map[chan *ipc.Event]struct{}{},
	}

	dr, err := drive.Open(ctx, drive.Options{
		Session: session,
		Log:     log,
		OnAuth: func(uid, access, refresh string) {
			if err := store.UpdateTokens(uid, access, refresh); err != nil {
				log.Errorf("could not persist refreshed tokens: %v", err)
			}
		},
		OnDeauth: func() {
			log.Errorf("Proton revoked this session; run `pdrive` to log in again")
			_ = store.Delete()
		},
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	d.drive = dr

	if err := db.SetMeta(state.KeyAccount, session.LoginEmail); err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}

// Close releases the daemon's resources.
func (d *Daemon) Close() {
	if d.drive != nil {
		d.drive.Close()
	}
	if d.db != nil {
		d.db.Close()
	}
}

// Config returns the daemon's configuration.
func (d *Daemon) Config() *config.Config { return d.cfg }

// Run starts the sync loop and blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	go d.syncLoop(ctx)
	go d.pollLoop(ctx)

	d.Trigger(TriggerStartup)
	<-ctx.Done()
	return nil
}

// Trigger asks for a sync. Redundant triggers collapse: a burst of writes
// produces one pass, not one per file.
func (d *Daemon) Trigger(t Trigger) {
	select {
	case d.wake <- t:
	default: // already queued; nothing to add
	}
}

// syncLoop is the only place a sync runs, so passes never overlap.
func (d *Daemon) syncLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return

		case t := <-d.wake:
			// Collapse anything that piled up while we were waiting.
			d.drainWake()
			d.runSync(ctx, syncRequest{trigger: t})

		case req := <-d.syncReq:
			out := d.runSync(ctx, req)
			if req.result != nil {
				req.result <- out
			}
		}
	}
}

func (d *Daemon) drainWake() {
	for {
		select {
		case <-d.wake:
		default:
			return
		}
	}
}

// RequestSync runs a sync and waits for it, for an interactive client.
func (d *Daemon) RequestSync(ctx context.Context, p ipc.SyncParams) (*mirror.Result, error) {
	if d.IsPaused() {
		return nil, fmt.Errorf("syncing is paused; run `pdrivectl resume`")
	}

	result := make(chan syncOutcome, 1)
	req := syncRequest{
		trigger:  TriggerManual,
		full:     p.Full,
		downOnly: p.DownOnly,
		opts:     mirror.Options{ConfirmDeletions: p.ConfirmDeletions},
		result:   result,
	}

	select {
	case d.syncReq <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	select {
	case out := <-result:
		return out.res, out.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (d *Daemon) runSync(ctx context.Context, req syncRequest) syncOutcome {
	if d.IsPaused() && req.trigger != TriggerManual {
		return syncOutcome{}
	}

	d.setState(StateSyncing)
	d.publish(ipc.EventSyncStarted, nil)

	opts := req.opts
	opts.Config = d.cfg
	opts.Progress = func(ev mirror.Event) {
		d.publish(ipc.EventActivity, ipc.ActivityData{
			Kind: activityKind(ev.Kind), Path: ev.Path, Size: ev.Size,
		})
		if ev.Kind == mirror.EventWarn {
			d.log.Warnf("%s: %v", ev.Path, ev.Err)
		}
	}

	m, err := mirror.New(d.drive, d.db, opts)
	if err != nil {
		return d.finishSync(nil, err)
	}

	var res *mirror.Result
	switch {
	case req.full:
		res, err = m.FullMirror(ctx)
	case req.downOnly:
		res, err = m.Sync(ctx)
	default:
		res, err = m.SyncBoth(ctx)
	}

	d.mu.Lock()
	d.lastPoll = time.Now()
	d.mu.Unlock()

	return d.finishSync(res, err)
}

func (d *Daemon) finishSync(res *mirror.Result, err error) syncOutcome {
	d.mu.Lock()
	if err != nil {
		d.state = StateError
		d.lastError = err.Error()
	} else {
		d.state = StateIdle
		d.lastError = ""
		d.lastSync = time.Now()
	}
	d.mu.Unlock()

	if err != nil {
		d.log.Errorf("sync failed: %v", err)
	} else if res != nil {
		d.logResult(res)
		if res.Dirs > 0 {
			// New folders exist; the gate has no marks for them yet.
			d.GateRescan()
		}
	}

	var data ipc.SyncData
	if res != nil {
		data = ipc.SyncData{
			Downloaded: res.Downloaded, Uploaded: res.Uploaded, Moved: res.Moved,
			TrashedRemote: res.TrashedRemote, Deleted: res.Deleted, Stubbed: res.Stubbed,
			Conflicts: res.Conflicts, Warnings: res.Warnings,
			Bytes: res.Bytes, UploadedBytes: res.UploadedBytes, FullMirror: res.FullMirror,
		}
	}
	d.publish(ipc.EventSyncFinished, data)

	return syncOutcome{res: res, err: err}
}

func (d *Daemon) logResult(res *mirror.Result) {
	if res.Downloaded == 0 && res.Uploaded == 0 && res.Moved == 0 &&
		res.TrashedRemote == 0 && res.Deleted == 0 && res.Conflicts == 0 {
		return // nothing happened; not worth a line
	}
	d.log.Infof("sync: %d down, %d up, %d moved, %d trashed, %d removed, %d conflicts",
		res.Downloaded, res.Uploaded, res.Moved, res.TrashedRemote, res.Deleted, res.Conflicts)
}

// pollLoop drives the event cursor on an adaptive cadence: often while the
// user is active, rarely when they are not.
//
// The integration rules require event-based sync and forbid frequent tree
// traversal, so this polls a cursor delta and nothing else.
func (d *Daemon) pollLoop(ctx context.Context) {
	timer := time.NewTimer(d.cfg.Freshness.IdleInterval.D())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if !d.IsPaused() {
				d.Trigger(TriggerPoll)
			}
			timer.Reset(d.pollInterval())
		}
	}
}

// pollInterval is short while the user has been active recently and long
// otherwise.
func (d *Daemon) pollInterval() time.Duration {
	d.mu.RLock()
	last := d.lastSync
	d.mu.RUnlock()

	if !last.IsZero() && time.Since(last) < d.cfg.Freshness.ActiveWindow.D() {
		return d.cfg.Freshness.ActiveInterval.D()
	}
	return d.cfg.Freshness.IdleInterval.D()
}

// Fresh reports whether the event cursor was polled recently enough that a
// directory listing can be allowed without waiting.
//
// This is what stops `ls` in a loop from hammering the API: inside the
// window, the gate is answered from what we already know.
func (d *Daemon) Fresh() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return !d.lastPoll.IsZero() && time.Since(d.lastPoll) < d.cfg.Freshness.FreshWindow.D()
}

// --- state accessors ---

func (d *Daemon) setState(s State) {
	d.mu.Lock()
	d.state = s
	d.mu.Unlock()
}

// State returns the daemon's current state.
func (d *Daemon) State() State {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.state
}

// Pause stops automatic syncing.
func (d *Daemon) Pause() {
	d.mu.Lock()
	d.state = StatePaused
	d.mu.Unlock()
	d.log.Infof("syncing paused")
}

// Resume restarts automatic syncing.
func (d *Daemon) Resume() {
	d.mu.Lock()
	if d.state == StatePaused {
		d.state = StateIdle
	}
	d.mu.Unlock()
	d.log.Infof("syncing resumed")
	d.Trigger(TriggerManual)
}

// IsPaused reports whether automatic syncing is stopped.
func (d *Daemon) IsPaused() bool { return d.State() == StatePaused }

// SetGateActive records whether pdrive-gate is connected.
func (d *Daemon) SetGateActive(active bool) {
	d.mu.Lock()
	d.gate = active
	d.mu.Unlock()
}

// Status assembles a status report.
func (d *Daemon) Status() ipc.StatusData {
	d.mu.RLock()
	st := ipc.StatusData{
		State:      string(d.state),
		Root:       d.cfg.SyncRoot(),
		LastError:  d.lastError,
		GateActive: d.gate,
		Uptime:     time.Since(d.started).Round(time.Second).String(),
	}
	if !d.lastSync.IsZero() {
		st.LastSync = d.lastSync.Format(time.RFC3339)
	}
	d.mu.RUnlock()

	if stats, err := d.db.Stats(); err == nil {
		st.Files, st.Dirs, st.Stubs = stats.Files, stats.Dirs, stats.Stubs
		st.Bytes, st.OnDisk = stats.Bytes, stats.MaterialBytes
	}
	if account, err := d.db.GetMeta(state.KeyAccount); err == nil {
		st.Account = account
	}
	if conflicts, err := d.Conflicts(); err == nil {
		st.Conflicts = len(conflicts)
	}
	return st
}

// Conflicts lists preserved local copies that still exist.
//
// A conflict is resolved by the user dealing with the preserved copy —
// merging it, renaming it, or deleting it. Once that copy is gone there is
// nothing left to act on, so the record is dropped rather than nagging about
// it forever.
func (d *Daemon) Conflicts() ([]ipc.ConflictEntry, error) {
	rows, err := d.db.Conflicts()
	if err != nil {
		return nil, err
	}

	root := d.cfg.SyncRoot()
	out := make([]ipc.ConflictEntry, 0, len(rows))
	for _, c := range rows {
		if _, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(c.KeptLocal))); statErr != nil {
			_ = d.db.DeleteConflict(c.Path, c.KeptLocal)
			continue
		}
		e := ipc.ConflictEntry{Path: c.Path, KeptLocal: c.KeptLocal}
		if !c.At.IsZero() {
			e.At = c.At.Format(time.RFC3339)
		}
		out = append(out, e)
	}
	return out, nil
}

// Materialize downloads a stubbed file.
func (d *Daemon) Materialize(ctx context.Context, path string) error {
	opts := mirror.Options{Config: d.cfg}
	m, err := mirror.New(d.drive, d.db, opts)
	if err != nil {
		return err
	}
	return m.Materialize(ctx, path)
}

// --- event fan-out ---

// Subscribe returns a channel of daemon events and a function to stop it.
func (d *Daemon) Subscribe() (chan *ipc.Event, func()) {
	ch := make(chan *ipc.Event, 64)
	d.subsMu.Lock()
	d.subs[ch] = struct{}{}
	d.subsMu.Unlock()

	return ch, func() {
		d.subsMu.Lock()
		delete(d.subs, ch)
		close(ch)
		d.subsMu.Unlock()
	}
}

func (d *Daemon) publish(kind string, data interface{}) {
	evt := &ipc.Event{Type: kind}
	if data != nil {
		evt.Data = ipc.MarshalData(data)
	}

	d.subsMu.Lock()
	defer d.subsMu.Unlock()
	for ch := range d.subs {
		select {
		case ch <- evt:
		default: // a slow subscriber must never stall the sync loop
		}
	}
}

func activityKind(k mirror.EventKind) string {
	switch k {
	case mirror.EventDir:
		return "dir"
	case mirror.EventDownload:
		return "download"
	case mirror.EventUpload:
		return "upload"
	case mirror.EventStub:
		return "stub"
	case mirror.EventSkip:
		return "skip"
	case mirror.EventDelete:
		return "delete"
	case mirror.EventMove:
		return "move"
	case mirror.EventTrashRemote:
		return "trash-remote"
	case mirror.EventMkdirRemote:
		return "mkdir-remote"
	case mirror.EventConflict:
		return "conflict"
	case mirror.EventWarn:
		return "warning"
	}
	return "unknown"
}
