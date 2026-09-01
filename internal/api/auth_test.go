package api

import (
	"encoding/base64"
	"testing"
)

func TestNeeds2FA(t *testing.T) {
	cases := []struct {
		name string
		auth AuthResponse
		want bool
	}{
		{
			name: "no 2fa on account",
			auth: AuthResponse{TwoFA: TwoFA{Enabled: 0}},
			want: false,
		},
		{
			name: "totp enabled, no elevated scope yet",
			auth: AuthResponse{TwoFA: TwoFA{Enabled: 1}, Scope: "self user loggedin"},
			want: true,
		},
		{
			name: "totp enabled but drive scope already granted",
			auth: AuthResponse{TwoFA: TwoFA{Enabled: 1}, Scopes: []string{"self", "drive"}},
			want: false,
		},
		{
			name: "totp enabled but full scope in space-separated field",
			auth: AuthResponse{TwoFA: TwoFA{Enabled: 1}, Scope: "self user full"},
			want: false,
		},
		{
			name: "fido2 only, no totp bit",
			auth: AuthResponse{TwoFA: TwoFA{Enabled: 2}, Scope: "self"},
			want: false,
		},
		{
			name: "totp and fido2 both set",
			auth: AuthResponse{TwoFA: TwoFA{Enabled: 3}, Scope: "self"},
			want: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Needs2FA(&c.auth); got != c.want {
				t.Errorf("Needs2FA() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestNeedsMailboxPassword(t *testing.T) {
	if NeedsMailboxPassword(&AuthResponse{PasswordMode: 1}) {
		t.Error("single-password account should not need a mailbox password")
	}
	if !NeedsMailboxPassword(&AuthResponse{PasswordMode: 2}) {
		t.Error("two-password account must need a mailbox password")
	}
}

func TestPrimaryKey(t *testing.T) {
	u := &User{Keys: []Key{
		{ID: "old", Primary: 0, Active: 0},
		{ID: "secondary", Primary: 0, Active: 1},
		{ID: "main", Primary: 1, Active: 1},
	}}
	if k := u.PrimaryKey(); k == nil || k.ID != "main" {
		t.Errorf("PrimaryKey() = %v, want key 'main'", k)
	}

	// No primary flagged: fall back to any active key rather than failing.
	u2 := &User{Keys: []Key{{ID: "inactive", Active: 0}, {ID: "usable", Active: 1}}}
	if k := u2.PrimaryKey(); k == nil || k.ID != "usable" {
		t.Errorf("PrimaryKey() fallback = %v, want key 'usable'", k)
	}

	if k := (&User{}).PrimaryKey(); k != nil {
		t.Errorf("PrimaryKey() on keyless account = %v, want nil", k)
	}
}

func TestSaltFor(t *testing.T) {
	r := &SaltsResponse{Salts: []Salt{
		{ID: "a", KeySalt: "c2FsdC1h"},
		{ID: "b", KeySalt: "c2FsdC1i"},
	}}
	if got := r.SaltFor("b"); got != "c2FsdC1i" {
		t.Errorf("SaltFor(b) = %q", got)
	}
	if got := r.SaltFor("missing"); got != "" {
		t.Errorf("SaltFor(missing) = %q, want empty", got)
	}
}

func TestDeriveKeyPassphrase(t *testing.T) {
	// 16 raw bytes is the salt size Proton issues.
	salt := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))

	pass, err := DeriveKeyPassphrase("hunter2", salt)
	if err != nil {
		t.Fatalf("DeriveKeyPassphrase() error: %v", err)
	}
	if len(pass) != 31 {
		t.Errorf("passphrase length = %d, want 31", len(pass))
	}

	// Deterministic for the same inputs — a different result each call would
	// mean nothing decrypts after a restart.
	again, err := DeriveKeyPassphrase("hunter2", salt)
	if err != nil {
		t.Fatalf("second derive error: %v", err)
	}
	if string(pass) != string(again) {
		t.Error("derivation is not deterministic")
	}

	// Different password must give a different passphrase.
	other, err := DeriveKeyPassphrase("hunter3", salt)
	if err != nil {
		t.Fatalf("third derive error: %v", err)
	}
	if string(pass) == string(other) {
		t.Error("different passwords produced the same passphrase")
	}
}

func TestDeriveKeyPassphraseErrors(t *testing.T) {
	if _, err := DeriveKeyPassphrase("pw", ""); err == nil {
		t.Error("expected an error for a missing salt")
	}
	if _, err := DeriveKeyPassphrase("pw", "!!!not base64!!!"); err == nil {
		t.Error("expected an error for an undecodable salt")
	}
}

func TestRequestErrorIsAuthError(t *testing.T) {
	cases := []struct {
		err  RequestError
		want bool
	}{
		{RequestError{Code: 10013}, true},      // refresh token revoked
		{RequestError{Code: 10002}, true},      // account deleted
		{RequestError{Code: 10003}, true},      // account disabled
		{RequestError{HTTPStatus: 401}, true},  // unauthorized
		{RequestError{HTTPStatus: 429}, false}, // rate limited: transient
		{RequestError{HTTPStatus: 503}, false}, // unavailable: transient
		{RequestError{HTTPStatus: 500, Code: 2000}, false},
	}
	for _, c := range cases {
		if got := c.err.IsAuthError(); got != c.want {
			t.Errorf("IsAuthError(%+v) = %v, want %v", c.err, got, c.want)
		}
	}

	if IsAuthError(nil) {
		t.Error("IsAuthError(nil) must be false")
	}
}
