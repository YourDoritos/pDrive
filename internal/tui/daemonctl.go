package tui

import (
	"context"

	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// startDaemon starts pdrived through systemd.
//
// Printing the command and making the user leave the TUI to type it was
// unnecessary friction: the TUI already knows the daemon is down, and this is
// a user service, so no privilege is involved.
func startDaemon() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		// A freshly installed unit file is invisible to systemd until it is
		// reloaded, which is exactly the state a first run is in.
		_ = exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload").Run()

		out, err := exec.CommandContext(ctx,
			"systemctl", "--user", "start", "pdrived").CombinedOutput()
		if err != nil {
			detail := strings.TrimSpace(string(out))
			if detail == "" {
				detail = err.Error()
			}
			return flashMsg{text: "could not start pdrived: " + firstLine(detail)}
		}
		return daemonStartedMsg{}
	}
}

// enableDaemon makes the daemon start at login as well as now.
func enableDaemon() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		_ = exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload").Run()

		out, err := exec.CommandContext(ctx,
			"systemctl", "--user", "enable", "--now", "pdrived").CombinedOutput()
		if err != nil {
			detail := strings.TrimSpace(string(out))
			if detail == "" {
				detail = err.Error()
			}
			return flashMsg{text: "could not enable pdrived: " + firstLine(detail)}
		}
		return daemonStartedMsg{}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
