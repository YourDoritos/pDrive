package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/YourDoritos/pdrive/internal/gate"
	"github.com/YourDoritos/pdrive/internal/mirror"
)

// ConnectGate keeps a connection to pdrive-gate, reconnecting as needed.
//
// The gate is optional. If it is not installed, not running, or goes away,
// freshness falls back to inotify hints plus adaptive polling and everything
// still works — just with a stale window on the first listing.
func (d *Daemon) ConnectGate(ctx context.Context) {
	if !d.cfg.Freshness.Gate {
		d.log.Infof("gate disabled in config; using polling for freshness")
		return
	}

	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		connected := d.runGateSession(ctx)
		d.SetGateActive(false)
		if ctx.Err() != nil {
			return
		}
		if connected {
			// The session worked and then ended, which usually means the gate
			// was restarted. Start over from a short delay: without this the
			// backoff stays wherever it climbed to during first startup, and
			// a gate that comes back takes half a minute to be noticed.
			backoff = time.Second
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// runGateSession holds one gate connection. It reports whether the session
// was ever established, which decides whether the retry delay resets.
func (d *Daemon) runGateSession(ctx context.Context) bool {
	raw, err := net.DialTimeout("unix", gate.SocketPath, 2*time.Second)
	if err != nil {
		d.log.Debugf("gate unavailable: %v", err)
		return false
	}
	defer raw.Close()

	conn := gate.NewConn(raw)

	// The gate needs our pid so it can allow our own reads without asking us
	// about them, which would deadlock the sync loop against itself.
	if err := conn.Send(&gate.Message{
		Type: gate.TypeRegister,
		Root: d.cfg.SyncRoot(),
		PID:  os.Getpid(),
		// One source of truth for the budget. If the gate released earlier
		// than this, the refresh would be cancelled mid-request and the
		// listing would be neither fresh nor blocked.
		DeadlineMS: d.cfg.Freshness.MaxBlockMS,
	}); err != nil {
		return false
	}

	d.mu.Lock()
	d.gateConn = conn
	d.mu.Unlock()

	d.SetGateActive(true)
	d.log.Infof("gate connected: directory listings will block until fresh")
	defer func() {
		d.mu.Lock()
		d.gateConn = nil
		d.mu.Unlock()
		d.SetGateActive(false)
		d.log.Infof("gate disconnected")
	}()

	go func() {
		<-ctx.Done()
		raw.Close()
	}()

	for {
		msg, err := conn.Recv()
		if err != nil {
			return true
		}
		if msg.Type != gate.TypeOpened {
			continue
		}
		// Answer on its own goroutine: several listings can be held at once,
		// and one slow answer must not delay the others.
		go func(id uint64, path string) {
			d.refreshForListing(ctx, path)
			_ = conn.Send(&gate.Message{Type: gate.TypeAck, ID: id})
		}(msg.ID, msg.Path)
	}
}

// refreshForListing brings the tree up to date for a held directory listing.
//
// The freshness window here is deliberately short. It exists only to collapse
// a burst of listings — a file manager opening the same folder several times,
// or `ls` in a loop — into one API call. Set long, it defeats the entire
// point of the gate: a file uploaded a second ago on another device would be
// answered from cache and missed by the very listing that was blocked to
// catch it.
//
// Concurrent listings share one pass rather than each starting their own.
func (d *Daemon) refreshForListing(ctx context.Context, openedPath string) {
	if d.IsPaused() {
		return
	}
	if d.Fresh() {
		return
	}

	// Single-flight: several directories are often opened at once, and one
	// pass covers all of them.
	d.listingMu.Lock()
	if d.listingRun != nil {
		wait := d.listingRun
		d.listingMu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
		}
		return
	}
	done := make(chan struct{})
	d.listingRun = done
	d.listingMu.Unlock()

	defer func() {
		d.listingMu.Lock()
		d.listingRun = nil
		d.listingMu.Unlock()
		close(done)
	}()

	// Bounded by the gate's own deadline: past that the listing is released
	// anyway, and work still in flight would only be wasted.
	// Slightly under the gate's hold, so a refresh that is nearly done is not
	// cancelled by its own deadline at the same instant.
	budget := time.Duration(d.cfg.Freshness.MaxBlockMS) * time.Millisecond
	budget -= budget / 10
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	m, err := mirror.New(d.drive, d.db, mirror.Options{
		Config:   d.cfg,
		Progress: d.onMirrorEvent,
	})
	if err != nil {
		return
	}

	started := time.Now()

	// Ask about the directory actually being opened. Polling the event cursor
	// instead would miss a file uploaded moments ago, because Proton has not
	// published its event yet.
	rel, ok := d.relativeTo(openedPath)
	var refreshErr error
	if ok {
		refreshErr = m.RefreshDir(ctx, rel)
	} else {
		_, refreshErr = m.Sync(ctx)
	}
	res := m.LastResult()
	elapsed := time.Since(started)
	if refreshErr != nil {
		d.log.Debugf("listing refresh failed after %s: %v",
			elapsed.Round(time.Millisecond), refreshErr)
		return
	}
	// Logged at info when it actually did something: this is the number the
	// hold budget has to cover, and it is the only way to tell a budget that
	// is too small from a sync that is simply slow.
	if res.Downloaded > 0 || res.Deleted > 0 {
		d.log.Infof("listing refresh took %s (%d down, %d removed)",
			elapsed.Round(time.Millisecond), res.Downloaded, res.Deleted)
	} else {
		d.log.Debugf("listing refresh took %s", elapsed.Round(time.Millisecond))
	}

	d.mu.Lock()
	d.lastPoll = time.Now()
	d.mu.Unlock()
}

// GateRescan asks the gate to mark directories that have appeared since it
// last looked.
//
// Marks are per-inode, so a folder created after registration is invisible to
// the gate until it is told. Without this, listing a newly synced folder would
// silently fall back to whatever the poller had — the freshness guarantee
// would hold everywhere except the folders most likely to have just changed.
func (d *Daemon) GateRescan() {
	d.mu.RLock()
	conn := d.gateConn
	d.mu.RUnlock()

	if conn == nil {
		return
	}
	if err := conn.Send(&gate.Message{Type: gate.TypeRescan}); err != nil {
		d.log.Debugf("gate rescan: %v", err)
	}
}

// relativeTo converts an absolute path inside the sync folder to the
// slash-separated form the state database uses.
func (d *Daemon) relativeTo(abs string) (string, bool) {
	if abs == "" {
		return "", false
	}
	root := d.cfg.SyncRoot()
	if abs == root {
		return "", true
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}
