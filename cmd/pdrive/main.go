// Command pdrive is the terminal UI for the pdrive Proton Drive sync client.
//
// pdrive is an unofficial, third-party application. It is not affiliated with
// or endorsed by Proton AG.
package main

import (
	"fmt"
	"os"

	"github.com/YourDoritos/pdrive/internal/api"
	"github.com/YourDoritos/pdrive/internal/config"
	"github.com/YourDoritos/pdrive/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Printf("pdrive %s\n", version)
			return
		case "paths":
			fmt.Println(config.DebugPaths())
			return
		case "backup":
			if err := cmdBackup(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "pdrive backup: %v\n", err)
				os.Exit(1)
			}
			return
		case "sync":
			if err := cmdSync(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "pdrive sync: %v\n", err)
				os.Exit(1)
			}
			return
		case "get":
			if err := cmdGet(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "pdrive get: %v\n", err)
				os.Exit(1)
			}
			return
		case "conflicts":
			if err := cmdConflicts(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "pdrive conflicts: %v\n", err)
				os.Exit(1)
			}
			return
		case "status":
			if err := cmdStatus(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "pdrive status: %v\n", err)
				os.Exit(1)
			}
			return
		case "verify":
			if err := cmdVerify(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "pdrive verify: %v\n", err)
				os.Exit(1)
			}
			return
		case "help", "--help", "-h":
			usage()
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
			usage()
			os.Exit(2)
		}
	}

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "pdrive: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`pdrive — Proton Drive sync for Linux (unofficial)

usage:
  pdrive                  launch the terminal UI
  pdrive sync [flags]     sync the folder both ways
  pdrive get <path>       download a file left as a stub by the size cap
  pdrive status           show what the mirror holds (offline)
  pdrive conflicts        list preserved local copies (offline)
  pdrive backup [flags]   make a separate verified archive of the account
  pdrive verify <dir>     re-check an existing backup (offline)
  pdrive version          print the version
  pdrive paths            print resolved config/state paths
  pdrive help             show this message

sync flags:
  --full                 force a full tree walk instead of replaying events
  --down-only            download only; never modify the Proton Drive account
  --confirm-deletions    proceed past the deletion-cliff guard for one pass
  --quiet                only print the summary

backup flags:
  --out DIR    destination (default ~/pdrive-backup-<date>)
  --resume     continue an interrupted backup
  --quiet      only print the summary

This is a third-party application not officially supported by Proton.
`)
}

func run() error {
	if err := config.EnsureDirs(); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	store, err := api.NewSessionStore(config.SessionFile())
	if err != nil {
		return err
	}

	// A session file that fails to decrypt is not fatal: the machine-id may
	// have changed, or the file may be truncated. Drop it and log in again.
	session, err := store.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pdrive: discarding unreadable session (%v)\n", err)
		_ = store.Delete()
		session = nil
	}

	client := api.NewClient(session)

	// Proton rotates the refresh token on every use. Persist immediately or
	// the session is lost on the next start.
	client.OnTokenRefresh = func(uid, accessToken, refreshToken string) {
		current, loadErr := store.Load()
		if loadErr != nil || current == nil {
			current = &api.Session{}
		}
		current.UID = uid
		current.AccessToken = accessToken
		current.RefreshToken = refreshToken
		if current.LoginEmail == "" {
			current.LoginEmail = client.LoginEmail()
		}
		_ = store.Save(current)
	}

	app := tui.NewApp(cfg, client, store, session != nil && session.AccessToken != "")

	prog := tea.NewProgram(app, tea.WithAltScreen())
	_, err = prog.Run()
	return err
}
