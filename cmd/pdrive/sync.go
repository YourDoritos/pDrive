package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/YourDoritos/pdrive/internal/config"
	"github.com/YourDoritos/pdrive/internal/drive"
	"github.com/YourDoritos/pdrive/internal/ipc"
	"github.com/YourDoritos/pdrive/internal/mirror"
	"github.com/YourDoritos/pdrive/internal/state"
)

// cmdSync brings the local folder up to date with Proton Drive.
//
// Phase 1 is download-only: it reads from Drive and writes locally, and calls
// nothing that mutates the account. Local changes are not uploaded yet.
func cmdSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	full := fs.Bool("full", false, "force a full tree walk instead of replaying events")
	downOnly := fs.Bool("down-only", false, "download only; never modify the Proton Drive account")
	confirm := fs.Bool("confirm-deletions", false, "proceed past the deletion-cliff guard for this pass")
	quiet := fs.Bool("quiet", false, "only print the summary")
	timing := fs.Bool("timing", false, "print a breakdown of where the time went")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	started := time.Now()

	// Prefer the daemon when it is running: it holds an open Drive session
	// and a warm connection, so it skips the six bootstrap API calls a cold
	// invocation pays.
	//
	// Only when it is syncing the same folder, though. The socket lives in
	// $XDG_RUNTIME_DIR, which does not move when someone points
	// XDG_CONFIG_HOME at a different configuration — so delegating blindly
	// meant `pdrive sync` could report on, and act on, a folder the caller
	// never asked about.
	if !*timing && daemonServes(cfg.SyncRoot()) {
		return syncViaDaemon(*full, *downOnly, *confirm, started)
	}

	m, db, d, err := openMirror(ctx, mirror.Options{
		ConfirmDeletions: *confirm,
		Progress:         syncProgress(*quiet),
	})
	if err != nil {
		return err
	}
	defer db.Close()
	defer d.Close()

	fmt.Printf("Syncing %s\n\n", cfg.SyncRoot())

	var res *mirror.Result
	switch {
	case *full:
		res, err = m.FullMirror(ctx)
	case *downOnly:
		res, err = m.Sync(ctx)
	default:
		res, err = m.SyncBoth(ctx)
	}
	if err != nil {
		var guard *mirror.ErrGuard
		if errors.As(err, &guard) {
			// Guards are a considered refusal, not a crash. Say so plainly.
			return fmt.Errorf("%w\n\nNothing was changed", guard)
		}
		return err
	}

	fmt.Printf("\n")
	if res.FullMirror {
		fmt.Printf("Full mirror in %s.\n", time.Since(started).Round(time.Second))
	} else {
		fmt.Printf("Event replay in %s.\n", time.Since(started).Round(time.Second))
	}
	fmt.Printf("  down: %d downloaded (%s), %d stubbed, %d unchanged, %d removed\n",
		res.Downloaded, humanBytes(res.Bytes), res.Stubbed, res.Skipped, res.Deleted)
	if res.Uploaded > 0 || res.Moved > 0 || res.TrashedRemote > 0 || res.Dirs > 0 {
		fmt.Printf("  up:   %d uploaded (%s), %d moved, %d trashed, %d folders\n",
			res.Uploaded, humanBytes(res.UploadedBytes), res.Moved, res.TrashedRemote, res.Dirs)
	}
	if res.Conflicts > 0 {
		fmt.Printf("  %d conflict(s) — both versions kept; see `pdrive conflicts`\n", res.Conflicts)
	}
	if res.Warnings > 0 {
		fmt.Printf("  %d warning(s)\n", res.Warnings)
	}
	if *timing {
		printTiming(d.Metrics().Snapshot(), time.Since(started))
	}
	return nil
}

