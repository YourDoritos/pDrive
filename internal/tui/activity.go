package tui

import (
	"fmt"
	"sort"
	"time"

	"github.com/YourDoritos/pdrive/internal/ipc"
	"github.com/charmbracelet/lipgloss"
)

// maxActivityLines caps what is held in memory for display. The daemon keeps
// the durable history; this is only the window being shown.
const maxActivityLines = 500

// ActivityModel shows what is moving now and what has moved before.
//
// The history comes from the daemon, not from this model: a log kept in the
// UI is empty every time the UI opens, which is exactly when someone wants to
// know what happened while they were not watching.
type ActivityModel struct {
	width, height int

	lines     []activityLine
	transfers map[string]*ipc.TransferData
	offset    int
	loaded    bool
}

type activityLine struct {
	at   time.Time
	kind string
	path string
	size int64
}

// NewActivityModel builds the activity screen.
func NewActivityModel() ActivityModel {
	return ActivityModel{transfers: map[string]*ipc.TransferData{}}
}

// SetSize records the terminal dimensions.
func (m *ActivityModel) SetSize(w, h int) { m.width, m.height = w, h }

// SetHistory installs the log fetched from the daemon.
//
// Stored oldest-first, rendered newest-first: the most recent thing is what
// someone opening this screen wants to see, without scrolling.
func (m *ActivityModel) SetHistory(log *ipc.ActivityLog) {
	if log == nil {
		return
	}
	m.loaded = true

	m.lines = m.lines[:0]
	for _, e := range log.Entries {
		at, err := time.Parse(time.RFC3339, e.At)
		if err != nil {
			at = time.Now()
		}
		m.lines = append(m.lines, activityLine{
			at: at, kind: e.Kind, path: e.Path, size: e.Size,
		})
	}
	m.trim()

	// Replace rather than merge: this is a fresh authoritative snapshot, and
	// a transfer that finished while we were not looking must not linger.
	m.transfers = map[string]*ipc.TransferData{}
	for i := range log.Transfers {
		t := log.Transfers[i]
		m.transfers[transferKey(t)] = &t
	}
}

// Add records one live event.
func (m *ActivityModel) Add(a ipc.ActivityData) {
	if a.Kind == "skip" {
		return
	}
	at := time.Now()
	if a.At != "" {
		if parsed, err := time.Parse(time.RFC3339, a.At); err == nil {
			at = parsed
		}
	}

	m.lines = append(m.lines, activityLine{at: at, kind: a.Kind, path: a.Path, size: a.Size})
	m.trim()
	if m.offset > 0 {
		m.offset++ // keep the view pinned while scrolled back
	}
}

// UpdateTransfer applies a live transfer event.
func (m *ActivityModel) UpdateTransfer(t ipc.TransferData) {
	if m.transfers == nil {
		m.transfers = map[string]*ipc.TransferData{}
	}
	key := transferKey(t)
	if t.Finished {
		delete(m.transfers, key)
		return
	}
	if existing, ok := m.transfers[key]; ok {
		existing.Done, existing.Total = t.Done, t.Total
		return
	}
	copied := t
	m.transfers[key] = &copied
}

func transferKey(t ipc.TransferData) string {
	if t.Up {
		return "up:" + t.Path
	}
	return "down:" + t.Path
}

func (m *ActivityModel) trim() {
	if len(m.lines) > maxActivityLines {
		m.lines = m.lines[len(m.lines)-maxActivityLines:]
	}
}

// ScrollUp moves back through the history.
func (m *ActivityModel) ScrollUp(n int) {
	m.offset += n
	if top := len(m.lines) - 1; m.offset > top {
		if top < 0 {
			top = 0
		}
		m.offset = top
	}
}

// ScrollDown moves toward the newest line.
func (m *ActivityModel) ScrollDown(n int) {
	m.offset -= n
	if m.offset < 0 {
		m.offset = 0
	}
}

// Clear empties the displayed log.
func (m *ActivityModel) Clear() {
	m.lines = nil
	m.offset = 0
}

// View renders the activity screen.
func (m ActivityModel) View() string {
	var rows []string

	if inflight := m.renderTransfers(); len(inflight) > 0 {
		rows = append(rows, StyleSubtitle.Render("Transferring"), "")
		rows = append(rows, inflight...)
		rows = append(rows, "")
	}

	switch {
	case len(m.lines) > 0:
		title := fmt.Sprintf("History (%d)", len(m.lines))
		if m.offset > 0 {
			title += StyleWarning.Render(fmt.Sprintf("   %d newer above", m.offset))
		}
		rows = append(rows, StyleSubtitle.Render(title), "")
		visible := m.visible()
		// Newest at the top.
		for i := len(visible) - 1; i >= 0; i-- {
			rows = append(rows, m.renderLine(visible[i]))
		}
	case !m.loaded:
		rows = append(rows, StyleDim.Render("  Loading…"))
	default:
		rows = append(rows,
			StyleDim.Render("  Nothing here yet."),
			"",
			StyleDim.Render("  Files you add, change or delete will show up here,"),
			StyleDim.Render("  on this computer or anywhere else."))
	}

	rows = append(rows, "",
		StyleHelp.Render("↑/↓ j/k: scroll  s: sync  c: clear  1-4: tabs  q: quit"))

	style := StyleActiveBox
	if len(m.lines) == 0 && len(m.transfers) == 0 {
		style = StyleBox
	}
	return CenterBox(m.width, m.height, style, lipgloss.JoinVertical(lipgloss.Left, rows...))
}

