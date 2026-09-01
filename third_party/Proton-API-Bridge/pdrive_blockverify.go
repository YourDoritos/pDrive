package proton_api_bridge

import (
	"context"
	"encoding/base64"
	"fmt"

	proton "github.com/rclone/go-proton-api"
)

// ---------------------------------------------------------------------------
// pDrive local addition. Not present upstream.
//
// Proton's storage backend requires a per-block verification token in
// POST /drive/blocks. Without one every upload is rejected with
//
//	400 "You are using an outdated version of the app." (Code=2000)
//
// which has left uploads broken in every Go client, rclone included, since
// September 2025.
//
// Ported from Proton's own MIT-licensed SDK:
//   - client/js/src/internal/upload/blockVerifier.ts
//   - client/js/src/internal/upload/cryptoService.ts (verifyBlock)
//
// See third_party/VENDOR.md.
// ---------------------------------------------------------------------------

// blockVerifier holds the material needed to stamp each block of one
// revision.
type blockVerifier struct {
	// code is the 32-byte verification code for this revision.
	code []byte
}

// newBlockVerifier fetches verification material for a draft revision.
//
// The v2 volume-scoped route is what current official clients use; the older
// share-scoped route is tried as a fallback, because an upload that cannot
// obtain this material cannot proceed at all.
func (protonDrive *ProtonDrive) newBlockVerifier(ctx context.Context, linkID, revisionID string) (*blockVerifier, error) {
	volumeID := ""
	shareID := ""
	if protonDrive.MainShare != nil {
		volumeID = protonDrive.MainShare.VolumeID
		shareID = protonDrive.MainShare.ShareID
	}

	var (
		res proton.RevisionVerification
		err error
	)
	if volumeID != "" {
		res, err = protonDrive.c.GetRevisionVerification(ctx, volumeID, linkID, revisionID)
	} else {
		err = fmt.Errorf("no volume ID available")
	}
	if err != nil && shareID != "" {
		res, err = protonDrive.c.GetRevisionVerificationByShare(ctx, shareID, linkID, revisionID)
	}
	if err != nil {
		return nil, fmt.Errorf("fetch block verification data: %w", err)
	}

	code, err := base64.StdEncoding.DecodeString(res.VerificationCode)
	if err != nil {
		return nil, fmt.Errorf("decode verification code: %w", err)
	}
	if len(code) == 0 {
		return nil, fmt.Errorf("empty verification code")
	}

	return &blockVerifier{code: code}, nil
}

// token computes the verification token for one encrypted block.
//
// From the SDK's cryptoService.verifyBlock:
//
//	verificationToken[i] = verificationCode[i] XOR (encryptedData[i] || 0)
//
// The token is always the length of the verification code; the ciphertext is
// treated as zero-padded when it is shorter, which happens for the final
// block of a small file.
func (v *blockVerifier) token(encryptedBlock []byte) string {
	out := make([]byte, len(v.code))
	for i := range v.code {
		var b byte
		if i < len(encryptedBlock) {
			b = encryptedBlock[i]
		}
		out[i] = v.code[i] ^ b
	}
	return base64.StdEncoding.EncodeToString(out)
}
