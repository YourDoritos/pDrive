package tui

import (
	"strings"
	"testing"

	"github.com/YourDoritos/pdrive/internal/api"
	"github.com/YourDoritos/pdrive/internal/config"
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

func TestStatusViewShowsQuota(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Validate()

	m := NewStatusModel(cfg)
	m.SetSize(100, 30)
	m.SetUser(&api.User{
		Email:     "someone@proton.me",
		UsedSpace: 128 * 1024 * 1024 * 1024,
		MaxSpace:  512 * 1024 * 1024 * 1024,
		MaxUpload: 5 * 1024 * 1024 * 1024,
		Keys:      []api.Key{{ID: "k1", Primary: 1, Active: 1}},
	})

	view := m.View()
	for _, want := range []string{"someone@proton.me", "128.0 GiB", "512.0 GiB", "25.0%"} {
		if !strings.Contains(view, want) {
			t.Errorf("status view missing %q\n%s", want, view)
		}
	}
}

func TestStatusViewHandlesZeroQuota(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Validate()
	m := NewStatusModel(cfg)
	m.SetUser(&api.User{Email: "x@y.z"})
	if m.View() == "" {
		t.Error("status view with zero quota rendered empty")
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
