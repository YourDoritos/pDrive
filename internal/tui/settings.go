package tui

import (
	"fmt"
	"strings"

	"github.com/YourDoritos/pdrive/internal/config"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// SettingsModel edits config.toml from the TUI.
//
// Every setting cycles through a fixed set of values rather than accepting
// free text. A sync client's configuration decides how much of your data it
// is willing to delete; a typo in a text field is not an acceptable way to
// find that out.
type SettingsModel struct {
	width, height int
	cfg           *config.Config
	cursor        int
	message       string
	dirty         bool

	// The sync folder is a path, not one of a handful of choices, so it is
	// the one setting that takes free text. Changing it moves the existing
	// files rather than re-downloading them, so it is guarded by an explicit
	// confirmation.
	editingRoot bool
	rootInput   textinput.Model
	confirming  bool
	pendingRoot string
}

// rootRow is the index of the sync folder, which sits above the cycling
// settings and behaves differently.
const rootRow = -1

type settingRow struct {
	label string
	help  string
	// value renders the current setting.
	value func(*config.Config) string
	// cycle advances to the next choice.
	cycle func(*config.Config)
	// restart marks settings that only take effect after a daemon restart.
	restart bool
}

var settingRows = []settingRow{
	{
		label: "Size cap",
		help:  "Larger files aren't downloaded until you ask for them.",
		value: func(c *config.Config) string {
			if c.Sync.MaxAutoDownloadSize <= 0 {
				return "unlimited"
			}
			return humanBytes(c.Sync.MaxAutoDownloadSize)
		},
		cycle: func(c *config.Config) {
			steps := []int64{0, 100 << 20, 512 << 20, 1 << 30, 5 << 30, 20 << 30}
			c.Sync.MaxAutoDownloadSize = nextInt64(steps, c.Sync.MaxAutoDownloadSize)
		},
	},
	{
		label: "Deletion guard",
		help:  "Stop a sync that would delete more of your files than this.",
		value: func(c *config.Config) string {
			if c.Sync.DeletionGuardPercent >= 100 {
				return "off (not recommended)"
			}
			return fmt.Sprintf("%d%%", c.Sync.DeletionGuardPercent)
		},
		cycle: func(c *config.Config) {
			steps := []int{10, 25, 50, 75, 100}
			c.Sync.DeletionGuardPercent = nextInt(steps, c.Sync.DeletionGuardPercent)
		},
	},
	{
		label: "Trash retention",
		help:  "How long deleted files stay recoverable on this computer.",
		value: func(c *config.Config) string {
			return fmt.Sprintf("%d days", c.Sync.TrashRetentionDays)
		},
		cycle: func(c *config.Config) {
			steps := []int{7, 30, 90, 365}
			c.Sync.TrashRetentionDays = nextInt(steps, c.Sync.TrashRetentionDays)
		},
	},
	{
		label: "Blocking ls",
		help:  "Wait for new files to arrive before listing the folder.",
		value: func(c *config.Config) string {
			if c.Freshness.Gate {
				return "on"
			}
			return "off"
		},
		cycle: func(c *config.Config) { c.Freshness.Gate = !c.Freshness.Gate },
	},
	{
		label: "Max hold",
		help:  "How long a listing may wait. Longer is fresher but slower.",
		value: func(c *config.Config) string {
			return fmt.Sprintf("%d ms", c.Freshness.MaxBlockMS)
		},
		cycle: func(c *config.Config) {
			steps := []int{100, 200, 400, 700, 1000}
			c.Freshness.MaxBlockMS = nextInt(steps, c.Freshness.MaxBlockMS)
		},
	},
	{
		label: "Upload limit",
		help:  "Limit how fast files are uploaded.",
		value: func(c *config.Config) string { return kbps(c.Limits.UploadKbps) },
		cycle: func(c *config.Config) {
			steps := []int{0, 256, 1024, 4096, 16384}
			c.Limits.UploadKbps = nextInt(steps, c.Limits.UploadKbps)
		},
	},
	{
		label: "Download limit",
		help:  "Limit how fast files are downloaded.",
		value: func(c *config.Config) string { return kbps(c.Limits.DownloadKbps) },
		cycle: func(c *config.Config) {
			steps := []int{0, 256, 1024, 4096, 16384}
			c.Limits.DownloadKbps = nextInt(steps, c.Limits.DownloadKbps)
		},
	},
	{
		label: "Parallel jobs",
		help:  "How many files transfer at the same time.",
		value: func(c *config.Config) string {
			return fmt.Sprintf("%d", c.Limits.MaxParallelTransfers)
		},
		cycle: func(c *config.Config) {
			steps := []int{1, 2, 4, 8}
			c.Limits.MaxParallelTransfers = nextInt(steps, c.Limits.MaxParallelTransfers)
		},
	},
}

// NewSettingsModel builds the settings screen.
func NewSettingsModel(cfg *config.Config) SettingsModel {
	input := textinput.New()
	input.Placeholder = "~/pdrive"
	input.CharLimit = 512
	input.Width = 44

	return SettingsModel{cfg: cfg, cursor: rootRow, rootInput: input}
}

// EditingRoot reports whether the folder field has focus, so the root model
// knows to route keystrokes here instead of treating them as shortcuts.
func (m SettingsModel) EditingRoot() bool { return m.editingRoot }

// Confirming reports whether a folder move is awaiting confirmation.
func (m SettingsModel) Confirming() bool { return m.confirming }

// PendingRoot returns the folder the user asked to move to.
func (m SettingsModel) PendingRoot() string { return m.pendingRoot }

// UpdateInput forwards a keystroke to the folder field.
func (m *SettingsModel) UpdateInput(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	m.rootInput, cmd = m.rootInput.Update(msg)
	return cmd
}

// BeginEditRoot focuses the folder field.
func (m *SettingsModel) BeginEditRoot() tea.Cmd {
	m.editingRoot = true
	m.message = ""
	m.rootInput.SetValue(m.cfg.Sync.Root)
	m.rootInput.CursorEnd()
	m.rootInput.Focus()
	return textinput.Blink
}

// CancelEditRoot abandons the edit.
func (m *SettingsModel) CancelEditRoot() {
	m.editingRoot = false
	m.confirming = false
	m.pendingRoot = ""
	m.rootInput.Blur()
}

// SubmitRoot validates the typed folder and asks for confirmation.
func (m *SettingsModel) SubmitRoot() {
	candidate := strings.TrimSpace(m.rootInput.Value())
	if candidate == "" {
		m.CancelEditRoot()
		return
	}

	expanded := config.ExpandPath(candidate)
	if err := config.ValidateSyncRoot(m.cfg.SyncRoot(), expanded); err != nil {
		m.message = StyleError.Render(err.Error())
		return
	}

	m.editingRoot = false
	m.rootInput.Blur()
	m.confirming = true
	m.pendingRoot = candidate
}

// ConfirmRoot accepts the pending move and records the new folder.
func (m *SettingsModel) ConfirmRoot() string {
	pending := m.pendingRoot
	m.confirming = false
	m.pendingRoot = ""
	if pending != "" {
		m.cfg.Sync.Root = pending
	}
	return pending
}

// SetSize records the terminal dimensions.
func (m *SettingsModel) SetSize(w, h int) { m.width, m.height = w, h }

// MoveCursor moves the selection. The folder sits one above the first
// cycling setting.
func (m *SettingsModel) MoveCursor(delta int) {
	m.cursor += delta
	if m.cursor < rootRow {
		m.cursor = rootRow
	}
	if m.cursor >= len(settingRows) {
		m.cursor = len(settingRows) - 1
	}
}

// OnRootRow reports whether the folder is selected.
func (m SettingsModel) OnRootRow() bool { return m.cursor == rootRow }

// Cycle advances the selected setting to its next value.
func (m *SettingsModel) Cycle() {
	if m.cfg == nil || m.cursor < 0 || m.cursor >= len(settingRows) {
		return
	}
	settingRows[m.cursor].cycle(m.cfg)
	m.cfg.Validate()
	m.dirty = true
	m.message = ""
}

// Dirty reports whether there are unsaved changes.
func (m SettingsModel) Dirty() bool { return m.dirty }

// Save writes config.toml.
func (m *SettingsModel) Save() error {
	if m.cfg == nil {
		return nil
	}
	if err := m.cfg.Save(); err != nil {
		m.message = StyleError.Render("could not save: " + err.Error())
		return err
	}
	m.dirty = false
	m.message = StyleSuccess.Render("saved")
	return nil
}

// SetMessage shows a transient line under the table.
func (m *SettingsModel) SetMessage(s string) { m.message = s }

// View renders the settings screen.
func (m SettingsModel) View() string {
	if m.cfg == nil {
		return StyleDim.Render("  No settings loaded.")
	}

	if m.confirming {
		return CenterBox(m.width, m.height, StyleActiveBox, m.confirmView())
	}

	rows := []string{m.rootRowView(), ""}

	for i, s := range settingRows {
		marker := "  "
		labelStyle := StyleLabel
		valueStyle := StyleValue
		if i == m.cursor {
			marker = StyleSelected.Render("▸ ")
			labelStyle = StyleLabel.Foreground(ColorAccent)
			valueStyle = StyleSelected
		}
		rows = append(rows, marker+labelStyle.Render(s.label)+" "+valueStyle.Render(s.value(m.cfg)))
	}

	// Always the same height, whichever row is selected, or the panel jumps
	// as the cursor moves.
	hint := "Where your files are kept. Changing this moves them."
	switch {
	case m.editingRoot:
		hint = "Type a path, or ~/ for your home folder. Your files move there."
	case m.cursor >= 0 && m.cursor < len(settingRows):
		hint = settingRows[m.cursor].help
	}
	rows = append(rows, "")
	for _, line := range WrapFixed(hint, BoxWidth-8, 2) {
		rows = append(rows, StyleDim.Render("  "+line))
	}

	// Also fixed height, so saving or an error does not shift the panel.
	switch {
	case m.message != "":
		rows = append(rows, "", "  "+m.message)
	case m.dirty:
		rows = append(rows, "", StyleWarning.Render("  Unsaved — press w to save"))
	default:
		rows = append(rows, "", "")
	}

	help := "↑/↓ j/k: select  enter: change  w: write  q: quit"
	if m.editingRoot {
		help = "enter: continue  esc: cancel"
	}
	rows = append(rows, "", StyleHelp.Render(help))

	return CenterBox(m.width, m.height,
		StyleActiveBox, lipgloss.JoinVertical(lipgloss.Left, rows...))
}

func (m SettingsModel) rootRowView() string {
	if m.editingRoot {
		return StyleSelected.Render("▸ ") +
			StyleLabel.Foreground(ColorAccent).Render("Sync folder") + " " +
			m.rootInput.View()
	}

	marker, labelStyle, valueStyle := "  ", StyleLabel, StyleValue
	if m.cursor == rootRow {
		marker = StyleSelected.Render("▸ ")
		labelStyle = StyleLabel.Foreground(ColorAccent)
		valueStyle = StyleSelected
	}
	return marker + labelStyle.Render("Sync folder") + " " +
		valueStyle.Render(truncate(m.cfg.SyncRoot(), BoxWidth-22))
}

// confirmView spells out what a folder move will do before it happens.
func (m SettingsModel) confirmView() string {
	return lipgloss.JoinVertical(lipgloss.Left,
		StyleWarning.Render("  Move your sync folder?"),
		"",
		row("From", StyleValue.Render(truncate(m.cfg.SyncRoot(), BoxWidth-22))),
		row("To", StyleValue.Render(truncate(config.ExpandPath(m.pendingRoot), BoxWidth-22))),
		"",
		StyleDim.Render("  Your files are moved to the new folder. Nothing is"),
		StyleDim.Render("  downloaded again, and nothing in Proton Drive changes."),
		"",
		StyleDim.Render("  Syncing pauses until the move finishes."),
		"",
		StyleHelp.Render("y: move  n/esc: cancel"),
	)
}

func kbps(v int) string {
	if v <= 0 {
		return "unlimited"
	}
	if v >= 1024 {
		return fmt.Sprintf("%d MiB/s", v/1024)
	}
	return fmt.Sprintf("%d KiB/s", v)
}

// nextInt returns the value after current in steps, wrapping around. An
// unrecognised current value lands on the first step rather than sticking.
func nextInt(steps []int, current int) int {
	for i, v := range steps {
		if v == current {
			return steps[(i+1)%len(steps)]
		}
	}
	return steps[0]
}

func nextInt64(steps []int64, current int64) int64 {
	for i, v := range steps {
		if v == current {
			return steps[(i+1)%len(steps)]
		}
	}
	return steps[0]
}
