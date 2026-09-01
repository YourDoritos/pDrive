package api

// Auth types. Endpoint shapes follow the Proton account API as used by
// rclone/go-proton-api against drive-api.proton.me.

type AuthInfoRequest struct {
	Username string `json:"Username"`
	Intent   string `json:"Intent"`
}

type AuthInfoResponse struct {
	Code            int    `json:"Code"`
	Version         int    `json:"Version"`
	Modulus         string `json:"Modulus"`
	ServerEphemeral string `json:"ServerEphemeral"`
	Salt            string `json:"Salt"`
	SRPSession      string `json:"SRPSession"`
	TwoFA           TwoFA  `json:"2FA"`
}

// TwoFA reports which second factors the account has enabled.
type TwoFA struct {
	Enabled int `json:"Enabled"` // bitfield: 1 = TOTP, 2 = FIDO2
	TOTP    int `json:"TOTP"`
}

// HasTOTP reports whether a TOTP code is required.
func (t TwoFA) HasTOTP() bool { return t.Enabled&1 != 0 }

type AuthRequest struct {
	Username        string `json:"Username"`
	ClientEphemeral string `json:"ClientEphemeral"`
	ClientProof     string `json:"ClientProof"`
	SRPSession      string `json:"SRPSession"`
}

type AuthResponse struct {
	Code         int      `json:"Code"`
	AccessToken  string   `json:"AccessToken"`
	RefreshToken string   `json:"RefreshToken"`
	TokenType    string   `json:"TokenType"`
	UID          string   `json:"UID"`
	UserID       string   `json:"UserID"`
	ServerProof  string   `json:"ServerProof"`
	Scope        string   `json:"Scope"`
	Scopes       []string `json:"Scopes"`
	ExpiresIn    int      `json:"ExpiresIn"`
	TwoFA        TwoFA    `json:"2FA"`
	// PasswordMode is 1 for single-password accounts and 2 for accounts with
	// a separate mailbox password. Two-password accounts need the mailbox
	// password to derive the key passphrase.
	PasswordMode int `json:"PasswordMode"`
}

type Auth2FARequest struct {
	TwoFactorCode string `json:"TwoFactorCode"`
}

type Auth2FAResponse struct {
	Code   int      `json:"Code"`
	Scopes []string `json:"Scopes"`
	Scope  string   `json:"Scope"`
}

type RefreshRequest struct {
	ResponseType string `json:"ResponseType"`
	GrantType    string `json:"GrantType"`
	RefreshToken string `json:"RefreshToken"`
	RedirectURI  string `json:"RedirectURI"`
}

type RefreshResponse struct {
	Code         int    `json:"Code"`
	AccessToken  string `json:"AccessToken"`
	RefreshToken string `json:"RefreshToken"`
	TokenType    string `json:"TokenType"`
	ExpiresIn    int    `json:"ExpiresIn"`
	Scope        string `json:"Scope"`
	UID          string `json:"UID"`
}

// Session is the persisted auth state.
type Session struct {
	UID          string `json:"uid"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	LoginEmail   string `json:"login_email"`
	// SaltedKeyPass is the derived PGP key passphrase (base64). Drive needs
	// it on every start to unlock the key hierarchy, and it cannot be
	// re-derived without the account password. pVPN never stores this — it
	// has no reason to hold key material at all.
	SaltedKeyPass string `json:"salted_key_pass,omitempty"`
}

// User and key types.

type Key struct {
	ID          string `json:"ID"`
	PrivateKey  string `json:"PrivateKey"`
	Primary     int    `json:"Primary"`
	Active      int    `json:"Active"`
	Fingerprint string `json:"Fingerprint"`
}

type User struct {
	ID          string `json:"ID"`
	Name        string `json:"Name"`
	DisplayName string `json:"DisplayName"`
	Email       string `json:"Email"`
	Keys        []Key  `json:"Keys"`

	UsedSpace int64 `json:"UsedSpace"`
	MaxSpace  int64 `json:"MaxSpace"`
	MaxUpload int64 `json:"MaxUpload"`

	Subscribed int `json:"Subscribed"`
}

// PrimaryKey returns the account's primary active key, or nil.
func (u *User) PrimaryKey() *Key {
	for i := range u.Keys {
		if u.Keys[i].Primary == 1 && u.Keys[i].Active == 1 {
			return &u.Keys[i]
		}
	}
	for i := range u.Keys {
		if u.Keys[i].Active == 1 {
			return &u.Keys[i]
		}
	}
	return nil
}

type UserResponse struct {
	Code int  `json:"Code"`
	User User `json:"User"`
}

type Salt struct {
	ID      string `json:"ID"`
	KeySalt string `json:"KeySalt"`
}

type SaltsResponse struct {
	Code  int    `json:"Code"`
	Salts []Salt `json:"KeySalts"`
}

// SaltFor returns the base64 key salt for the given key ID.
func (r *SaltsResponse) SaltFor(keyID string) string {
	for _, s := range r.Salts {
		if s.ID == keyID {
			return s.KeySalt
		}
	}
	return ""
}

// APIError is the Proton error envelope.
type APIError struct {
	Code    int         `json:"Code"`
	Error   string      `json:"Error"`
	Details interface{} `json:"Details,omitempty"`
}

// IsSuccess reports whether the response code indicates success.
func (e *APIError) IsSuccess() bool { return e.Code == 1000 || e.Code == 1001 }