// printTiming breaks a pass down by API call.
//
// The Phase 3 gate may hold a directory listing for at most max_block_ms, so
// it matters a great deal which of these costs are paid once at startup (the
// daemon pays them once and holds the connection) and which are paid per
// change (those are the ones inside the budget).
func printTiming(m drive.MetricsSnapshot, total time.Duration) {
	fmt.Printf("\nTiming\n")
	row := func(label string, calls int, d time.Duration) {
		if calls == 0 {
			return
		}
		fmt.Printf("  %-22s %5d call(s)  %8s total  %8s avg\n",
			label, calls, d.Round(time.Millisecond), (d / time.Duration(calls)).Round(time.Millisecond))
	}
	row("open drive (startup)", m.OpenCalls, m.OpenTime)
	row("event poll", m.EventPolls, m.EventTime)
	row("list directory", m.ListCalls, m.ListTime)
	row("revision attributes", m.AttrCalls, m.AttrTime)
	row("download", m.DownloadCalls, m.DownloadTime)
	row("upload", m.UploadCalls, m.UploadTime)
	row("create/move/trash", m.MutateCalls, m.MutateTime)

	api := m.OpenTime + m.EventTime + m.ListTime + m.AttrTime + m.DownloadTime +
		m.UploadTime + m.MutateTime
	fmt.Printf("  %-22s %8s total: %s in API calls, %s local\n", "",
		total.Round(time.Millisecond), api.Round(time.Millisecond),
		(total - api).Round(time.Millisecond))

	// The number that matters for the gate: everything except one-time
	// connection setup.
	perChange := m.EventTime + m.ListTime + m.AttrTime
	fmt.Printf("  %-22s %8s  (event + list + attrs, excludes startup and downloads)\n",
		"gate-relevant cost", perChange.Round(time.Millisecond))
}

// daemonServes reports whether a running daemon is syncing this exact folder.
func daemonServes(root string) bool {
	c, err := ipc.Dial(config.SocketPath())
	if err != nil {
		return false
	}
	defer c.Close()

	st, err := c.Status()
	if err != nil {
		return false
	}
	return st.Root == root
}

// syncViaDaemon runs the pass in pdrived, which already has a warm session.
func syncViaDaemon(full, downOnly, confirm bool, started time.Time) error {
	c, err := ipc.Dial(config.SocketPath())
	if err != nil {
		return err
	}
	defer c.Close()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	fmt.Printf("Syncing %s (via pdrived)\n\n", cfg.SyncRoot())

	res, err := c.Sync(ipc.SyncParams{
		Full: full, DownOnly: downOnly, ConfirmDeletions: confirm,
	})
	if err != nil {
		return err
	}

	fmt.Printf("Done in %s.\n", time.Since(started).Round(time.Millisecond))
	fmt.Printf("  down: %d downloaded (%s), %d stubbed, %d removed\n",
		res.Downloaded, humanBytes(res.Bytes), res.Stubbed, res.Deleted)
	if res.Uploaded > 0 || res.Moved > 0 || res.TrashedRemote > 0 {
		fmt.Printf("  up:   %d uploaded (%s), %d moved, %d trashed\n",
			res.Uploaded, humanBytes(res.UploadedBytes), res.Moved, res.TrashedRemote)
	}
	if res.Conflicts > 0 {
		fmt.Printf("  %d conflict(s) — both versions kept; see `pdrive conflicts`\n", res.Conflicts)
	}
	if res.Warnings > 0 {
		fmt.Printf("  %d warning(s)\n", res.Warnings)
	}
	return nil
}

// cmdGet downloads a file that was left as a stub by the size cap.
func cmdGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := fs.Arg(0)
	if path == "" {
		return fmt.Errorf("usage: pdrive get <path-inside-the-sync-folder>")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	m, db, d, err := openMirror(ctx, mirror.Options{Progress: syncProgress(false)})
	if err != nil {
		return err
	}
	defer db.Close()
	defer d.Close()

	if err := m.Materialize(ctx, path); err != nil {
		return err
	}
	fmt.Printf("Downloaded %s\n", path)
	return nil
}

// cmdStatus reports what the mirror currently holds. Offline: it reads only
// the local state database.
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	db, err := state.Open(config.StateDB())
	if err != nil {
		return err
	}
	defer db.Close()

	stats, err := db.Stats()
	if err != nil {
		return err
	}
	account, _ := db.GetMeta(state.KeyAccount)
	cursor, _ := db.GetMeta(state.KeyEventCursor)
	lastScan, _ := db.GetMeta(state.KeyLastFullScan)

	if account == "" {
		account = "(not synced yet)"
	}

	fmt.Printf("Account      %s\n", account)
	fmt.Printf("Folder       %s\n", cfg.SyncRoot())
	fmt.Printf("Tracked      %d files, %d folders\n", stats.Files, stats.Dirs)
	fmt.Printf("On disk      %s of %s\n", humanBytes(stats.MaterialBytes), humanBytes(stats.Bytes))
	if stats.Stubs > 0 {
		fmt.Printf("Stubs        %d file(s) over the size cap — fetch with `pdrive get <path>`\n", stats.Stubs)
	}
	if cursor == "" {
		fmt.Printf("Sync state   no cursor yet — run `pdrive sync`\n")
	} else {
		fmt.Printf("Sync state   event cursor held; last full walk %s\n", orNever(lastScan))
	}
	return nil
}

