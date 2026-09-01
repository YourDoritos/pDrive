package tui

import "github.com/charmbracelet/lipgloss"

// Palette.
//
// Deliberately NOT Proton's brand colours. The Proton Drive integration rules
// require third-party applications to be visually distinguishable from
// official Proton products and to use no Proton logos, trademarks or design
// elements. pdrive uses its own teal/amber identity.
var (
	ColorPrimary   = lipgloss.Color("#00B8A9") // teal
	ColorSecondary = lipgloss.Color("#3FD0C9")
	ColorAccent    = lipgloss.Color("#F6C177") // amber
	ColorSuccess   = lipgloss.Color("#2ECC71")
	ColorWarning   = lipgloss.Color("#F39C12")
	ColorError     = lipgloss.Color("#E74C3C")
	ColorBg        = lipgloss.Color("#11161B")
	ColorBgLight   = lipgloss.Color("#1B2229")
	ColorFg        = lipgloss.Color("#E8E8E8")
	ColorFgDim     = lipgloss.Color("#7A8794")
	ColorBorder    = lipgloss.Color("#2E3A45")
)

// Shared styles.
var (
	StyleTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorPrimary)

	StyleSubtitle = lipgloss.NewStyle().
			Foreground(ColorFgDim)

	StyleSelected = lipgloss.NewStyle().
			Foreground(ColorAccent).
			Bold(true)

	StyleNormal = lipgloss.NewStyle().
			Foreground(ColorFg)

	StyleDim = lipgloss.NewStyle().
			Foreground(ColorFgDim)

	StyleSuccess = lipgloss.NewStyle().
			Foreground(ColorSuccess).
			Bold(true)

	StyleError = lipgloss.NewStyle().
			Foreground(ColorError).
			Bold(true)

	StyleWarning = lipgloss.NewStyle().
			Foreground(ColorWarning)

	StyleBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorBorder).
			Padding(1, 2)

	StyleActiveBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorPrimary).
			Padding(1, 2)

	StyleStatusBar = lipgloss.NewStyle().
			Foreground(ColorFg).
			Background(ColorBgLight).
			Padding(0, 1)

	StyleHelp = lipgloss.NewStyle().
			Foreground(ColorFgDim)

	StyleLabel = lipgloss.NewStyle().
			Foreground(ColorFgDim).
			Width(16)

	StyleValue = lipgloss.NewStyle().
			Foreground(ColorFg)

	StyleBadge = lipgloss.NewStyle().
			Foreground(ColorBg).
			Background(ColorPrimary).
			Padding(0, 1).
			Bold(true)

	// StyleDisclosure renders the mandatory third-party notice. Required by
	// the Proton Drive integration rules wherever account details are
	// requested. Do not remove.
	StyleDisclosure = lipgloss.NewStyle().
			Foreground(ColorWarning).
			Italic(true)
)

// Disclosure is the notice that must be shown wherever pdrive asks for
// account credentials.
const Disclosure = "This is a third-party application not officially supported by Proton."

// QuotaBar renders a proportional usage bar.
func QuotaBar(used, max int64, width int) string {
	if width < 4 {
		width = 4
	}
	if max <= 0 {
		return lipgloss.NewStyle().Foreground(ColorFgDim).Render(repeat("░", width))
	}

	ratio := float64(used) / float64(max)
	if ratio > 1 {
		ratio = 1
	}
	filled := int(ratio * float64(width))
	if filled == 0 && used > 0 {
		filled = 1
	}

	color := ColorSuccess
	switch {
	case ratio >= 0.95:
		color = ColorError
	case ratio >= 0.80:
		color = ColorWarning
	}

	bar := lipgloss.NewStyle().Foreground(color).Render(repeat("█", filled))
	rest := lipgloss.NewStyle().Foreground(ColorBorder).Render(repeat("░", width-filled))
	return bar + rest
}

func repeat(s string, n int) string {
	if n <= 0 {
		return ""
	}
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
