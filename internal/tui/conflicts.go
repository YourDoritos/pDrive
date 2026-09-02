package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/YourDoritos/pdrive/internal/ipc"
	"github.com/charmbracelet/lipgloss"
)

// ConflictsModel lists preserved local copies.
//
// A conflict in pDrive never means data was lost — both versions are kept.
// The job of this screen is to make that obvious and to show where the other
// version went, because a user who does not know it was preserved will
// assume it was destroyed.
type ConflictsModel struct {
	width, height int
	root          string
	conflicts     []ipc.ConflictEntry
	cursor        int
}

// NewConflictsModel builds the conflicts screen.
func NewConflictsModel(root string) ConflictsModel {
	return ConflictsModel{root: root}
}

// SetSize records the terminal dimensions.
func (m *ConflictsModel) SetSize(w, h int) { m.width, m.height = w, h }

// SetConflicts installs the list.
func (m *ConflictsModel) SetConflicts(c []ipc.ConflictEntry) {
	m.conflicts = c
	if m.cursor >= len(c) {
		m.cursor = len(c) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// MoveCursor moves the selection.
func (m *ConflictsModel) MoveCursor(delta int) {
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.conflicts) {
		m.cursor = len(m.conflicts) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// Selected returns the highlighted conflict, or nil.
func (m ConflictsModel) Selected() *ipc.ConflictEntry {
	if m.cursor < 0 || m.cursor >= len(m.conflicts) {
		return nil
	}
	return &m.conflicts[m.cursor]
}

// View renders the conflicts screen.
func (m ConflictsModel) View() string {
	if len(m.conflicts) == 0 {
		body := lipgloss.JoinVertical(lipgloss.Left,
			StyleSuccess.Render("  No conflicts."),
			"",
			StyleDim.Render("  If the same file is ever changed in two places at once,"),
			StyleDim.Render("  both versions are kept and listed here. Nothing is"),
			StyleDim.Render("  overwritten and nothing is discarded."),
		)
		return lipgloss.JoinVertical(lipgloss.Left, "",
			StyleBox.Render(body), "",
			StyleHelp.Render("r: refresh   q: quit"))
	}

	header := StyleSubtitle.Render(
		fmt.Sprintf("%d conflict(s) — both versions were kept", len(m.conflicts)))

	rows := []string{header, ""}
	for i, c := range m.conflicts {
		marker := "  "
		nameStyle := StyleValue
		if i == m.cursor {
			marker = StyleSelected.Render("▸ ")
			nameStyle = StyleSelected
		}
		rows = append(rows, marker+nameStyle.Render(truncate(c.Path, m.pathWidth())))
		rows = append(rows,
			StyleDim.Render("      your copy: ")+
				StyleDim.Render(truncate(c.KeptLocal, m.pathWidth())))
		if c.At != "" {
			rows = append(rows, StyleDim.Render("      "+relTime(c.At)))
		}
		rows = append(rows, "")
	}

	if sel := m.Selected(); sel != nil {
		rows = append(rows, m.explain(*sel))
	}

	body := lipgloss.JoinVertical(lipgloss.Left, rows...)
	return lipgloss.JoinVertical(lipgloss.Left, "",
		StyleActiveBox.Render(body), "",
		StyleHelp.Render("↑/↓: select   r: refresh   q: quit"))
}

// explain says, for the selected conflict, what is where.
//
// The diff command is shown relative to the sync folder rather than as two
// absolute paths, and split over two lines. Conflict names carry a timestamp
// and a hostname, so an absolute pair runs well past a normal terminal width
// and pushes the panel off screen — and truncating it instead would leave the
// user with a command they cannot paste.
func (m ConflictsModel) explain(c ipc.ConflictEntry) string {
	kept := filepath.Join(m.root, filepath.FromSlash(c.KeptLocal))

	lines := []string{
		StyleDim.Render("  The version from Proton Drive is at the original name."),
		StyleDim.Render("  Your version was renamed beside it and uploaded too, so"),
		StyleDim.Render("  both exist on every device."),
		"",
		StyleDim.Render("  Compare them, from inside the sync folder:"),
		StyleValue.Render("    cd " + shortenHome(m.root)),
		// Split across lines rather than truncated: a conflict name carries a
		// timestamp and a hostname, and a command the user cannot paste is
		// worse than one that takes two lines.
		StyleValue.Render(fmt.Sprintf("    diff %q \\", c.Path)),
		StyleValue.Render(fmt.Sprintf("         %q", c.KeptLocal)),
		"",
		StyleDim.Render("  Delete the copy you do not want; this entry then clears"),
		StyleDim.Render("  itself on the next refresh."),
	}

	if _, err := os.Stat(kept); err != nil {
		lines = append(lines, "", StyleWarning.Render(
			"  The preserved copy is no longer on disk; this entry will clear."))
	}
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}

// shortenHome renders a path under the user's home as ~/…, which is both
// shorter and how the user thinks of it.
func shortenHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || !strings.HasPrefix(p, home) {
		return p
	}
	return "~" + strings.TrimPrefix(p, home)
}

func (m ConflictsModel) pathWidth() int {
	w := m.width - 20
	if w < 24 {
		return 24
	}
	if w > 76 {
		return 76
	}
	return w
}
