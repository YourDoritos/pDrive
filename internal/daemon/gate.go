package daemon

import (
	"context"
	"net"
	"os"
	"time"

	"github.com/YourDoritos/pdrive/internal/gate"
	"github.com/YourDoritos/pdrive/internal/ipc"
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
		go func(id uint64) {
			d.refreshForListing(ctx)
			_ = conn.Send(&gate.Message{Type: gate.TypeAck, ID: id})
		}(msg.ID)
	}
}

// refreshForListing brings the tree up to date for a held directory listing.
//
// Two things keep this inside the gate's budget. First, a recent poll is
// enough: inside fresh_window the answer is already known and no request is
// made at all, which is what stops `ls` in a loop from hammering the API.
// Second, this runs the pull half only. A full bidirectional pass would scan
// and hash the local tree, and a listing must never wait on that.
func (d *Daemon) refreshForListing(ctx context.Context) {
	if d.Fresh() || d.IsPaused() {
		return
	}

	// Bounded well inside the gate's own deadline: if this cannot finish in
	// time the gate releases the listing anyway, and a request still in
	// flight would only waste work.
	budget := time.Duration(d.cfg.Freshness.MaxBlockMS) * time.Millisecond
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	m, err := mirror.New(d.drive, d.db, mirror.Options{
		Config: d.cfg,
		Progress: func(ev mirror.Event) {
			d.publish(ipc.EventActivity, ipc.ActivityData{
				Kind: activityKind(ev.Kind), Path: ev.Path, Size: ev.Size,
			})
		},
	})
	if err != nil {
		return
	}

	if _, err := m.Sync(ctx); err != nil {
		d.log.Debugf("listing refresh: %v", err)
		return
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
