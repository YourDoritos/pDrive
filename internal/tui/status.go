package tui

import (
	"fmt"

	"github.com/YourDoritos/pdrive/internal/api"
	"github.com/YourDoritos/pdrive/internal/config"
	"github.com/charmbracelet/lipgloss"
)

// StatusModel shows the authenticated account.
//
// Phase 0 scope: prove the credential chain works end to end. The sync
// status, activity log and conflict browser arrive in Phase 3.
type StatusModel struct {
	width, height int
	user          *api.User
	cfg           *config.Config
	gateAvailable bool
}

// NewStatusModel builds the status screen.
func NewStatusModel(cfg *config.Config) StatusModel {
	return StatusModel{cfg: cfg}
}

// SetSize records the terminal dimensions.
func (m *StatusModel) SetSize(w, h int) { m.width, m.height = w, h }

// SetUser installs the authenticated account.
func (m *StatusModel) SetUser(u *api.User) { m.user = u }

// SetGateAvailable records whether pdrive-gate is reachable.
func (m *StatusModel) SetGateAvailable(ok bool) { m.gateAvailable = ok }

// View renders the status screen.
func (m StatusModel) View() string {
	if m.user == nil {
		return StyleDim.Render("  no account loaded")
	}

	name := m.user.Email
	if name == "" {
		name = m.user.Name
	}

	quotaWidth := 32
	usedPct := 0.0
	if m.user.MaxSpace > 0 {
		usedPct = float64(m.user.UsedSpace) / float64(m.user.MaxSpace) * 100
	}

	rows := []string{
		row("Account", StyleValue.Render(name)),
		row("Storage", fmt.Sprintf("%s  %s / %s  (%.1f%%)",
			QuotaBar(m.user.UsedSpace, m.user.MaxSpace, quotaWidth),
			humanBytes(m.user.UsedSpace),
			humanBytes(m.user.MaxSpace),
			usedPct)),
		row("Max upload", StyleValue.Render(humanBytes(m.user.MaxUpload))),
		row("Keys", StyleSuccess.Render(fmt.Sprintf("unlocked (%d on account)", len(m.user.Keys)))),
		"",
		row("Sync folder", StyleValue.Render(m.cfg.SyncRoot())),
		row("Freshness", m.freshnessLine()),
		row("Size cap", StyleValue.Render(sizeCap(m.cfg.Sync.MaxAutoDownloadSize))),
	}

	body := StyleTitle.Render("pdrive") + "  " +
		StyleSubtitle.Render("signed in") + "\n\n" +
		lipgloss.JoinVertical(lipgloss.Left, rows...)

	phase := StyleWarning.Render(
		"Phase 0: authentication only. No files are synced yet — nothing on this\n" +
			"  machine or in your account is read or written beyond the account query above.")

	help := StyleHelp.Render("l: log out   q: quit")

	return lipgloss.JoinVertical(lipgloss.Left,
		"", StyleActiveBox.Render(body), "", StyleBox.Render(phase), "", help)
}

func (m StatusModel) freshnessLine() string {
	if !m.cfg.Freshness.Gate {
		return StyleDim.Render("polling only (gate disabled in config)")
	}
	if m.gateAvailable {
		return StyleSuccess.Render(fmt.Sprintf("gate active (blocking, max %dms)", m.cfg.Freshness.MaxBlockMS))
	}
	return StyleWarning.Render("gate not running — falling back to polling")
}

func row(label, value string) string {
	return StyleLabel.Render("  "+label) + " " + value
}

func sizeCap(n int64) string {
	if n <= 0 {
		return "unlimited"
	}
	return humanBytes(n) + " (larger files land as .pdrive-stub)"
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
