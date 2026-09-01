package tui

import (
	"context"
	"net"
	"time"

	"github.com/YourDoritos/pdrive/internal/api"
	"github.com/YourDoritos/pdrive/internal/config"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type screen int

const (
	screenLogin screen = iota
	screenStatus
)

// App is the root bubbletea model.
type App struct {
	width, height int
	screen        screen

	cfg    *config.Config
	client *api.Client
	store  *api.SessionStore

	login  LoginModel
	status StatusModel

	resuming bool
	err      error
	quitting bool
}

type resumeDoneMsg struct{ user *api.User }
type resumeFailedMsg struct{ err error }
type gateProbeMsg struct{ available bool }

// NewApp builds the root model. When a stored session is present it is
// resumed rather than prompting for credentials again.
func NewApp(cfg *config.Config, client *api.Client, store *api.SessionStore, hasSession bool) App {
	app := App{
		cfg:      cfg,
		client:   client,
		store:    store,
		login:    NewLoginModel(cfg.Account.Email),
		status:   NewStatusModel(cfg),
		screen:   screenLogin,
		resuming: hasSession,
	}
	return app
}

// Init implements tea.Model.
func (a App) Init() tea.Cmd {
	cmds := []tea.Cmd{probeGate(a.cfg)}
	if a.resuming {
		cmds = append(cmds, a.resumeSession())
	} else {
		cmds = append(cmds, a.login.Init())
	}
	return tea.Batch(cmds...)
}

// resumeSession revalidates a stored session against the API. A session that
// no longer works drops the user back to the login screen rather than failing
// later in the sync loop.
func (a App) resumeSession() tea.Cmd {
	client := a.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		user, err := client.GetUser(ctx)
		if err != nil {
			return resumeFailedMsg{err: err}
		}
		return resumeDoneMsg{user: user}
	}
}

// probeGate checks whether pdrive-gate is listening. The gate is optional:
// its absence downgrades freshness, it never blocks startup.
func probeGate(cfg *config.Config) tea.Cmd {
	enabled := cfg.Freshness.Gate
	return func() tea.Msg {
		if !enabled {
			return gateProbeMsg{available: false}
		}
		conn, err := net.DialTimeout("unix", config.GateSocketPath(), 300*time.Millisecond)
		if err != nil {
			return gateProbeMsg{available: false}
		}
		conn.Close()
		return gateProbeMsg{available: true}
	}
}

// Update implements tea.Model.
func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		a.login.SetSize(msg.Width, msg.Height)
		a.status.SetSize(msg.Width, msg.Height)
		return a, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			a.quitting = true
			return a, tea.Quit
		case "q":
			// Only quit on bare "q" when no text field has focus, otherwise
			// the user cannot type a "q" in their password.
			if a.screen == screenStatus {
				a.quitting = true
				return a, tea.Quit
			}
		case "l":
			if a.screen == screenStatus {
				return a.logout()
			}
		}

	case gateProbeMsg:
		a.status.SetGateAvailable(msg.available)
		return a, nil

	case resumeDoneMsg:
		a.resuming = false
		a.status.SetUser(msg.user)
		a.screen = screenStatus
		return a, nil

	case resumeFailedMsg:
		// Stored session is dead: wipe it and ask for credentials.
		a.resuming = false
		if api.IsAuthError(msg.err) {
			_ = a.store.Delete()
			a.err = nil
		} else {
			a.err = msg.err
		}
		a.screen = screenLogin
		return a, a.login.Init()

	case LoginSuccessMsg:
		a.status.SetUser(msg.Result.User)
		a.screen = screenStatus
		// Remember the email so the next login is one field shorter. No
		// secret material goes into config.toml.
		a.cfg.Reload()
		a.cfg.Account.Email = a.client.LoginEmail()
		_ = a.cfg.Save()
		return a, nil
	}

	if a.screen == screenLogin && !a.resuming {
		var cmd tea.Cmd
		a.login, cmd = a.login.Update(msg, a.client, a.store)
		return a, cmd
	}
	return a, nil
}

func (a App) logout() (tea.Model, tea.Cmd) {
	client, store := a.client, a.store
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Logout(ctx)
	}()
	_ = store.Delete()

	a.screen = screenLogin
	a.login = NewLoginModel(a.cfg.Account.Email)
	return a, a.login.Init()
}

// View implements tea.Model.
func (a App) View() string {
	if a.quitting {
		return ""
	}

	if a.resuming {
		return "\n" + StyleBox.Render(
			StyleTitle.Render("pdrive")+"\n\n"+StyleDim.Render("Resuming session…")) + "\n"
	}

	var body string
	switch a.screen {
	case screenStatus:
		body = a.status.View()
	default:
		body = a.login.View()
	}

	if a.err != nil {
		body = lipgloss.JoinVertical(lipgloss.Left, body, "", StyleError.Render("  "+a.err.Error()))
	}
	return body
}
