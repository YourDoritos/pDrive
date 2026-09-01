package tui

import (
	"context"
	"encoding/base64"
	"time"

	"github.com/YourDoritos/pdrive/internal/api"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/pquerna/otp/totp"
)

type loginStep int

const (
	stepUsername loginStep = iota
	stepPassword
	stepMailbox
	step2FA
	stepWorking
)

// LoginModel drives the credential flow: SRP login, optional TOTP, optional
// mailbox password, then verification that the key hierarchy actually unlocks.
type LoginModel struct {
	width, height int
	step          loginStep

	username textinput.Model
	password textinput.Model
	mailbox  textinput.Model
	twofa    textinput.Model

	err    error
	status string

	// pendingAuth is the auth response awaiting a second factor.
	pendingAuth *api.AuthResponse
}

// LoginSuccessMsg is emitted once the account is authenticated and its key
// hierarchy has been verified.
type LoginSuccessMsg struct {
	Result *api.KeyUnlockResult
}

type loginDoneMsg struct{ result *api.KeyUnlockResult }
type loginErrMsg struct{ err error }
type login2FAMsg struct{ auth *api.AuthResponse }
type loginMailboxMsg struct{}

// NewLoginModel builds the login screen, pre-filling a remembered email.
func NewLoginModel(rememberedEmail string) LoginModel {
	username := textinput.New()
	username.Placeholder = "you@proton.me"
	username.CharLimit = 128
	username.Width = 44
	username.SetValue(rememberedEmail)
	username.Focus()

	password := textinput.New()
	password.Placeholder = "password"
	password.EchoMode = textinput.EchoPassword
	password.EchoCharacter = '•'
	password.CharLimit = 256
	password.Width = 44

	mailbox := textinput.New()
	mailbox.Placeholder = "mailbox (second) password"
	mailbox.EchoMode = textinput.EchoPassword
	mailbox.EchoCharacter = '•'
	mailbox.CharLimit = 256
	mailbox.Width = 44

	twofa := textinput.New()
	twofa.Placeholder = "6-digit code, or a TOTP secret"
	twofa.CharLimit = 64
	twofa.Width = 44

	return LoginModel{
		step:     stepUsername,
		username: username,
		password: password,
		mailbox:  mailbox,
		twofa:    twofa,
	}
}

// Init implements tea.Model.
func (m LoginModel) Init() tea.Cmd { return textinput.Blink }

// SetSize records the terminal dimensions.
func (m *LoginModel) SetSize(w, h int) { m.width, m.height = w, h }

// InputFocused reports whether a text field currently has focus.
func (m LoginModel) InputFocused() bool { return m.step != stepWorking }

// Update handles login input and the async login commands.
func (m LoginModel) Update(msg tea.Msg, client *api.Client, store *api.SessionStore) (LoginModel, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			return m.handleEnter(client, store)
		case "tab", "shift+tab":
			return m.handleTab()
		case "esc":
			m.err = nil
		}

	case loginDoneMsg:
		result := msg.result
		return m, func() tea.Msg { return LoginSuccessMsg{Result: result} }

	case login2FAMsg:
		m.pendingAuth = msg.auth
		m.step = step2FA
		m.status = ""
		m.twofa.Focus()
		return m, textinput.Blink

	case loginMailboxMsg:
		m.step = stepMailbox
		m.status = ""
		m.mailbox.Focus()
		return m, textinput.Blink

	case loginErrMsg:
		m.err = msg.err
		m.status = ""
		if m.step == stepWorking {
			m.step = stepPassword
			m.password.Focus()
		}
		return m, nil
	}

	switch m.step {
	case stepUsername:
		m.username, cmd = m.username.Update(msg)
	case stepPassword:
		m.password, cmd = m.password.Update(msg)
	case stepMailbox:
		m.mailbox, cmd = m.mailbox.Update(msg)
	case step2FA:
		m.twofa, cmd = m.twofa.Update(msg)
	}
	return m, cmd
}

func (m LoginModel) handleEnter(client *api.Client, store *api.SessionStore) (LoginModel, tea.Cmd) {
	switch m.step {
	case stepUsername:
		if m.username.Value() == "" {
			return m, nil
		}
		m.step = stepPassword
		m.username.Blur()
		m.password.Focus()
		return m, textinput.Blink

	case stepPassword:
		if m.password.Value() == "" {
			return m, nil
		}
		m.step = stepWorking
		m.password.Blur()
		m.status = "Authenticating…"
		m.err = nil
		return m, m.doLogin(client, store)

	case step2FA:
		if m.twofa.Value() == "" {
			return m, nil
		}
		m.step = stepWorking
		m.twofa.Blur()
		m.status = "Verifying second factor…"
		m.err = nil
		return m, m.do2FA(client, store)

	case stepMailbox:
		if m.mailbox.Value() == "" {
			return m, nil
		}
		m.step = stepWorking
		m.mailbox.Blur()
		m.status = "Unlocking keys…"
		m.err = nil
		return m, m.doVerify(client, store, m.mailbox.Value())
	}
	return m, nil
}

