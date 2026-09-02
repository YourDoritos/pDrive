// Command pdrived is the pDrive sync daemon.
//
// It runs as your user, not root: nothing it does needs privilege. The one
// privileged component is pdrive-gate, which is optional and separate.
//
// pDrive is an unofficial, third-party application, not affiliated with or
// endorsed by Proton AG.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/YourDoritos/pdrive/internal/config"
	"github.com/YourDoritos/pdrive/internal/daemon"
)

var version = "dev"

func main() {
	foreground := false
	for _, arg := range os.Args[1:] {
		switch arg {
		case "-f", "--foreground":
			foreground = true
		case "-v", "--version", "version":
			fmt.Printf("pdrived %s\n", version)
			return
		case "-h", "--help", "help":
			fmt.Print(`pdrived — the pDrive sync daemon (unofficial)

usage:
  pdrived [-f]

  -f, --foreground   also log to stderr

Runs as your user. Log: ~/.local/state/pdrive/pdrive.log
`)
			return
		default:
			fmt.Fprintf(os.Stderr, "pdrived: unknown argument %q\n", arg)
			os.Exit(2)
		}
	}

	if err := run(foreground); err != nil {
		fmt.Fprintf(os.Stderr, "pdrived: %v\n", err)
		os.Exit(1)
	}
}

func run(foreground bool) error {
	if err := config.EnsureDirs(); err != nil {
		return err
	}

	log := daemon.NewLogger(config.LogFile(), foreground)
	defer log.Close()
	log.Infof("pdrived %s starting", version)

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	d, err := daemon.New(ctx, log)
	if err != nil {
		return err
	}
	defer d.Close()

	root := d.Config().SyncRoot()
	if err := os.MkdirAll(root, 0700); err != nil {
		return fmt.Errorf("create sync folder: %w", err)
	}

	// Local changes and directory listings both come from one inotify watch.
	watcher, err := daemon.NewWatcher(root, log,
		func() {
			// A local change may have created folders; the gate needs marks
			// for them before anyone lists them.
			d.GateRescan()
			d.Trigger(daemon.TriggerLocal)
		},
		func() { d.Trigger(daemon.TriggerListing) },
	)
	if err != nil {
		log.Warnf("could not watch %s (%v); falling back to polling only", root, err)
	} else {
		defer watcher.Close()
		log.Infof("watching %s (%d directories)", root, watcher.Watched())
		go watcher.Run(ctx)
	}

	go d.WatchResume(ctx)
	go d.ConnectGate(ctx)

	errc := make(chan error, 1)
	go func() { errc <- d.Serve(ctx, config.SocketPath()) }()
	go func() { errc <- d.Run(ctx) }()

	select {
	case <-ctx.Done():
		log.Infof("shutting down")
		return nil
	case err := <-errc:
		if err != nil {
			return err
		}
		<-ctx.Done()
		return nil
	}
}