func orNever(s string) string {
	if s == "" {
		return "never"
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Local().Format("2006-01-02 15:04")
	}
	return s
}

// openMirror wires up session, Drive, state DB and Mirror.
func openMirror(ctx context.Context, opts mirror.Options) (*mirror.Mirror, *state.DB, *drive.Drive, error) {
	session, store, err := loadSession()
	if err != nil {
		return nil, nil, nil, err
	}

	cfg, err := config.Load()
	if err != nil {
		return nil, nil, nil, err
	}
	opts.Config = cfg

	db, err := state.Open(config.StateDB())
	if err != nil {
		return nil, nil, nil, err
	}

	d, err := openDrive(ctx, session, store, nil)
	if err != nil {
		db.Close()
		return nil, nil, nil, err
	}

	if err := db.SetMeta(state.KeyAccount, session.LoginEmail); err != nil {
		db.Close()
		return nil, nil, nil, err
	}

	m, err := mirror.New(d, db, opts)
	if err != nil {
		db.Close()
		return nil, nil, nil, err
	}
	return m, db, d, nil
}

func syncProgress(quiet bool) func(mirror.Event) {
	return func(ev mirror.Event) {
		switch ev.Kind {
		case mirror.EventWarn:
			fmt.Fprintf(os.Stderr, "  ! %s: %v\n", ev.Path, ev.Err)
		case mirror.EventDir:
			if !quiet {
				fmt.Printf("  dir    %s/\n", ev.Path)
			}
		case mirror.EventDownload:
			if !quiet {
				fmt.Printf("  get    %s (%s)\n", ev.Path, humanBytes(ev.Size))
			}
		case mirror.EventStub:
			if !quiet {
				fmt.Printf("  stub   %s (%s, over the size cap)\n", ev.Path, humanBytes(ev.Size))
			}
		case mirror.EventSkip:
			if !quiet {
				fmt.Printf("  ok     %s\n", ev.Path)
			}
		case mirror.EventDelete:
			fmt.Printf("  gone   %s (moved to local trash)\n", ev.Path)
		case mirror.EventUpload:
			if !quiet {
				fmt.Printf("  put    %s (%s)\n", ev.Path, humanBytes(ev.Size))
			}
		case mirror.EventMkdirRemote:
			if !quiet {
				fmt.Printf("  mkdir  %s/ (remote)\n", ev.Path)
			}
		case mirror.EventMove:
			fmt.Printf("  move   %s\n", ev.Path)
		case mirror.EventTrashRemote:
			fmt.Printf("  trash  %s (moved to Proton trash)\n", ev.Path)
		case mirror.EventConflict:
			fmt.Printf("  CONFLICT %s\n", ev.Path)
		}
	}
}

// cmdConflicts lists preserved local copies. Offline.
func cmdConflicts(args []string) error {
	fs := flag.NewFlagSet("conflicts", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := state.Open(config.StateDB())
	if err != nil {
		return err
	}
	defer db.Close()

	conflicts, err := db.Conflicts()
	if err != nil {
		return err
	}
	if len(conflicts) == 0 {
		fmt.Println("No conflicts recorded.")
		return nil
	}

	fmt.Printf("%d conflict(s). Both versions were kept — nothing was discarded.\n\n", len(conflicts))
	for _, c := range conflicts {
		fmt.Printf("  %s\n", c.Path)
		fmt.Printf("    remote version is at that path; your local copy was kept as:\n")
		fmt.Printf("    %s\n", c.KeptLocal)
		if !c.At.IsZero() {
			fmt.Printf("    (%s)\n", c.At.Local().Format("2006-01-02 15:04"))
		}
		fmt.Println()
	}
	return nil
}
