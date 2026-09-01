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
	confirm := fs.Bool("confirm-deletions", false, "proceed past the deletion-cliff guard for this pass")
	quiet := fs.Bool("quiet", false, "only print the summary")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	m, db, d, err := openMirror(ctx, mirror.Options{
		ConfirmDeletions: *confirm,
		Progress:         syncProgress(*quiet),
	})
	if err != nil {
		return err
	}
	defer db.Close()
	defer d.Close()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	fmt.Printf("Syncing %s\n\n", cfg.SyncRoot())

	started := time.Now()

	var res *mirror.Result
	if *full {
		res, err = m.FullMirror(ctx)
	} else {
		res, err = m.Sync(ctx)
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
	fmt.Printf("  %d downloaded (%s), %d stubbed, %d unchanged, %d removed, %d folders\n",
		res.Downloaded, humanBytes(res.Bytes), res.Stubbed, res.Skipped, res.Deleted, res.Dirs)
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
	session, err := loadSession()
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

	d, err := drive.Open(ctx, session, nil)
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
		}
	}
}