func (m LoginModel) handleTab() (LoginModel, tea.Cmd) {
	switch m.step {
	case stepUsername:
		m.step = stepPassword
		m.username.Blur()
		m.password.Focus()
	case stepPassword:
		m.step = stepUsername
		m.password.Blur()
		m.username.Focus()
	default:
		return m, nil
	}
	return m, textinput.Blink
}

func (m LoginModel) doLogin(client *api.Client, store *api.SessionStore) tea.Cmd {
	username, password := m.username.Value(), m.password.Value()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()

		auth, err := client.Login(ctx, username, password)
		if err != nil {
			return loginErrMsg{err: err}
		}
		if api.Needs2FA(auth) {
			return login2FAMsg{auth: auth}
		}
		return finishLogin(ctx, client, store, auth, password)
	}
}

func (m LoginModel) do2FA(client *api.Client, store *api.SessionStore) tea.Cmd {
	code := m.twofa.Value()
	password := m.password.Value()
	auth := m.pendingAuth
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()

		// A stored TOTP secret is accepted in place of a code, so an
		// unattended daemon can re-authenticate without a human. See
		// docs/PROJECT_SPEC.md § Known Limitations for the tradeoff.
		if len(code) > 8 {
			if generated, err := totp.GenerateCode(code, time.Now()); err == nil {
				code = generated
			}
		}

		if err := client.Submit2FA(ctx, code); err != nil {
			return loginErrMsg{err: err}
		}
		return finishLogin(ctx, client, store, auth, password)
	}
}

func (m LoginModel) doVerify(client *api.Client, store *api.SessionStore, keyPassword string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		return finishVerify(ctx, client, store, keyPassword)
	}
}

// finishLogin decides whether the key passphrase can be derived from the
// login password, or whether a separate mailbox password is required.
func finishLogin(ctx context.Context, client *api.Client, store *api.SessionStore, auth *api.AuthResponse, password string) tea.Msg {
	if auth != nil && api.NeedsMailboxPassword(auth) {
		// Persist the token pair now so the mailbox step does not have to
		// redo SRP.
		session := client.GetSession()
		if err := store.Save(&session); err != nil {
			return loginErrMsg{err: err}
		}
		return loginMailboxMsg{}
	}
	return finishVerify(ctx, client, store, password)
}

// finishVerify proves the whole chain: tokens work, key salts fetch, the
// passphrase derives, and the primary key actually unlocks.
func finishVerify(ctx context.Context, client *api.Client, store *api.SessionStore, keyPassword string) tea.Msg {
	result, err := client.VerifyKeyAccess(ctx, keyPassword)
	if err != nil {
		return loginErrMsg{err: err}
	}

	session := client.GetSession()
	session.SaltedKeyPass = encodeKeyPass(result.SaltedKeyPass)
	if err := store.Save(&session); err != nil {
		return loginErrMsg{err: err}
	}
	return loginDoneMsg{result: result}
}

// View renders the login screen.
func (m LoginModel) View() string {
	title := StyleTitle.Render("pDrive")
	sub := StyleSubtitle.Render("Proton Drive sync for Linux")
	notice := StyleDisclosure.Render(Disclosure)

	var field string
	switch m.step {
	case stepUsername:
		field = fieldRow("Email", m.username.View())
	case stepPassword:
		field = fieldRow("Email", StyleDim.Render(m.username.Value())) + "\n" +
			fieldRow("Password", m.password.View())
	case stepMailbox:
		field = fieldRow("Email", StyleDim.Render(m.username.Value())) + "\n" +
			StyleWarning.Render("  Two-password account — the mailbox password unlocks your files.") + "\n" +
			fieldRow("Mailbox pw", m.mailbox.View())
	case step2FA:
		field = fieldRow("Email", StyleDim.Render(m.username.Value())) + "\n" +
			fieldRow("2FA code", m.twofa.View())
	case stepWorking:
		field = StyleDim.Render("  " + m.status)
	}

	body := title + "  " + sub + "\n\n" + notice + "\n\n" + field

	if m.err != nil {
		body += "\n\n" + StyleError.Render("  "+m.err.Error())
	}

	help := StyleHelp.Render("enter: continue   tab: switch field   esc: clear error   ctrl+c: quit")
	box := StyleActiveBox.Render(body)

	return lipgloss.JoinVertical(lipgloss.Left, "", box, "", help)
}

func fieldRow(label, value string) string {
	return StyleLabel.Render("  "+label) + " " + value
}

// encodeKeyPass base64-encodes the derived passphrase. It is 31 raw bytes and
// is not valid UTF-8, so it cannot be stored in a JSON string as-is without
// being silently mangled.
func encodeKeyPass(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
