package tui

import (
	"fmt"

	"time"

	"github.com/YourDoritos/pdrive/internal/config"
	"github.com/YourDoritos/pdrive/internal/ipc"
	"github.com/charmbracelet/lipgloss"
)

// StatusModel is the main screen: what the daemon is doing and what the
// folder holds.
type StatusModel struct {
	width, height int
	cfg           *config.Config

	status   *ipc.StatusData
	daemonUp bool
	// syncing is driven by the event stream rather than by the polled status.
	// Polling samples a moment; a sync that takes 200 ms would either be
	// missed entirely or, once caught, be displayed for the whole poll
	// interval. Events say exactly when one starts and stops.
	syncing     bool
	streamAlive bool
	lastEvent   string
	spinner     int
}

// NewStatusModel builds the status screen.
func NewStatusModel(cfg *config.Config) StatusModel {
	return StatusModel{cfg: cfg}
}

// SetSize records the terminal dimensions.
func (m *StatusModel) SetSize(w, h int) { m.width, m.height = w, h }

// SetStatus installs the latest daemon status.
func (m *StatusModel) SetStatus(s *ipc.StatusData) {
	m.status = s
	m.daemonUp = s != nil
	// Without a live event stream the polled state is all there is.
	if s != nil && !m.streamAlive {
		m.syncing = s.State == "syncing"
	}
}

// SetSyncing records the real-time sync state from the event stream.
func (m *StatusModel) SetSyncing(syncing bool) {
	m.syncing = syncing
	m.streamAlive = true
}

// StreamLost falls back to the polled state for the sync indicator.
func (m *StatusModel) StreamLost() { m.streamAlive = false }

// SetDaemonDown records that the daemon is unreachable.
func (m *StatusModel) SetDaemonDown() {
	m.daemonUp = false
	m.status = nil
}

// SetActivity records the most recent activity line.
func (m *StatusModel) SetActivity(line string) { m.lastEvent = line }

// Tick advances the spinner.
func (m *StatusModel) Tick() { m.spinner++ }

// Syncing reports whether a pass is running, so the caller can animate only
// when there is something to animate.
func (m StatusModel) Syncing() bool { return m.syncing }

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// View renders the status screen.
func (m StatusModel) View() string {
	if !m.daemonUp || m.status == nil {
		return m.viewDaemonDown()
	}
	st := m.status

	state := m.renderState(st)

	rows := []string{
		row("Status", state),
		row("Account", StyleValue.Render(orDash(st.Account))),
		row("Folder", StyleValue.Render(st.Root)),
		"",
		row("Synced", StyleValue.Render(fmt.Sprintf("%d files, %d folders  (%s)",
			st.Files, st.Dirs, humanBytes(st.OnDisk)))),
	}

	// Account quota, not the size of the local mirror. Showing "572 KiB of
	// 572 KiB" under a heading of "Storage" read as a full drive; it was in
	// fact "everything tracked here is downloaded".
	if st.QuotaTotal > 0 {
		pct := float64(st.QuotaUsed) / float64(st.QuotaTotal) * 100
		rows = append(rows, row("Drive storage", fmt.Sprintf("%s  %s of %s (%.1f%%)",
			QuotaBar(st.QuotaUsed, st.QuotaTotal, 20),
			humanBytes(st.QuotaUsed), humanBytes(st.QuotaTotal), pct)))
	} else {
		rows = append(rows, row("Drive storage", StyleDim.Render("…")))
	}

	if st.Stubs > 0 {
		rows = append(rows, row("Not downloaded",
			StyleWarning.Render(fmt.Sprintf("%d over the size cap", st.Stubs))))
	}
	if st.Conflicts > 0 {
		rows = append(rows, row("Conflicts",
			StyleError.Render(fmt.Sprintf("%d — see the Conflicts tab", st.Conflicts))))
	}

	rows = append(rows,
		"",
		row("Freshness", m.renderFreshness(st)),
		row("Last sync", StyleDim.Render(relTime(st.LastSync))),
		row("Running for", StyleDim.Render(orDash(st.Uptime))),
	)

	if m.lastEvent != "" {
		rows = append(rows, "", row("Latest", StyleDim.Render(truncate(m.lastEvent, m.contentWidth()-20))))
	}

	rows = append(rows, "",
		StyleHelp.Render("s: sync  p: pause/resume  r: refresh  1-4: tabs  q: quit"))

	return CenterBox(m.width, m.height,
		StyleActiveBox, lipgloss.JoinVertical(lipgloss.Left, rows...))
}

func (m StatusModel) renderState(st *ipc.StatusData) string {
	if m.syncing && st.State != "paused" && st.State != "error" {
		frame := spinnerFrames[m.spinner%len(spinnerFrames)]
		return lipgloss.NewStyle().Foreground(ColorAccent).Render(frame + " syncing")
	}
	switch st.State {
	case "syncing":
		if !m.streamAlive {
			frame := spinnerFrames[m.spinner%len(spinnerFrames)]
			return lipgloss.NewStyle().Foreground(ColorAccent).Render(frame + " syncing")
		}
		return StyleSuccess.Render("✓ up to date")
	case "paused":
		return StyleWarning.Render("‖ paused")
	case "error":
		return StyleError.Render("! " + truncate(st.LastError, m.contentWidth()-20))
	default:
		return StyleSuccess.Render("✓ up to date")
	}
}

func (m StatusModel) renderFreshness(st *ipc.StatusData) string {
	if st.GateActive {
		return StyleSuccess.Render("instant — folders update as you open them")
	}
	if m.cfg != nil && !m.cfg.Freshness.Gate {
		return StyleDim.Render("checks periodically (turned off in settings)")
	}
	return StyleWarning.Render("polling — pdrive-gate not running")
}

func (m StatusModel) viewDaemonDown() string {
	rows := []string{
		row("Status", StyleError.Render("daemon not running")),
		"",
		StyleDim.Render("  Nothing is syncing right now."),
		StyleDim.Render("  Your files are safe. They just will not change until"),
		StyleDim.Render("  you start syncing again."),
		"",
		StyleSelected.Render("  s") + StyleValue.Render("  Start syncing"),
		StyleSelected.Render("  e") + StyleValue.Render("  Start syncing, and again at every login"),
	}
	if m.lastEvent != "" {
		rows = append(rows, "", StyleError.Render("  "+truncate(m.lastEvent, BoxWidth-6)))
	}
	rows = append(rows, "", StyleHelp.Render("s: start  e: enable  r: retry  q: quit"))

	return CenterBox(m.width, m.height, StyleBox, lipgloss.JoinVertical(lipgloss.Left, rows...))
}

// DaemonDown reports whether the daemon is unreachable, so the root model
// knows which meaning to give the s key.
func (m StatusModel) DaemonDown() bool { return !m.daemonUp }

func (m StatusModel) contentWidth() int {
	if m.width < 40 {
		return 40
	}
	return m.width
}

func row(label, value string) string {
	return StyleLabel.Render("  "+label) + " " + value
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func truncate(s string, max int) string {
	if max < 8 {
		max = 8
	}
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
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
	case d < 2*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return t.Local().Format("2006-01-02 15:04")
	}
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
