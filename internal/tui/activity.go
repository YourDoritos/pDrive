package tui

import (
	"fmt"
	"time"

	"github.com/YourDoritos/pdrive/internal/ipc"
	"github.com/charmbracelet/lipgloss"
)

// maxActivityLines caps the scrollback. A sync of a large tree emits one
// event per file; keeping all of them would grow without bound in a process
// that is expected to stay open for days.
const maxActivityLines = 500

// ActivityModel is a live log of what the daemon is doing.
type ActivityModel struct {
	width, height int
	lines         []activityLine
	offset        int // how far scrolled back from the newest line
}

type activityLine struct {
	at   time.Time
	kind string
	path string
	size int64
}

// NewActivityModel builds the activity screen.
func NewActivityModel() ActivityModel { return ActivityModel{} }

// SetSize records the terminal dimensions.
func (m *ActivityModel) SetSize(w, h int) { m.width, m.height = w, h }

// Add records one activity event.
func (m *ActivityModel) Add(a ipc.ActivityData) {
	// "skip" means a file was already correct. It is the overwhelming
	// majority of events on a steady tree and says nothing happened, so it is
	// dropped rather than burying the events that matter.
	if a.Kind == "skip" {
		return
	}

	m.lines = append(m.lines, activityLine{
		at: time.Now(), kind: a.Kind, path: a.Path, size: a.Size,
	})
	if len(m.lines) > maxActivityLines {
		m.lines = m.lines[len(m.lines)-maxActivityLines:]
	}
	// Following the tail is the useful default; scrolling back pins the view.
	if m.offset > 0 {
		m.offset++
	}
}

// ScrollUp moves back through the history.
func (m *ActivityModel) ScrollUp(n int) {
	m.offset += n
	if max := len(m.lines) - 1; m.offset > max {
		if max < 0 {
			max = 0
		}
		m.offset = max
	}
}

// ScrollDown moves toward the newest line.
func (m *ActivityModel) ScrollDown(n int) {
	m.offset -= n
	if m.offset < 0 {
		m.offset = 0
	}
}

// Clear empties the log.
func (m *ActivityModel) Clear() {
	m.lines = nil
	m.offset = 0
}

// View renders the activity log.
func (m ActivityModel) View() string {
	if len(m.lines) == 0 {
		body := lipgloss.JoinVertical(lipgloss.Left,
			StyleDim.Render("  Nothing yet."),
			"",
			StyleDim.Render("  Transfers, moves, deletions and conflicts appear here as"),
			StyleDim.Render("  they happen. Drop a file into the folder to see one."),
		)
		return lipgloss.JoinVertical(lipgloss.Left, "",
			StyleBox.Render(body), "",
			StyleHelp.Render("s: sync now   c: clear   q: quit"))
	}

	rows := m.visible()
	rendered := make([]string, 0, len(rows))
	for _, l := range rows {
		rendered = append(rendered, m.renderLine(l))
	}

	title := fmt.Sprintf("Activity (%d)", len(m.lines))
	if m.offset > 0 {
		title += StyleWarning.Render(fmt.Sprintf("  ↑ %d newer", m.offset))
	}

	body := lipgloss.JoinVertical(lipgloss.Left,
		append([]string{StyleSubtitle.Render(title), ""}, rendered...)...)

	return lipgloss.JoinVertical(lipgloss.Left, "",
		StyleActiveBox.Render(body), "",
		StyleHelp.Render("↑/↓: scroll   s: sync now   c: clear   q: quit"))
}

// visible returns the slice of lines that fits the window.
func (m ActivityModel) visible() []activityLine {
	rows := m.height - 8 // borders, title, help
	if rows < 3 {
		rows = 3
	}
	if rows > 30 {
		rows = 30
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

	line := StyleDim.Render("  "+l.at.Format("15:04:05")) + " " +
		style.Width(9).Render(label) + " " +
		StyleValue.Render(truncate(l.path, m.pathWidth()))
	if l.size > 0 {
		line += StyleDim.Render("  " + humanBytes(l.size))
	}
	return line
}

func (m ActivityModel) pathWidth() int {
	w := m.width - 34
	if w < 20 {
		return 20
	}
	if w > 70 {
		return 70
	}
	return w
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