// renderTransfers draws a bar per transfer in flight.
func (m ActivityModel) renderTransfers() []string {
	if len(m.transfers) == 0 {
		return nil
	}

	keys := make([]string, 0, len(m.transfers))
	for k := range m.transfers {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, k := range keys {
		t := m.transfers[k]

		arrow := StyleSelected.Render("↓")
		if t.Up {
			arrow = lipgloss.NewStyle().Foreground(ColorSecondary).Render("↑")
		}

		// Everything on one line, sized to the panel: a transfer row that
		// wraps turns the list into a wall of half-lines.
		name := truncate(baseName(t.Path), 21)
		bar := QuotaBar(t.Done, t.Total, 10)

		line := fmt.Sprintf("  %s %-21s %s  %s",
			arrow, name, bar, StyleDim.Render(bytePair(t.Done, t.Total)))
		if rate := transferRate(t); rate != "" {
			line += StyleDim.Render("  " + rate)
		}
		out = append(out, line)
	}
	return out
}

// transferRate estimates throughput from the elapsed time.
func transferRate(t *ipc.TransferData) string {
	if t.Started == "" || t.Done <= 0 {
		return ""
	}
	started, err := time.Parse(time.RFC3339, t.Started)
	if err != nil {
		return ""
	}
	elapsed := time.Since(started).Seconds()
	// Below a second the estimate is dominated by startup and reads as noise.
	if elapsed < 1 {
		return ""
	}
	return humanBytes(int64(float64(t.Done)/elapsed)) + "/s"
}

// bytePair renders progress compactly, sharing the unit when both sides use
// it: "1.8/4.0 GiB" rather than "1.8 GiB / 4.0 GiB".
func bytePair(done, total int64) string {
	if total <= 0 {
		return humanBytes(done)
	}
	d, du := scaleBytes(done)
	tt, tu := scaleBytes(total)
	if du == tu {
		return fmt.Sprintf("%.1f/%.1f %s", d, tt, tu)
	}
	return fmt.Sprintf("%s/%s", humanBytes(done), humanBytes(total))
}

func scaleBytes(n int64) (float64, string) {
	const unit = 1024
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	v := float64(n)
	i := 0
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return v, units[i]
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

// visible returns the slice of lines that fits the window.
func (m ActivityModel) visible() []activityLine {
	rows := m.height - 10 - 2*len(m.transfers)
	if rows < 3 {
		rows = 3
	}
	if rows > 24 {
		rows = 24
	}

	end := len(m.lines) - m.offset
	if end < 0 {
		end = 0
	}
	start := end - rows
	if start < 0 {
		start = 0
	}
	return m.lines[start:end]
}

func (m ActivityModel) renderLine(l activityLine) string {
	label, style := activityLabel(l.kind)

	stamp := l.at.Format("15:04:05")
	// A log that survives restarts spans days, so the day matters once the
	// entry is not from today.
	if time.Since(l.at) > 12*time.Hour {
		stamp = l.at.Format("02 Jan 15:04")
	}

	line := StyleDim.Render("  "+fmt.Sprintf("%-12s", stamp)) + " " +
		style.Width(9).Render(label) + " " +
		StyleValue.Render(truncate(l.path, m.pathWidth()))
	if l.size > 0 {
		line += StyleDim.Render("  " + humanBytes(l.size))
	}
	return line
}

func (m ActivityModel) pathWidth() int {
	return BoxWidth - 38
}

// activityLabel maps an event kind to a short label and a colour, so a glance
// separates "content moved" from "something went wrong".
func activityLabel(kind string) (string, lipgloss.Style) {
	switch kind {
	case "download":
		return "download", lipgloss.NewStyle().Foreground(ColorAccent)
	case "upload":
		return "upload", lipgloss.NewStyle().Foreground(ColorSecondary)
	case "move":
		return "move", lipgloss.NewStyle().Foreground(ColorPrimary)
	case "delete":
		return "removed", StyleWarning
	case "trash-remote":
		return "trashed", StyleWarning
	case "stub":
		return "stub", StyleDim
	case "dir", "mkdir-remote":
		return "folder", StyleDim
	case "conflict":
		return "CONFLICT", StyleError
	case "warning":
		return "warning", StyleError
	}
	return kind, StyleDim
}
