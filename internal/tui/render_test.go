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
		if !strings.Contains(m.View(), Disclosure) {
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
		Bytes: 512 * 1024 * 1024 * 1024, OnDisk: 128 * 1024 * 1024 * 1024,
		GateActive: true,
	})

	view := m.View()
	for _, want := range []string{"someone@proton.me", "128.0 GiB", "512.0 GiB", "12 files", "up to date"} {
		if !strings.Contains(view, want) {
			t.Errorf("status view missing %q\n%s", want, view)
		}
	}
}

// The daemon being down is the one state a user must not misread as "synced".
func TestStatusViewWhenDaemonIsDown(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Validate()

	m := NewStatusModel(cfg)
	m.SetSize(100, 30)
	m.SetDaemonDown()

	view := m.View()
	for _, want := range []string{"daemon not running", "systemctl --user"} {
		if !strings.Contains(view, want) {
			t.Errorf("daemon-down view missing %q\n%s", want, view)
		}
	}
	if strings.Contains(view, "up to date") {
		t.Error("a stopped daemon must never render as up to date")
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
