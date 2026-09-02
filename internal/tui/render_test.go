package tui

import (
	"strings"
	"testing"

	"github.com/YourDoritos/pdrive/internal/config"
	"github.com/YourDoritos/pdrive/internal/ipc"
)

// The third-party disclosure is mandatory wherever pDrive asks for account
// details. If this test fails, the Proton Drive integration rules are being
// violated — fix the view, not the test.
func TestLoginViewAlwaysShowsDisclosure(t *testing.T) {
	steps := map[string]loginStep{
		"username": stepUsername,
		"password": stepPassword,
		"mailbox":  stepMailbox,
		"2fa":      step2FA,
		"working":  stepWorking,
	}

	for name, step := range steps {
		m := NewLoginModel("")
		m.step = step
		m.SetSize(100, 30)
		// Whitespace-insensitive: the notice is wrapped to fit the panel, and
		// the requirement is that it is shown, not that it is on one line.
		if !strings.Contains(flatten(m.View()), flatten(Disclosure)) {
			t.Errorf("login step %q does not show the third-party disclosure", name)
		}
	}
}

func TestLoginViewRendersEachStep(t *testing.T) {
	for _, step := range []loginStep{stepUsername, stepPassword, stepMailbox, step2FA, stepWorking} {
		m := NewLoginModel("someone@proton.me")
		m.step = step
		m.SetSize(100, 30)
		if got := m.View(); got == "" {
			t.Errorf("step %d rendered empty", step)
		}
	}
}

func TestStatusViewShowsQuotaAndState(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Validate()

	m := NewStatusModel(cfg)
	m.SetSize(100, 30)
	m.SetStatus(&ipc.StatusData{
		State:   "idle",
		Account: "someone@proton.me",
		Root:    "/home/someone/pdrive",
		Files:   12, Dirs: 3,
		Bytes: 70 << 20, OnDisk: 70 << 20,
		QuotaUsed: 67 << 20, QuotaTotal: 500 << 30,
		GateActive: true,
	})

	view := m.View()
	for _, want := range []string{"someone@proton.me", "12 files", "up to date", "67.0 MiB", "500.0 GiB"} {
		if !strings.Contains(view, want) {
			t.Errorf("status view missing %q\n%s", want, view)
		}
	}
}

// Regression: "Storage" showed the size of the local mirror against itself —
// "572.7 KiB of 572.7 KiB" — which reads as a full drive. What a user means
// by storage is the Proton account, which was nowhere on the screen.
func TestStatusShowsAccountQuotaNotMirrorSize(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Validate()

	m := NewStatusModel(cfg)
	m.SetSize(100, 30)
	m.SetStatus(&ipc.StatusData{
		State: "idle",
		// The mirror is tiny; the account is not.
		Bytes: 572 << 10, OnDisk: 572 << 10,
		QuotaUsed: 67 << 20, QuotaTotal: 500 << 30,
	})

	view := flatten(m.View())
	if !strings.Contains(view, "67.0 MiB of 500.0 GiB") {
		t.Errorf("account quota not shown as used-of-total:\n%s", m.View())
	}
	// The mirror size must never be presented as a quota.
	if strings.Contains(view, "572.7 KiB of 572.7 KiB") {
		t.Error("the local mirror size is still being rendered as storage usage")
	}
}

// Until the first quota fetch lands there is nothing to show; it must not
// fall back to the mirror size.
func TestStatusHandlesUnknownQuota(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Validate()

	m := NewStatusModel(cfg)
	m.SetSize(100, 30)
	m.SetStatus(&ipc.StatusData{State: "idle", Bytes: 1 << 20, OnDisk: 1 << 20})

	if strings.Contains(flatten(m.View()), "1.0 MiB of 1.0 MiB") {
		t.Error("with no quota known, the mirror size was shown as storage")
	}
}

// The daemon being down is the one state a user must not misread as "synced".
func TestStatusViewWhenDaemonIsDown(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Validate()

	m := NewStatusModel(cfg)
	m.SetSize(100, 30)
	m.SetDaemonDown()

	view := flatten(m.View())
	// The screen must offer to start the daemon, not print a command for the
	// user to go and type somewhere else.
	for _, want := range []string{"daemon not running", "start it now", "s: start"} {
		if !strings.Contains(view, want) {
			t.Errorf("daemon-down view missing %q\n%s", want, m.View())
		}
	}
	if strings.Contains(view, "up to date") {
		t.Error("a stopped daemon must never render as up to date")
	}
	if !m.DaemonDown() {
		t.Error("DaemonDown() should be true after SetDaemonDown()")
	}
}

func TestStatusViewSurfacesConflictsAndStubs(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Validate()

	m := NewStatusModel(cfg)
	m.SetSize(100, 30)
	m.SetStatus(&ipc.StatusData{State: "idle", Conflicts: 2, Stubs: 5})

	view := m.View()
	if !strings.Contains(view, "2") || !strings.Contains(view, "Conflicts") {
		t.Errorf("conflicts not surfaced:\n%s", view)
	}
	if !strings.Contains(view, "size cap") {
		t.Errorf("stubs not surfaced:\n%s", view)
	}
}

func TestStatusViewShowsError(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Validate()

	m := NewStatusModel(cfg)
	m.SetSize(100, 30)
	m.SetStatus(&ipc.StatusData{State: "error", LastError: "deletion-cliff guard stopped this pass"})

	if !strings.Contains(m.View(), "deletion-cliff") {
		t.Error("an error state must show what went wrong")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{5 * 1024 * 1024 * 1024, "5.0 GiB"},
		{2 * 1024 * 1024 * 1024 * 1024, "2.0 TiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestQuotaBarWidth(t *testing.T) {
	// The bar must always render exactly `width` cells regardless of ratio,
	// otherwise the status table jitters as usage changes.
	for _, used := range []int64{0, 1, 50, 99, 100, 200} {
		bar := stripANSI(QuotaBar(used, 100, 20))
		if n := len([]rune(bar)); n != 20 {
			t.Errorf("QuotaBar(%d,100,20) rendered %d cells, want 20", used, n)
		}
	}
}

func TestQuotaBarZeroMax(t *testing.T) {
	if bar := stripANSI(QuotaBar(0, 0, 10)); len([]rune(bar)) != 10 {
		t.Errorf("QuotaBar with zero max rendered %d cells, want 10", len([]rune(bar)))
	}
}

// stripANSI removes SGR escape sequences so cell counts can be asserted.
func stripANSI(s string) string {
	var out []rune
	inEsc := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			inEsc = true
		case inEsc && (r == 'm' || r == 'K'):
			inEsc = false
		case !inEsc:
			out = append(out, r)
		}
	}
	return string(out)
}

// flatten reduces rendered output to bare words: styling escapes are removed,
// box-drawing characters dropped, and runs of whitespace collapsed.
//
// Needed because a sentence wrapped inside a bordered panel is interrupted by
// a border and a colour reset between its lines, so a plain substring match
// would fail on text that is plainly visible on screen.
func flatten(s string) string {
	var b strings.Builder
	inEscape := false

	for _, r := range s {
		switch {
		case r == 0x1b:
			inEscape = true
		case inEscape:
			// SGR and other CSI sequences end with an alphabetic byte.
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
		case r == '│' || r == '─' || r == '╭' || r == '╮' || r == '╰' || r == '╯':
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
