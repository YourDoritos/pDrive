// Package drive wraps the Proton Drive API behind a small interface.
//
// Everything cryptographic lives behind this boundary on purpose. Proton has
// announced a new Drive cryptographic model for end 2026 / early 2027, and
// clients implementing only the previous model will stop interoperating. When
// that lands, this package is the only one that should need to change.
package drive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	bridge "github.com/rclone/Proton-API-Bridge"
	"github.com/rclone/Proton-API-Bridge/common"
	proton "github.com/rclone/go-proton-api"

	"github.com/YourDoritos/pdrive/internal/api"
)

// Node is one entry in the Drive tree.
type Node struct {
	LinkID   string
	ParentID string
	Name     string
	Path     string // slash-separated, relative to the Drive root
	IsDir    bool
	Size     int64
	Modified time.Time
	// Digest is Proton's own SHA1 of the plaintext, when the file carries
	// the extended attribute. It gives us an integrity check that does not
	// depend on our own download path being correct.
	Digest string
}

// Drive is an authenticated Proton Drive session.
type Drive struct {
	pd      *bridge.ProtonDrive
	log     Logger
	metrics *Metrics
}

// Logger receives progress and diagnostic output.
type Logger interface {
	Errorf(format string, v ...interface{})
	Warnf(format string, v ...interface{})
	Debugf(format string, v ...interface{})
}

type nopLogger struct{}

func (nopLogger) Errorf(string, ...interface{}) {}
func (nopLogger) Warnf(string, ...interface{})  {}
func (nopLogger) Debugf(string, ...interface{}) {}

// Options configures Open.
type Options struct {
	// Session is the authenticated pdrive session.
	Session *api.Session
	// Log receives diagnostics. Optional.
	Log Logger

	// OnAuth is called whenever Proton issues a new token pair, and MUST
	// persist it.
	//
	// Proton rotates the refresh token on every refresh and invalidates the
	// previous one immediately. A client that does not save the new pair is
	// left holding a spent token: the current process keeps working from
	// memory, and the *next* start fails with "Invalid refresh token"
	// (Code=10013). Passing a no-op here is a silent, delayed logout.
	OnAuth func(uid, accessToken, refreshToken string)

	// OnDeauth is called when Proton revokes the session outright. The
	// stored session is dead and the user must log in again.
	OnDeauth func()
}

// Open establishes a Drive session from an existing pdrive session.
//
// The bridge cannot perform a 2FA login itself, but it accepts an already
// authenticated session (UID, tokens and the base64 key passphrase). pdrive
// does its own SRP + TOTP login in internal/api and hands the result over
// here, which is what lets 2FA accounts work at all.
func Open(ctx context.Context, opts Options) (*Drive, error) {
	session := opts.Session
	if session == nil || session.AccessToken == "" {
		return nil, fmt.Errorf("not logged in")
	}
	if session.SaltedKeyPass == "" {
		return nil, fmt.Errorf("session has no key passphrase — log in again with `pdrive`")
	}
	log := opts.Log
	if log == nil {
		log = nopLogger{}
	}

	cfg := bridge.NewDefaultConfig()

	// Identify pdrive honestly. Required by the Proton Drive integration
	// rules; spoofing a first-party client is forbidden.
	cfg.AppVersion = api.AppVersion()
	cfg.UserAgent = api.UserAgent
	cfg.Logger = log

	// Never write a plaintext credential cache file. Our session store is
	// encrypted; the bridge's cache is not.
	cfg.CredentialCacheFile = ""

	// PDRIVE_TRACE=1 logs every HTTP request, including the bridge's
	// otherwise-opaque bootstrap.
	cfg.Transport = newTracingTransport(nil)

	cfg.UseReusableLogin = true
	cfg.ReusableCredential = &common.ReusableCredentialData{
		UID:           session.UID,
		AccessToken:   session.AccessToken,
		RefreshToken:  session.RefreshToken,
		SaltedKeyPass: session.SaltedKeyPass,
	}
	cfg.FirstLoginCredential = &common.FirstLoginCredentialData{}

	// A failed upload leaves a draft revision behind, and Proton then refuses
	// further uploads to that path until it is cleared. Without this, one
	// transient failure permanently blocks a file from ever syncing.
	//
	// The tradeoff: if another device is genuinely mid-upload to the same
	// path, its draft is replaced. That is the lesser problem — a lost
	// in-flight upload retries, whereas a stuck draft needs manual
	// intervention the user has no tool for.
	cfg.ReplaceExistingDraft = true

	// Be conservative with concurrency. The integration rules warn that
	// excessive parallelism gets the application and the account
	// rate-limited, and a backup is not worth that.
	cfg.ConcurrentBlockUploadCount = 4

	onAuth := func(auth proton.Auth) {
		if opts.OnAuth != nil {
			opts.OnAuth(auth.UID, auth.AccessToken, auth.RefreshToken)
		}
	}
	onDeauth := func() {
		if opts.OnDeauth != nil {
			opts.OnDeauth()
		}
	}

	metrics := &Metrics{}
	openStarted := time.Now()

	pd, _, err := bridge.NewProtonDrive(ctx, cfg, onAuth, onDeauth)
	metrics.add(&metrics.OpenCalls, &metrics.OpenTime, openStarted)
	if err != nil {
		if isDeadSession(err) {
			return nil, ErrSessionExpired
		}
		return nil, fmt.Errorf("open drive: %w", err)
	}

	return &Drive{pd: pd, log: log, metrics: metrics}, nil
}

