package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/YourDoritos/pdrive/internal/api"
	"github.com/YourDoritos/pdrive/internal/config"
	"github.com/YourDoritos/pdrive/internal/ipc"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// View identifies a screen.
type View int

const (
	// ViewLogin asks for credentials.
	ViewLogin View = iota
	// ViewStatus shows what the daemon is doing.
	ViewStatus
	// ViewActivity is the live log.
	ViewActivity
	// ViewConflicts lists preserved local copies.
	ViewConflicts
	// ViewSettings edits config.toml.
	ViewSettings
)

// App is the root bubbletea model.
type App struct {
	width, height int
	view          View

	cfg    *config.Config
	client *api.Client
	store  *api.SessionStore

	login     LoginModel
	status    StatusModel
	activity  ActivityModel
	conflicts ConflictsModel
	settings  SettingsModel

	authenticated bool
	resuming      bool
	err           error
	quitting      bool

	// events carries daemon pushes; nil when the daemon is unreachable.
	events   <-chan *ipc.Event
	stopSubs func()
}

type (
	statusMsg     struct{ status *ipc.StatusData }
	daemonDownMsg struct{}
	conflictsMsg  struct{ conflicts []ipc.ConflictEntry }
	daemonEvtMsg  struct{ evt *ipc.Event }
	subscribedMsg struct {
		events <-chan *ipc.Event
		stop   func()
	}
	tickMsg         struct{}
	resumeDoneMsg   struct{ user *api.User }
	resumeFailedMsg struct{ err error }
	flashMsg        struct{ text string }
	// daemonStartedMsg follows a successful `systemctl --user start`.
	daemonStartedMsg struct{}
	// streamEndedMsg means the event stream closed. That is NOT evidence the
	// daemon is gone — only a failed status call is. Conflating the two made
	// a healthy daemon render as "not running".
	streamEndedMsg struct{}
)

// NewApp builds the root model.
func NewApp(cfg *config.Config, client *api.Client, store *api.SessionStore, hasSession bool) App {
	return App{
		cfg:       cfg,
		client:    client,
		store:     store,
		login:     NewLoginModel(cfg.Account.Email),
		status:    NewStatusModel(cfg),
		activity:  NewActivityModel(),
		conflicts: NewConflictsModel(cfg.SyncRoot()),
		settings:  NewSettingsModel(cfg),
		view:      ViewLogin,
		resuming:  hasSession,
	}
}

// Init implements tea.Model.
func (a App) Init() tea.Cmd {
	if a.resuming {
		return tea.Batch(a.resumeSession(), tick())
	}
	return tea.Batch(a.login.Init(), tick())
}

func tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

// resumeSession revalidates a stored session so a dead one drops the user
// back to the login screen rather than failing later.
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

// fetchStatus asks the daemon what it is doing.
func fetchStatus() tea.Cmd {
	return func() tea.Msg {
		c, err := ipc.Dial(config.SocketPath())
		if err != nil {
			return daemonDownMsg{}
		}
		defer c.Close()

		st, err := c.Status()
		if err != nil {
			return daemonDownMsg{}
		}
		return statusMsg{status: st}
	}
}

func fetchConflicts() tea.Cmd {
	return func() tea.Msg {
		c, err := ipc.Dial(config.SocketPath())
		if err != nil {
			return daemonDownMsg{}
		}
		defer c.Close()

		list, err := c.Conflicts()
		if err != nil {
			return daemonDownMsg{}
		}
		return conflictsMsg{conflicts: list}
	}
}

// subscribe opens the daemon's event stream.
func subscribe() tea.Cmd {
	return func() tea.Msg {
		events, stop, err := ipc.Subscribe(config.SocketPath())
		if err != nil {
			return daemonDownMsg{}
		}
		return subscribedMsg{events: events, stop: stop}
	}
}

// waitForEvent turns the event channel into a bubbletea message.
func waitForEvent(ch <-chan *ipc.Event) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		evt, ok := <-ch
		if !ok {
			return streamEndedMsg{}
		}
		return daemonEvtMsg{evt: evt}
	}
}

