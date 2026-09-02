// Command pdrivectl is the scriptable interface to pdrived.
//
// Output is deliberately terse and stable so it can be piped into a status
// bar. `pdrivectl status --short` is one line suitable for waybar or tmux.
//
// pDrive is an unofficial, third-party application, not affiliated with or
// endorsed by Proton AG.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/YourDoritos/pdrive/internal/config"
	"github.com/YourDoritos/pdrive/internal/ipc"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	cmd, args := os.Args[1], os.Args[2:]
	var err error

	switch cmd {
	case "status":
		err = cmdStatus(args)
	case "sync":
		err = cmdSync(args)
	case "pause":
		err = simple(func(c *ipc.Client) error { return c.Pause() }, "syncing paused")
	case "resume":
		err = simple(func(c *ipc.Client) error { return c.Resume() }, "syncing resumed")
	case "watch":
		err = cmdWatch(args)
	case "conflicts":
		err = cmdConflicts(args)
	case "get":
		err = cmdGet(args)
	case "version", "--version", "-v":
		fmt.Printf("pdrivectl %s\n", version)
		return
	case "help", "--help", "-h":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "pdrivectl: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "pdrivectl: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`pdrivectl — control the pDrive daemon (unofficial)

usage:
  pdrivectl status [--short|--json]   daemon and sync state
  pdrivectl sync [--full|--down-only] run a pass now and wait for it
  pdrivectl pause                     stop automatic syncing
  pdrivectl resume                    restart automatic syncing
  pdrivectl watch                     follow sync activity as it happens
  pdrivectl conflicts                 list preserved local copies
  pdrivectl get <path>                download a file left as a stub

--short prints one line, for a status bar.
`)
}

// connect reaches the daemon, with an actionable message when it is not up.
func connect() (*ipc.Client, error) {
	c, err := ipc.Dial(config.SocketPath())
	if err != nil {
		return nil, fmt.Errorf("pdrived is not running (start it with: systemctl --user start pdrived)")
	}
	return c, nil
}

func simple(fn func(*ipc.Client) error, msg string) error {
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()
	if err := fn(c); err != nil {
		return err
	}
	fmt.Println(msg)
	return nil
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	short := fs.Bool("short", false, "one line, for a status bar")
	asJSON := fs.Bool("json", false, "machine-readable")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := connect()
	if err != nil {
		if *short {
			// A status bar must always render something.
			fmt.Println("pDrive: off")
			return nil
		}
		return err
	}
	defer c.Close()

	st, err := c.Status()
	if err != nil {
		return err
	}

	switch {
	case *asJSON:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)

	case *short:
		icon := map[string]string{
			"idle": "✓", "syncing": "↕", "paused": "‖", "error": "!",
		}[st.State]
		if icon == "" {
			icon = "?"
		}
		line := fmt.Sprintf("pDrive %s %d files", icon, st.Files)
		if st.QuotaTotal > 0 {
			line += fmt.Sprintf(" %s", humanBytes(st.QuotaUsed))
		}
		if st.Conflicts > 0 {
			line += fmt.Sprintf(" (%d conflicts)", st.Conflicts)
		}
		fmt.Println(line)
		return nil
	}

	fmt.Printf("State        %s\n", st.State)
	if st.LastError != "" {
		fmt.Printf("Error        %s\n", st.LastError)
	}
	fmt.Printf("Account      %s\n", orDash(st.Account))
	fmt.Printf("Folder       %s\n", st.Root)
	fmt.Printf("Synced       %d files, %d folders (%s on disk)\n",
		st.Files, st.Dirs, humanBytes(st.OnDisk))
	// The account quota, not the size of the local mirror. Printing the
	// mirror against itself always reads as a full drive.
	if st.QuotaTotal > 0 {
		fmt.Printf("Drive        %s of %s used (%.1f%%)\n",
			humanBytes(st.QuotaUsed), humanBytes(st.QuotaTotal),
			float64(st.QuotaUsed)/float64(st.QuotaTotal)*100)
	}
	if st.Stubs > 0 {
		fmt.Printf("Stubs        %d over the size cap — `pdrivectl get <path>`\n", st.Stubs)
	}
	if st.Conflicts > 0 {
		fmt.Printf("Conflicts    %d — `pdrivectl conflicts`\n", st.Conflicts)
	}
	if st.GateActive {
		fmt.Printf("Freshness    gate active (listings block until current)\n")
	} else {
		fmt.Printf("Freshness    polling (pdrive-gate not connected)\n")
	}
	fmt.Printf("Last sync    %s\n", relTime(st.LastSync))
	fmt.Printf("Uptime       %s\n", orDash(st.Uptime))
	return nil
}

func cmdSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	full := fs.Bool("full", false, "force a full tree walk")
	downOnly := fs.Bool("down-only", false, "download only; do not modify the account")
	confirm := fs.Bool("confirm-deletions", false, "proceed past the deletion-cliff guard")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()

	res, err := c.Sync(ipc.SyncParams{
		Full: *full, DownOnly: *downOnly, ConfirmDeletions: *confirm,
	})
	if err != nil {
		return err
	}

	fmt.Printf("down: %d downloaded (%s), %d stubbed, %d removed\n",
		res.Downloaded, humanBytes(res.Bytes), res.Stubbed, res.Deleted)
	fmt.Printf("up:   %d uploaded (%s), %d moved, %d trashed\n",
		res.Uploaded, humanBytes(res.UploadedBytes), res.Moved, res.TrashedRemote)
	if res.Conflicts > 0 {
		fmt.Printf("%d conflict(s) — both versions kept; see `pdrivectl conflicts`\n", res.Conflicts)
	}
	if res.Warnings > 0 {
		fmt.Printf("%d warning(s) — see the log\n", res.Warnings)
	}
	return nil
}

// cmdWatch follows the daemon's event stream, like tail -f for sync activity.
func cmdWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Confirm the daemon is up first, so "nothing happening" is never
	// confused with "not connected".
	c, err := connect()
	if err != nil {
		return err
	}
	st, err := c.Status()
	c.Close()
	if err != nil {
		return err
	}
	fmt.Printf("watching %s (daemon %s) — ctrl+c to stop\n\n", st.Root, st.State)

	events, stop, err := ipc.Subscribe(config.SocketPath())
	if err != nil {
		return err
	}
	defer stop()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	for {
		select {
		case <-sig:
			return nil
		case evt, ok := <-events:
			if !ok {
				return fmt.Errorf("the daemon closed the event stream")
			}
			printEvent(evt)
		}
	}
}

func printEvent(evt *ipc.Event) {
	stamp := time.Now().Format("15:04:05")

	switch evt.Type {
	case ipc.EventSyncStarted:
		fmt.Printf("%s  sync started\n", stamp)
	case ipc.EventSyncFinished:
		var d ipc.SyncData
		if err := json.Unmarshal(evt.Data, &d); err != nil {
			return
		}
		if d.Downloaded == 0 && d.Uploaded == 0 && d.Moved == 0 &&
			d.TrashedRemote == 0 && d.Deleted == 0 && d.Conflicts == 0 {
			return // nothing happened; not worth a line
		}
		fmt.Printf("%s  sync finished: %d down, %d up, %d moved, %d trashed, %d conflicts\n",
			stamp, d.Downloaded, d.Uploaded, d.Moved, d.TrashedRemote, d.Conflicts)
	case ipc.EventActivity:
		var a ipc.ActivityData
		if err := json.Unmarshal(evt.Data, &a); err != nil {
			return
		}
		if a.Kind == "skip" {
			return
		}
		line := fmt.Sprintf("%s  %-12s %s", stamp, a.Kind, a.Path)
		if a.Size > 0 {
			line += "  " + humanBytes(a.Size)
		}
		fmt.Println(line)
	}
}

func cmdConflicts(args []string) error {
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()

	conflicts, err := c.Conflicts()
	if err != nil {
		return err
	}
	if len(conflicts) == 0 {
		fmt.Println("No conflicts recorded.")
		return nil
	}

	fmt.Printf("%d conflict(s). Both versions were kept — nothing was discarded.\n\n", len(conflicts))
	for _, cf := range conflicts {
		fmt.Printf("  %s\n", cf.Path)
		fmt.Printf("    remote version is at that path; your local copy was kept as:\n")
		fmt.Printf("    %s\n", cf.KeptLocal)
		if cf.At != "" {
			fmt.Printf("    (%s)\n", relTime(cf.At))
		}
		fmt.Println()
	}
	return nil
}

func cmdGet(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: pdrivectl get <path-inside-the-sync-folder>")
	}
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.Close()

	if err := c.Get(args[0]); err != nil {
		return err
	}
	fmt.Printf("Downloaded %s\n", args[0])
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func relTime(s string) string {
	if s == "" {
		return "never"
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return t.Local().Format("2006-01-02 15:04")
	}
}

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
