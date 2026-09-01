package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/ProtonMail/go-srp"
)

// Login authenticates with SRP. If the account has TOTP enabled the returned
// response will require Submit2FA before the session is usable; check with
// Needs2FA.
func (c *Client) Login(ctx context.Context, username, password string) (*AuthResponse, error) {
	info, err := c.getAuthInfo(ctx, username)
	if err != nil {
		return nil, fmt.Errorf("get auth info: %w", err)
	}

	auth, err := srp.NewAuth(info.Version, username, []byte(password), info.Salt, info.Modulus, info.ServerEphemeral)
	if err != nil {
		return nil, fmt.Errorf("SRP auth init: %w", err)
	}

	proofs, err := auth.GenerateProofs(2048)
	if err != nil {
		return nil, fmt.Errorf("SRP generate proofs: %w", err)
	}

	req := AuthRequest{
		Username:        username,
		ClientEphemeral: base64.StdEncoding.EncodeToString(proofs.ClientEphemeral),
		ClientProof:     base64.StdEncoding.EncodeToString(proofs.ClientProof),
		SRPSession:      info.SRPSession,
	}

	var resp AuthResponse
	if err := c.doSingleRequest(ctx, http.MethodPost, "/auth/v4", req, &resp); err != nil {
		return nil, fmt.Errorf("auth request: %w", err)
	}

	// Verify the server proved knowledge of the verifier. Skipping this would
	// make the login vulnerable to a malicious endpoint.
	expected := base64.StdEncoding.EncodeToString(proofs.ExpectedServerProof)
	if resp.ServerProof != expected {
		return nil, fmt.Errorf("server proof verification failed")
	}

	c.SetSession(resp.UID, resp.AccessToken, resp.RefreshToken)
	c.mu.Lock()
	c.loginEmail = username
	c.mu.Unlock()

	return &resp, nil
}

// Needs2FA reports whether a TOTP code is still required to unlock the session.
func Needs2FA(auth *AuthResponse) bool {
	if !auth.TwoFA.HasTOTP() {
		return false
	}
	// If the session already carries a scope that implies full access, the
	// second factor has been satisfied.
	for _, scope := range auth.Scopes {
		if scope == "drive" || scope == "full" {
			return false
		}
	}
	for _, scope := range strings.Fields(auth.Scope) {
		if scope == "drive" || scope == "full" {
			return false
		}
	}
	return true
}

// NeedsMailboxPassword reports whether this is a two-password account, where
// the key passphrase derives from a separate mailbox password rather than the
// login password.
func NeedsMailboxPassword(auth *AuthResponse) bool {
	return auth.PasswordMode == 2
}

// Submit2FA completes authentication with a TOTP code.
func (c *Client) Submit2FA(ctx context.Context, code string) error {
	var resp Auth2FAResponse
	if err := c.doRequest(ctx, http.MethodPost, "/auth/v4/2fa", Auth2FARequest{TwoFactorCode: code}, &resp); err != nil {
		return fmt.Errorf("2FA submission: %w", err)
	}
	return nil
}

// Logout revokes the session server-side and clears it locally.
func (c *Client) Logout(ctx context.Context) error {
	err := c.doRequest(ctx, http.MethodDelete, "/auth/v4", nil, nil)
	c.SetSession("", "", "")
	c.mu.Lock()
	c.loginEmail = ""
	c.mu.Unlock()
	return err
}

func (c *Client) getAuthInfo(ctx context.Context, username string) (*AuthInfoResponse, error) {
	req := AuthInfoRequest{Username: username, Intent: "Proton"}

	var info AuthInfoResponse
	if err := c.doSingleRequest(ctx, http.MethodPost, "/auth/v4/info", req, &info); err != nil {
		return nil, err
	}
	return &info, nil
}