// runSync asks the daemon for a pass. The TUI never syncs on its own: two
// reconcilers on one tree would race.
func runSync() tea.Cmd {
	return func() tea.Msg {
		c, err := ipc.Dial(config.SocketPath())
		if err != nil {
			return flashMsg{text: "daemon not running"}
		}
		defer c.Close()

		if _, err := c.Sync(ipc.SyncParams{}); err != nil {
			return flashMsg{text: err.Error()}
		}
		return flashMsg{text: "sync finished"}
	}
}

func togglePause(paused bool) tea.Cmd {
	return func() tea.Msg {
		c, err := ipc.Dial(config.SocketPath())
		if err != nil {
			return flashMsg{text: "daemon not running"}
		}
		defer c.Close()

		if paused {
			if err := c.Resume(); err != nil {
				return flashMsg{text: err.Error()}
			}
			return flashMsg{text: "resumed"}
		}
		if err := c.Pause(); err != nil {
			return flashMsg{text: err.Error()}
		}
		return flashMsg{text: "paused"}
	}
}

func reloadDaemon() tea.Cmd {
	return func() tea.Msg {
		c, err := ipc.Dial(config.SocketPath())
		if err != nil {
			return flashMsg{text: "saved (daemon not running)"}
		}
		defer c.Close()

		if err := c.Reload(); err != nil {
			return flashMsg{text: "saved, but reload failed: " + err.Error()}
		}
		return flashMsg{text: "saved and applied"}
	}
}

// Update implements tea.Model.
func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		a.setSizes()
		return a, nil

	case tickMsg:
		a.status.Tick()
		cmds := []tea.Cmd{tick()}
		if a.authenticated {
			cmds = append(cmds, fetchStatus())
		}
		return a, tea.Batch(cmds...)

	case statusMsg:
		a.status.SetStatus(msg.status)
		if a.events == nil {
			return a, subscribe()
		}
		return a, nil

	case daemonDownMsg:
		// Authoritative: the status call itself failed.
		a.status.SetDaemonDown()
		a.dropStream()
		return a, nil

	case streamEndedMsg:
		// Only the stream went away. Drop it and let the next status poll
		// decide whether the daemon is actually gone; it will be resubscribed
		// automatically if it is not.
		a.dropStream()
		return a, nil

	case subscribedMsg:
		a.events = msg.events
		a.stopSubs = msg.stop
		return a, waitForEvent(a.events)

	case daemonEvtMsg:
		a.handleEvent(msg.evt)
		return a, waitForEvent(a.events)

	case conflictsMsg:
		a.conflicts.SetConflicts(msg.conflicts)
		return a, nil

	case flashMsg:
		a.settings.SetMessage(StyleDim.Render(msg.text))
		a.status.SetActivity(msg.text)
		return a, nil

	case daemonStartedMsg:
		a.status.SetActivity("")
		// systemd returns as soon as the unit is started; the daemon still
		// has to open its Drive session before it can answer.
		return a, tea.Tick(1500*time.Millisecond, func(time.Time) tea.Msg {
			return tickMsg{}
		})

	case resumeDoneMsg:
		a.resuming = false
		a.authenticated = true
		a.view = ViewStatus
		return a, tea.Batch(fetchStatus(), fetchConflicts())

	case resumeFailedMsg:
		a.resuming = false
		if api.IsAuthError(msg.err) {
			_ = a.store.Delete()
			a.err = nil
		} else {
			a.err = msg.err
		}
		a.view = ViewLogin
		return a, a.login.Init()

	case LoginSuccessMsg:
		a.authenticated = true
		a.view = ViewStatus
		a.cfg.Reload()
		a.cfg.Account.Email = a.client.LoginEmail()
		_ = a.cfg.Save()
		return a, tea.Batch(fetchStatus(), fetchConflicts())

	case tea.KeyMsg:
		return a.handleKey(msg)
	}

	if a.view == ViewLogin && !a.resuming {
		var cmd tea.Cmd
		a.login, cmd = a.login.Update(msg, a.client, a.store)
		return a, cmd
	}
	return a, nil
}