// ErrSessionExpired reports that the stored session can no longer be used and
// the user must log in again.
var ErrSessionExpired = errors.New(
	"your Proton session has expired — run `pdrive` and log in again")

// isDeadSession recognises the API's permanent-auth-failure signals. Proton
// returns code 10013 when a refresh token has already been spent or revoked.
func isDeadSession(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "10013") ||
		strings.Contains(msg, "Invalid refresh token") ||
		strings.Contains(msg, "de-auth")
}

// Close releases the Drive session. It does not revoke the session
// server-side — pdrive keeps it for the next run.
func (d *Drive) Close() {}

// About returns the account, including quota.
func (d *Drive) About(ctx context.Context) (*proton.User, error) {
	return d.pd.About(ctx)
}

// RootLinkID returns the link ID of the Drive root folder.
func (d *Drive) RootLinkID() string { return d.pd.RootLink.LinkID }

// WalkFunc is called once per node during Walk. Returning an error aborts
// the walk.
type WalkFunc func(n Node) error

// Walk traverses the Drive tree depth-first from the root, calling fn for
// every active folder and file.
//
// This is a full tree traversal and is deliberately reserved for one-shot
// operations such as the initial backup. Steady-state synchronisation must
// use the event cursor instead: the integration rules explicitly forbid
// frequent recursive traversals.
func (d *Drive) Walk(ctx context.Context, fn WalkFunc) error {
	return d.walk(ctx, d.pd.RootLink.LinkID, "", fn)
}

func (d *Drive) walk(ctx context.Context, linkID, prefix string, fn WalkFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	started := time.Now()
	children, err := d.pd.ListDirectory(ctx, linkID)
	d.metrics.add(&d.metrics.ListCalls, &d.metrics.ListTime, started)
	if err != nil {
		return fmt.Errorf("list %q: %w", pathOrRoot(prefix), err)
	}

	for _, child := range children {
		path := child.Name
		if prefix != "" {
			path = prefix + "/" + child.Name
		}

		node := Node{
			LinkID:   child.Link.LinkID,
			ParentID: linkID,
			Name:     child.Name,
			Path:     path,
			IsDir:    child.IsFolder,
		}

		if !child.IsFolder {
			d.fillAttrs(ctx, child.Link, &node)
		}

		if err := fn(node); err != nil {
			return err
		}

		if child.IsFolder {
			if err := d.walk(ctx, child.Link.LinkID, path, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// Download opens a file's active revision for reading. The caller closes the
// returned reader. The reported size is the plaintext size.
func (d *Drive) Download(ctx context.Context, linkID string) (io.ReadCloser, int64, error) {
	started := time.Now()
	rc, size, _, err := d.pd.DownloadFileByID(ctx, linkID, 0)
	d.metrics.add(&d.metrics.DownloadCalls, &d.metrics.DownloadTime, started)
	if err != nil {
		return nil, 0, fmt.Errorf("download %s: %w", linkID, err)
	}
	return rc, size, nil
}

func pathOrRoot(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// fillAttrs populates size, modification time and Proton's own digest.
//
// These live in an encrypted extended attribute that is documented as
// sometimes absent. A file without it is still perfectly downloadable, so a
// miss is unknown metadata rather than a failure.
func (d *Drive) fillAttrs(ctx context.Context, link *proton.Link, node *Node) {
	started := time.Now()
	attrs, err := d.pd.GetActiveRevisionAttrs(ctx, link)
	d.metrics.add(&d.metrics.AttrCalls, &d.metrics.AttrTime, started)
	if err != nil {
		d.log.Warnf("attributes unavailable for %q: %v", node.Path, err)
		return
	}
	if attrs == nil {
		return
	}
	node.Size = attrs.Size
	node.Modified = attrs.ModificationTime
	node.Digest = attrs.Digests
}
