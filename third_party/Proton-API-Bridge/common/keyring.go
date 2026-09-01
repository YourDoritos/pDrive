package common

import (
	"context"

	"github.com/ProtonMail/gopenpgp/v3/crypto"
	"github.com/rclone/go-proton-api"
)

/*
The Proton account keys are organized in the following hierarchy.

An account has some users, each of the user will have one or more user keys.
Each of the user will have some addresses, each of the address will have one or more address keys.

A key is encrypted by a passphrase, and the passphrase is encrypted by another key.

The address keyrings are encrypted with the primary user keyring at the time.

The primary address key is used to create (encrypt) and retrieve (decrypt) data, e.g. shares
*/
func getAccountKRs(ctx context.Context, c *proton.Client, keyPass, saltedKeyPass []byte) (*crypto.KeyRing, map[string]*crypto.KeyRing, map[string]proton.Address, []byte, error) {
	/* Code taken and modified from proton-bridge */

	// pdrive: these two are deliberately SEQUENTIAL, despite being
	// independent and costing a round trip each.
	//
	// They are the first API calls a process makes. If the stored access
	// token has expired, running them concurrently means both receive a 401
	// and each fires its own POST /auth/v4/refresh. Proton rotates the
	// refresh token and invalidates the previous one on every refresh, so two
	// in flight at once is a race that can spend the token twice and leave
	// the session dead with Code=10013.
	//
	// Observed in practice while benchmarking: parallelising these produced
	// two concurrent refreshes on a cold start. Parallelism after this point
	// is safe, because the token is known good by then — see the concurrent
	// volumes/shares fetch in drive.go.
	user, err := c.GetUser(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	addrsArr, err := c.GetAddresses(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	if saltedKeyPass == nil {
		if keyPass == nil {
			return nil, nil, nil, nil, ErrKeyPassOrSaltedKeyPassMustBeNotNil
		}

		/*
			Notes for -> BUG: Access token does not have sufficient scope
			Only within the first x minutes that the user logs in with username and password, the getSalts route will be available to be called!
		*/
		salts, err := c.GetSalts(ctx)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		// log.Printf("salts %#v", salts)

		saltedKeyPass, err = salts.SaltForKey(keyPass, user.Keys.Primary().ID)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		// log.Printf("saltedKeyPass ok")
	}

	userKR, addrKRs, err := proton.Unlock(user, addrsArr, saltedKeyPass, nil)
	if err != nil {
		return nil, nil, nil, nil, err

	} else if userKR.CountDecryptionEntities(0) == 0 {
		if err != nil {
			return nil, nil, nil, nil, ErrFailedToUnlockUserKeys
		}
	}

	addrs := make(map[string]proton.Address)
	for _, addr := range addrsArr {
		addrs[addr.Email] = addr
	}

	return userKR, addrKRs, addrs, saltedKeyPass, nil
}