func (a *App) handleEvent(evt *ipc.Event) {
	switch evt.Type {
	case ipc.EventActivity:
		var ad ipc.ActivityData
		if err := json.Unmarshal(evt.Data, &ad); err == nil {
			a.activity.Add(ad)
			if ad.Kind != "skip" && ad.Path != "" {
				a.status.SetActivity(ad.Kind + " " + ad.Path)
			}
		}
	case ipc.EventSyncStarted:
		a.status.SetActivity("syncing…")
	case ipc.EventSyncFinished:
		a.status.SetActivity("")
	}
}

func (a App) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Ctrl+C always quits, even mid-typing.
	if msg.String() == "ctrl+c" {
		a.quitting = true
		if a.stopSubs != nil {
			a.stopSubs()
		}
		return a, tea.Quit
	}

	// While a text field has focus every other key belongs to it, or the
	// user could not type a "q" in their password.
	if a.view == ViewLogin || !a.authenticated {
		var cmd tea.Cmd
		a.login, cmd = a.login.Update(msg, a.client, a.store)
		return a, cmd
	}

	switch msg.String() {
	case "q":
		a.quitting = true
		if a.stopSubs != nil {
			a.stopSubs()
		}
		return a, tea.Quit

	case "esc":
		a.view = ViewStatus
		return a, fetchStatus()

	case "left", "h":
		a.view = prevView(a.view)
		return a, a.onTabChange()

	case "right":
		a.view = nextView(a.view)
		return a, a.onTabChange()

	case "1":
		a.view = ViewStatus
		return a, fetchStatus()
	case "2":
		a.view = ViewActivity
		return a, nil
	case "3":
		a.view = ViewConflicts
		return a, fetchConflicts()
	case "4":
		a.view = ViewSettings
		return a, nil

	case "s":
		if a.status.DaemonDown() {
			a.status.SetActivity("starting pdrived…")
			return a, startDaemon()
		}
		a.status.SetActivity("syncing…")
		return a, runSync()

	case "e":
		if a.status.DaemonDown() {
			a.status.SetActivity("enabling pdrived…")
			return a, enableDaemon()
		}
		return a, nil

	case "p":
		paused := a.status.status != nil && a.status.status.State == "paused"
		return a, togglePause(paused)

	case "r":
		switch a.view {
		case ViewConflicts:
			return a, fetchConflicts()
		default:
			return a, fetchStatus()
		}

	case "c":
		if a.view == ViewActivity {
			a.activity.Clear()
		}
		return a, nil

	case "up", "k":
		switch a.view {
		case ViewActivity:
			a.activity.ScrollUp(1)
		case ViewConflicts:
			a.conflicts.MoveCursor(-1)
		case ViewSettings:
			a.settings.MoveCursor(-1)
		}
		return a, nil

	case "down", "j":
		switch a.view {
		case ViewActivity:
			a.activity.ScrollDown(1)
		case ViewConflicts:
			a.conflicts.MoveCursor(1)
		case ViewSettings:
			a.settings.MoveCursor(1)
		}
		return a, nil

	case "enter", " ":
		if a.view == ViewSettings {
			a.settings.Cycle()
		}
		return a, nil

	case "w":
		if a.view == ViewSettings {
			if err := a.settings.Save(); err != nil {
				return a, nil
			}
			return a, reloadDaemon()
		}
		return a, nil

	case "l":
		// Logging out invalidates the session the daemon is using, so say so
		// rather than leaving it failing in the background.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = a.client.Logout(ctx)
		}()
		_ = a.store.Delete()
		a.authenticated = false
		a.view = ViewLogin
		a.login = NewLoginModel(a.cfg.Account.Email)
		return a, a.login.Init()
	}
	return a, nil
}

