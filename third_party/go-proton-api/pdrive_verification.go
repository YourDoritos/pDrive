package proton

import (
	"context"

	"github.com/go-resty/resty/v2"
)

// ---------------------------------------------------------------------------
// pDrive local addition. Not present upstream.
//
// Proton's storage backend requires a per-block verification token in
// POST /drive/blocks. Uploads without one are rejected with
//
//	400 "You are using an outdated version of the app." (Code=2000)
//
// This file adds the endpoint that supplies the verification material. The
// token derivation itself lives in the bridge, next to block encryption.
//
// Endpoint and response shape ported from Proton's own MIT-licensed SDK,
// client/js/src/internal/upload/apiService.ts (getVerificationData).
// See third_party/VENDOR.md.
// ---------------------------------------------------------------------------

// RevisionVerification carries the material needed to compute per-block
// upload verification tokens.
type RevisionVerification struct {
	// VerificationCode is 32 bytes, base64-encoded.
	VerificationCode string
	// ContentKeyPacket is the revision's content key packet, base64-encoded.
	ContentKeyPacket string
}

// GetRevisionVerification fetches verification material for a draft revision
// using the v2 volume-scoped route, which is what current official clients
// call.
func (c *Client) GetRevisionVerification(ctx context.Context, volumeID, linkID, revisionID string) (RevisionVerification, error) {
	var res struct {
		RevisionVerification
	}

	if err := c.do(ctx, func(r *resty.Request) (*resty.Response, error) {
		return r.SetResult(&res).Get("/drive/v2/volumes/" + volumeID +
			"/links/" + linkID + "/revisions/" + revisionID + "/verification")
	}); err != nil {
		return RevisionVerification{}, err
	}

	return res.RevisionVerification, nil
}

// GetRevisionVerificationByShare fetches the same material using the older
// share-scoped route. Kept as a fallback: which routes an account is served
// varies, and an upload that cannot fetch verification material cannot
// proceed at all.
func (c *Client) GetRevisionVerificationByShare(ctx context.Context, shareID, linkID, revisionID string) (RevisionVerification, error) {
	var res struct {
		RevisionVerification
	}

	if err := c.do(ctx, func(r *resty.Request) (*resty.Response, error) {
		return r.SetResult(&res).Get("/drive/shares/" + shareID +
			"/links/" + linkID + "/revisions/" + revisionID + "/verification")
	}); err != nil {
		return RevisionVerification{}, err
	}

	return res.RevisionVerification, nil
}
