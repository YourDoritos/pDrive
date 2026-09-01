package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/YourDoritos/pdrive/internal/api"
	"github.com/YourDoritos/pdrive/internal/backup"
	"github.com/YourDoritos/pdrive/internal/config"
	"github.com/YourDoritos/pdrive/internal/drive"
)

// cmdBackup mirrors the account to disk and verifies the copy.
//
// Phase 0.5 of the plan, and a hard gate: nothing in pdrive writes to a
// Proton Drive account until this has produced a clean verification. It only
// lists and downloads — no call it makes can modify the account.
func cmdBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	out := fs.String("out", "", "destination directory (default ~/pdrive-backup-<date>)")
	resume := fs.Bool("resume", false, "continue an interrupted backup, skipping files already present and intact")
	quiet := fs.Bool("quiet", false, "only print the summary")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dest := *out
	if dest == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home directory: %w", err)
		}
		dest = filepath.Join(home, "pdrive-backup-"+time.Now().Format("2006-01-02"))
	}
	dest = config.ExpandPath(dest)

	if err := checkDest(dest, *resume); err != nil {
		return err
	}

	session, store, err := loadSession()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("Opening Proton Drive as %s…\n", session.LoginEmail)
	d, err := openDrive(ctx, session, store, nil)
	if err != nil {
		return err
	}
	defer d.Close()

	var warnings int
	opts := backup.Options{
		Dest:    dest,
		Account: session.LoginEmail,
		Resume:  *resume,
		Progress: func(ev backup.Event) {
			switch ev.Kind {
			case backup.EventWarn:
				warnings++
				fmt.Fprintf(os.Stderr, "  ! %s: %v\n", ev.Path, ev.Err)
			case backup.EventDir:
				if !*quiet {
					fmt.Printf("  dir   %s/\n", ev.Path)
				}
			case backup.EventSkip:
				if !*quiet {
					fmt.Printf("  skip  %s (%s, already intact)\n", ev.Path, humanBytes(ev.Size))
				}
			case backup.EventFile:
				if !*quiet {
					fmt.Printf("  get   %s (%s)\n", ev.Path, humanBytes(ev.Size))
				}
			}
		},
	}

	fmt.Printf("Backing up to %s\n\n", dest)
	started := time.Now()

	manifest, err := backup.Run(ctx, d, opts)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}

	fmt.Printf("\nDownloaded %d files (%s) and %d folders in %s.\n",
		manifest.Files, humanBytes(manifest.Bytes), manifest.Dirs, time.Since(started).Round(time.Second))
	fmt.Printf("Manifests written: %s, %s\n\n", backup.ManifestName, backup.ChecksumName)

	fmt.Println("Verifying every file against the manifest…")
	res, err := backup.Verify(dest, nil)
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	reportVerify(res, warnings)

	if !res.Clean() {
		return fmt.Errorf("backup did NOT verify clean — do not proceed to any write-path testing")
	}
	fmt.Printf("\nBackup verified clean. Independent re-check available with:\n  cd %s && sha1sum -c %s\n",
		dest, backup.ChecksumName)
	return nil
}

// cmdVerify re-hashes an existing backup. Offline: it never touches Proton.
func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	dest := fs.Arg(0)
	if dest == "" {
		return fmt.Errorf("usage: pdrive verify <backup-directory>")
	}
	dest = config.ExpandPath(dest)

	m, err := backup.LoadManifest(dest)
	if err != nil {
		return err
	}
	fmt.Printf("Backup of %s taken %s: %d files, %s\n\n",
		m.Account, m.CreatedAt.Local().Format("2006-01-02 15:04"), m.Files, humanBytes(m.Bytes))

	res, err := backup.Verify(dest, nil)
	if err != nil {
		return err
	}
	reportVerify(res, 0)

	if !res.Clean() {
		return fmt.Errorf("backup did NOT verify clean")
	}
	fmt.Println("\nBackup verified clean.")
	return nil
}

func reportVerify(res *backup.VerifyResult, warnings int) {
	fmt.Printf("  checked %d files, %d intact\n", res.Checked, res.OK)

	if warnings > 0 {
		fmt.Printf("  %d warning(s) during download\n", warnings)
	}
	for _, p := range res.Missing {
		fmt.Printf("  MISSING   %s\n", p)
	}
	for _, p := range res.Mismatch {
		fmt.Printf("  CORRUPT   %s (on-disk content does not match its recorded hash)\n", p)
	}
	for _, p := range res.RemoteMismatch {
		fmt.Printf("  SUSPECT   %s (downloaded content disagreed with Proton's own digest)\n", p)
	}
}

// loadSession reads the stored session and refuses to continue without the
// key passphrase, which is what actually decrypts the account.
func loadSession() (*api.Session, *api.SessionStore, error) {
	store, err := api.NewSessionStore(config.SessionFile())
	if err != nil {
		return nil, nil, err
	}
	session, err := store.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("read session: %w", err)
	}
	if session == nil || session.AccessToken == "" {
		return nil, nil, fmt.Errorf("not logged in — run `pdrive` first")
	}
	if session.SaltedKeyPass == "" {
		return nil, nil, fmt.Errorf("session predates key storage — run `pdrive`, log out and back in")
	}
	return session, store, nil
}

// openDrive opens an authenticated Drive session with token rotation wired to
// the session store.
//
// Proton issues a new refresh token on every refresh and invalidates the old
// one at once. If the new pair is not written back, the running process keeps
// working from memory and the *next* invocation fails with 10013. Every path
// that opens a Drive must go through here.
func openDrive(ctx context.Context, session *api.Session, store *api.SessionStore, log drive.Logger) (*drive.Drive, error) {
	return drive.Open(ctx, drive.Options{
		Session: session,
		Log:     log,
		OnAuth: func(uid, accessToken, refreshToken string) {
			if err := store.UpdateTokens(uid, accessToken, refreshToken); err != nil {
				fmt.Fprintf(os.Stderr, "pdrive: WARNING: could not persist refreshed tokens: %v\n", err)
				fmt.Fprintf(os.Stderr, "        the next run will need a fresh login\n")
			}
		},
		OnDeauth: func() {
			// The session is revoked server-side. Remove it so the next run
			// prompts for a login rather than replaying a dead token.
			_ = store.Delete()
		},
	})
}

// checkDest refuses to write into a directory that already holds something,
// so a second backup can never quietly half-overwrite the first.
func checkDest(dest string, resume bool) error {
	entries, err := os.ReadDir(dest)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", dest, err)
	}
	if len(entries) == 0 || resume {
		return nil
	}
	return fmt.Errorf("%s already exists and is not empty\n"+
		"       pass --resume to continue it, or --out to choose another directory", dest)
}

// humanBytes formats a byte count with binary units.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