func (a *App) setSizes() {
	content := a.height - navHeight
	if content < 10 {
		content = 10
	}
	a.login.SetSize(a.width, a.height)
	a.status.SetSize(a.width, content)
	a.activity.SetSize(a.width, content)
	a.conflicts.SetSize(a.width, content)
	a.settings.SetSize(a.width, content)
}

// navHeight is the tab bar: border, content, border.
const navHeight = 3

// View implements tea.Model.
func (a App) View() string {
	if a.quitting || a.width == 0 {
		return ""
	}

	if a.resuming {
		return "\n" + StyleBox.Render(
			StyleTitle.Render("pDrive")+"\n\n"+StyleDim.Render("Resuming session…")) + "\n"
	}

	var content string
	switch a.view {
	case ViewStatus:
		content = a.status.View()
	case ViewActivity:
		content = a.activity.View()
	case ViewConflicts:
		content = a.conflicts.View()
	case ViewSettings:
		content = a.settings.View()
	default:
		content = a.login.View()
	}

	if a.err != nil {
		content = lipgloss.JoinVertical(lipgloss.Left, content, "",
			StyleError.Render("  "+a.err.Error()))
	}

	if !a.authenticated {
		return content
	}
	return lipgloss.JoinVertical(lipgloss.Left, a.renderNav(), content)
}

func (a App) renderNav() string {
	brand := lipgloss.NewStyle().
		Bold(true).Foreground(ColorPrimary).Padding(0, 1).Render("pDrive")

	tabs := []struct {
		key, label string
		view       View
	}{
		{"1", "Status", ViewStatus},
		{"2", "Activity", ViewActivity},
		{"3", "Conflicts", ViewConflicts},
		{"4", "Settings", ViewSettings},
	}

	active := lipgloss.NewStyle().
		Foreground(ColorFg).Bold(true).Padding(0, 1).
		Border(lipgloss.RoundedBorder()).BorderForeground(ColorAccent)
	inactive := lipgloss.NewStyle().
		Foreground(ColorFgDim).Padding(0, 1).
		Border(lipgloss.RoundedBorder()).BorderForeground(ColorBorder)

	var parts []string
	for _, t := range tabs {
		label := fmt.Sprintf("%s %s", t.key, t.label)
		// A conflict is the one thing the user must not miss, so the tab
		// carries the count wherever they happen to be.
		if t.view == ViewConflicts && a.status.status != nil && a.status.status.Conflicts > 0 {
			label = fmt.Sprintf("%s %s (%d)", t.key, t.label, a.status.status.Conflicts)
		}
		if a.view == t.view {
			parts = append(parts, active.Render(label))
		} else {
			parts = append(parts, inactive.Render(label))
		}
	}

	bar := lipgloss.JoinHorizontal(lipgloss.Center, parts...)
	return lipgloss.NewStyle().Width(a.width).Render(
		lipgloss.JoinHorizontal(lipgloss.Center, brand, "  ", bar))
}

// tabOrder is the left-to-right order of the tabs.
var tabOrder = []View{ViewStatus, ViewActivity, ViewConflicts, ViewSettings}

func nextView(v View) View {
	for i, t := range tabOrder {
		if t == v {
			return tabOrder[(i+1)%len(tabOrder)]
		}
	}
	return ViewStatus
}

func prevView(v View) View {
	for i, t := range tabOrder {
		if t == v {
			return tabOrder[(i-1+len(tabOrder))%len(tabOrder)]
		}
	}
	return ViewStatus
}

// onTabChange refreshes whatever the newly shown tab displays.
func (a App) onTabChange() tea.Cmd {
	switch a.view {
	case ViewConflicts:
		return fetchConflicts()
	case ViewStatus:
		return fetchStatus()
	}
	return nil
}

// dropStream tears down the event subscription without judging the daemon.
func (a *App) dropStream() {
	if a.stopSubs != nil {
		a.stopSubs()
		a.stopSubs = nil
	}
	a.events = nil
}
