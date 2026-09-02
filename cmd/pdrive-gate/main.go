// Command pdrive-gate makes directory listings in the sync folder wait until
// pDrive has caught that directory up.
//
// It is the only part of pDrive that needs privilege, because fanotify
// permission events require CAP_SYS_ADMIN. It is therefore kept deliberately
// small: it holds a fanotify descriptor and a socket, and does no network
// I/O, no cryptography, and holds no credentials.
//
// It never denies an open; it only ever delays one, and every error path
// releases the tree entirely. Killing it releases any held listing
// immediately, because the kernel allows outstanding permission events when
// the descriptor closes.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/YourDoritos/pdrive/internal/gate"
)

var version = "dev"

type stderrLogger struct{}

func (stderrLogger) logf(level, format string, v ...interface{}) {
	fmt.Fprintf(os.Stderr, "%s %-5s %s\n",
		time.Now().Format("2006-01-02 15:04:05"), level, fmt.Sprintf(format, v...))
}

func (l stderrLogger) Infof(f string, v ...interface{})  { l.logf("INFO", f, v...) }
func (l stderrLogger) Warnf(f string, v ...interface{})  { l.logf("WARN", f, v...) }
func (l stderrLogger) Errorf(f string, v ...interface{}) { l.logf("ERROR", f, v...) }

func main() {
	deadline := 400 * time.Millisecond

	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "-v", "--version", "version":
			fmt.Printf("pdrive-gate %s\n", version)
			return
		case "-h", "--help", "help":
			fmt.Print(`pdrive-gate — blocking freshness for pDrive (unofficial)

usage:
  pdrive-gate [--deadline 400ms]

Requires root: fanotify permission events need CAP_SYS_ADMIN.
The gate never denies an open, only delays one, and fails open on every
error. It is safe to stop at any time; pDrive falls back to polling.
`)
			return
		case "--deadline":
			if i+1 >= len(os.Args) {
				fmt.Fprintln(os.Stderr, "pdrive-gate: --deadline needs a value")
				os.Exit(2)
			}
			i++
			d, err := time.ParseDuration(os.Args[i])
			if err != nil {
				fmt.Fprintf(os.Stderr, "pdrive-gate: bad deadline %q: %v\n", os.Args[i], err)
				os.Exit(2)
			}
			deadline = d
		default:
			fmt.Fprintf(os.Stderr, "pdrive-gate: unknown argument %q\n", os.Args[i])
			os.Exit(2)
		}
	}

	if err := run(deadline); err != nil {
		fmt.Fprintf(os.Stderr, "pdrive-gate: %v\n", err)
		os.Exit(1)
	}
}

func run(deadline time.Duration) error {
	log := stderrLogger{}

	if os.Geteuid() != 0 {
		return fmt.Errorf("must run as root (fanotify permission events need CAP_SYS_ADMIN)")
	}

	srv, err := gate.NewServer(log, deadline)
	if err != nil {
		return err
	}
	defer srv.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go srv.Run(ctx)

	log.Infof("pdrive-gate %s started", version)

	errc := make(chan error, 1)
	go func() { errc <- srv.Listen(ctx, gate.SocketPath) }()

	select {
	case <-ctx.Done():
		allowed, held, timedOut := srv.Stats()
		log.Infof("stopping: %d listings seen, %d held, %d released on deadline",
			allowed, held, timedOut)
		return nil
	case err := <-errc:
		return err
	}
}
