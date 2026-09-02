package tui

import (
	"fmt"

	"github.com/YourDoritos/pdrive/internal/config"
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
}

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
		help:  "Files larger than this are not downloaded; a .pdrive-stub marks them.",
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
		help:  "Refuse any pass that would delete more than this share of your files.",
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
		help:  "How long files removed remotely are kept in the local trash.",
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
		help:  "Hold `ls` until the folder is current. Needs pdrive-gate (root).",
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
		help:  "Longest a listing may ever wait. Proton latency must not become disk latency.",
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
		help:  "Cap upload bandwidth.",
		value: func(c *config.Config) string { return kbps(c.Limits.UploadKbps) },
		cycle: func(c *config.Config) {
			steps := []int{0, 256, 1024, 4096, 16384}
			c.Limits.UploadKbps = nextInt(steps, c.Limits.UploadKbps)
		},
	},
	{
		label: "Download limit",
		help:  "Cap download bandwidth.",
		value: func(c *config.Config) string { return kbps(c.Limits.DownloadKbps) },
		cycle: func(c *config.Config) {
			steps := []int{0, 256, 1024, 4096, 16384}
			c.Limits.DownloadKbps = nextInt(steps, c.Limits.DownloadKbps)
		},
	},
	{
		label: "Parallel jobs",
		help:  "Too many gets the account rate-limited by Proton.",
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
	return SettingsModel{cfg: cfg}
}

// SetSize records the terminal dimensions.
func (m *SettingsModel) SetSize(w, h int) { m.width, m.height = w, h }

// MoveCursor moves the selection.
func (m *SettingsModel) MoveCursor(delta int) {
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(settingRows) {
		m.cursor = len(settingRows) - 1
	}
}

// Cycle advances the selected setting to its next value.
func (m *SettingsModel) Cycle() {
	if m.cfg == nil || m.cursor >= len(settingRows) {
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
		return StyleDim.Render("  no configuration loaded")
	}

	rows := []string{
		row("Sync folder", StyleValue.Render(m.cfg.SyncRoot())),
		StyleDim.Render("                   restart the daemon to change this"),
		"",
	}

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

	if m.cursor < len(settingRows) {
		rows = append(rows, "", StyleDim.Render("  "+settingRows[m.cursor].help))
	}
	if m.dirty {
		rows = append(rows, "", StyleWarning.Render("  unsaved changes — press w to write them"))
	}
	if m.message != "" {
		rows = append(rows, "", "  "+m.message)
	}

	rows = append(rows, "",
		StyleHelp.Render("↑/↓ j/k: select  enter: change  w: write  q: quit"))
	return CenterBox(m.width, m.height,
		StyleActiveBox, lipgloss.JoinVertical(lipgloss.Left, rows...))
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
