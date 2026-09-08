// Command screenshot renders pDrive's terminal screens to SVG.
//
// Generated rather than hand-captured so the images in the README cannot
// drift from the code: rerun it after a UI change and the screenshots are
// correct again. It draws the same models the running program does, through
// the same exported entry points.
//
//	go run ./tools/screenshot -out assets
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/YourDoritos/pdrive/internal/config"
	"github.com/YourDoritos/pdrive/internal/ipc"
	"github.com/YourDoritos/pdrive/internal/tui"
)

const (
	screenWidth  = 92
	screenHeight = 30
	// demoRoot is the sync folder the screenshots claim to be showing.
	demoRoot = "/home/you/pdrive"
)

type shot struct {
	name    string
	title   string
	caption string
	body    string
	align   vAlign
}

func main() {
	out := flag.String("out", "assets", "directory to write the SVGs into")
	flag.Parse()

	// Colour is chosen by the terminal profile, and there is no terminal
	// here. Without this every screenshot renders in plain grey.
	lipgloss.SetColorProfile(termenv.TrueColor)

	if err := os.MkdirAll(*out, 0755); err != nil {
		fail(err)
	}

	for _, s := range shots() {
		svg := renderSVG(s.title, parseANSI(s.body), s.align)
		path := filepath.Join(*out, s.name+".svg")
		if err := os.WriteFile(path, []byte(svg), 0644); err != nil {
			fail(err)
		}
		fmt.Printf("  %-24s %s\n", s.name+".svg", s.caption)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "screenshot:", err)
	os.Exit(1)
}

// demoConfig is the configuration the screenshots are taken against.
func demoConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Sync.Root = demoRoot
	cfg.Account.Email = "you@proton.me"
	cfg.Validate()
	return cfg
}

func shots() []shot {
	cfg := demoConfig()
	now := time.Now()

	// --- Status ---
	status := tui.NewStatusModel(cfg)
	status.SetSize(screenWidth, screenHeight)
	status.SetStatus(&ipc.StatusData{
		State: "idle", Account: "you@proton.me", Root: "/home/you/pdrive",
		Files: 1284, Dirs: 96, OnDisk: 41 << 30,
		QuotaUsed: 47 << 30, QuotaTotal: 500 << 30,
		GateActive: true, Uptime: "3h12m",
		LastSync: now.Add(-18 * time.Second).Format(time.RFC3339),
	})

	// --- Activity ---
	activity := tui.NewActivityModel()
	activity.SetSize(screenWidth, screenHeight)
	activity.SetHistory(&ipc.ActivityLog{Entries: []ipc.ActivityData{
		{Kind: "upload", Path: "Notes/meeting.md", Size: 4 << 10, At: at(now, -22*time.Minute)},
		{Kind: "download", Path: "Design/logo-v3.svg", Size: 82 << 10, At: at(now, -14*time.Minute)},
		{Kind: "move", Path: "draft.txt -> Archive/draft.txt", At: at(now, -9*time.Minute)},
		{Kind: "trash-remote", Path: "scratch.tmp", At: at(now, -6*time.Minute)},
		{Kind: "conflict", Path: "budget.ods", At: at(now, -3*time.Minute)},
		{Kind: "download", Path: "Photos/coast.jpg", Size: 6 << 20, At: at(now, -40*time.Second)},
	}})
	activity.UpdateTransfer(ipc.TransferData{
		Path: "Videos/holiday.mov", Total: 4 << 30, Done: 1740 << 20,
		Started: at(now, -32*time.Second),
	})
	activity.UpdateTransfer(ipc.TransferData{
		Path: "Backups/laptop.tar.zst", Up: true, Total: 900 << 20, Done: 210 << 20,
		Started: at(now, -11*time.Second),
	})

	// --- Conflicts ---
	//
	// The screen warns when the preserved copy is missing from disk, which
	// would be true of any made-up path and wrong to show in a screenshot.
	// Stage a real one so the panel renders the ordinary case.
	conflictRoot := stageConflict("budget (conflict 2026-09-03 20-41-12 laptop).ods")
	conflicts := tui.NewConflictsModel(conflictRoot)
	conflicts.SetSize(screenWidth, screenHeight)
	conflicts.SetConflicts([]ipc.ConflictEntry{{
		Path:      "budget.ods",
		KeptLocal: "budget (conflict 2026-09-03 20-41-12 laptop).ods",
		At:        at(now, -3*time.Minute),
	}})

	// --- Settings ---
	settings := tui.NewSettingsModel(cfg)
	settings.SetSize(screenWidth, screenHeight)
	settings.MoveCursor(1) // the size cap, so the hint line has something to say

	// --- Login ---
	login := tui.NewLoginModel("you@proton.me")
	login.SetSize(screenWidth, screenHeight+2)

	return []shot{
		{"tui-status", "pdrive — status", "account, storage and freshness",
			withNav(tui.ViewStatus, 1, status.View()), alignTop},
		{"tui-activity", "pdrive — activity", "live transfers over a durable history",
			withNav(tui.ViewActivity, 1, activity.View()), alignTop},
		{"tui-conflicts", "pdrive — conflicts", "both versions kept, never overwritten",
			// The panel prints the folder it was given, and that is a
			// throwaway directory here. Show the path a user would have.
			substituteRoot(
				withNav(tui.ViewConflicts, 1, conflicts.View()),
				conflictRoot, demoRoot), alignTop},
		{"tui-settings", "pdrive — settings", "every knob, no config file needed",
			withNav(tui.ViewSettings, 1, settings.View()), alignTop},
		// No tab bar above the login form, so nothing to top-align it with.
		{"tui-login", "pdrive — sign in", "SRP with 2FA, session stored encrypted",
			login.View(), alignMiddle},
	}
}

// substituteRoot swaps the staging directory for the path a user would have.
//
// The screen is already rendered and padded when this runs, so a plain
// ReplaceAll shortens whichever row carries the path and leaves the panel's
// right border adrift on that one row — it sat 11 columns in for exactly as
// long as the substitution was a bare ReplaceAll. Pad the replacement back to
// the original display width so every row still ends in the same column.
func substituteRoot(s, from, to string) string {
	pad := lipgloss.Width(from) - lipgloss.Width(to)
	if pad < 0 {
		// Would push the border out instead of pulling it in; there is no
		// padding that fixes that, so say so rather than ship a broken box.
		fail(fmt.Errorf("substituteRoot: %q is wider than the staging path %q", to, from))
	}
	return strings.ReplaceAll(s, from, to+strings.Repeat(" ", pad))
}

// stageConflict creates a throwaway directory holding the preserved copy, so
// the conflicts panel renders its normal state rather than its warning.
func stageConflict(kept string) string {
	dir, err := os.MkdirTemp("", "pdrive-shot-")
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(filepath.Join(dir, kept), []byte("demo"), 0600); err != nil {
		fail(err)
	}
	return dir
}

func at(now time.Time, d time.Duration) string {
	return now.Add(d).Format(time.RFC3339)
}

// withNav stacks the tab bar above a screen, exactly as the running program
// does.
func withNav(active tui.View, conflicts int, body string) string {
	return lipgloss.JoinVertical(lipgloss.Left,
		tui.RenderNav(screenWidth, active, conflicts), body)
}
