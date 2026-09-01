package api

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/ProtonMail/go-srp"
	"github.com/ProtonMail/gopenpgp/v3/crypto"
)

// DeriveKeyPassphrase turns the account password into the passphrase that
// unlocks the PGP key hierarchy:
//
//	account password + key salt -> key passphrase
//	  -> user key -> address key -> share key -> node key -> content key
//
// For two-password accounts the mailbox password is used here instead of the
// login password. This is the step pVPN never performs: a VPN session needs
// only API tokens, whereas Drive cannot decrypt anything without it.
func DeriveKeyPassphrase(password, keySaltB64 string) ([]byte, error) {
	if keySaltB64 == "" {
		return nil, fmt.Errorf("no key salt for primary key")
	}

	keySalt, err := base64.StdEncoding.DecodeString(keySaltB64)
	if err != nil {
		return nil, fmt.Errorf("decode key salt: %w", err)
	}

	salted, err := srp.MailboxPassword([]byte(password), keySalt)
	if err != nil {
		return nil, fmt.Errorf("derive mailbox password: %w", err)
	}
	// Proton uses only the bcrypt hash portion, not the full crypt() string.
	if len(salted) < 31 {
		return nil, fmt.Errorf("derived passphrase too short (%d bytes)", len(salted))
	}
	return salted[len(salted)-31:], nil
}

// UnlockUserKey verifies that a passphrase actually opens the account's
// primary key. Doing this at login turns "wrong mailbox password" into an
// error the user sees immediately, rather than an undecryptable file three
// screens later.
func UnlockUserKey(armoredPrivateKey string, passphrase []byte) error {
	key, err := crypto.NewKeyFromArmored(armoredPrivateKey)
	if err != nil {
		return fmt.Errorf("parse private key: %w", err)
	}
	unlocked, err := key.Unlock(passphrase)
	if err != nil {
		return fmt.Errorf("unlock private key: %w", err)
	}
	unlocked.ClearPrivateParams()
	return nil
}

// KeyUnlockResult reports what VerifyKeyAccess established about an account.
type KeyUnlockResult struct {
	User          *User
	SaltedKeyPass []byte
}

// VerifyKeyAccess fetches the account and its key salts, derives the key
// passphrase and confirms it unlocks the primary key.
//
// Phase 0 ends here: proving the full chain from password to an unlocked key
// is what separates "the login worked" from "we can actually read Drive".
func (c *Client) VerifyKeyAccess(ctx context.Context, password string) (*KeyUnlockResult, error) {
	user, err := c.GetUser(ctx)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}

	primary := user.PrimaryKey()
	if primary == nil {
		return nil, fmt.Errorf("account has no active primary key")
	}

	salts, err := c.GetSalts(ctx)
	if err != nil {
		return nil, fmt.Errorf("get key salts: %w", err)
	}

	passphrase, err := DeriveKeyPassphrase(password, salts.SaltFor(primary.ID))
	if err != nil {
		return nil, err
	}

	if err := UnlockUserKey(primary.PrivateKey, passphrase); err != nil {
		return nil, fmt.Errorf("key passphrase rejected: %w "+
			"(two-password account? the mailbox password is needed here, not the login password)", err)
	}

	return &KeyUnlockResult{User: user, SaltedKeyPass: passphrase}, nil
}
