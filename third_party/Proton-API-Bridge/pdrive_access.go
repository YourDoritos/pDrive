package proton_api_bridge

import (
	proton "github.com/rclone/go-proton-api"
)

// ---------------------------------------------------------------------------
// pDrive local addition. Not present upstream.
//
// The bridge keeps its *proton.Client unexported, so the Drive event API
// (GetLatestVolumeEventID / GetVolumeEvent) is unreachable from outside the
// package. Event-cursor sync is mandatory — the Proton Drive integration
// rules forbid recursive tree polling — so pDrive needs access to it.
//
// Creating a second client instead would mean two independent token-refresh
// loops racing over Proton's rotating refresh token, which reliably logs the
// session out. Exposing the single existing client is the correct fix.
//
// Keep this file separate from upstream sources so `git diff` against a new
// upstream release stays readable. See third_party/VENDOR.md.
// ---------------------------------------------------------------------------

// Client returns the underlying Proton API client.
func (protonDrive *ProtonDrive) Client() *proton.Client {
	return protonDrive.c
}

// VolumeID returns the volume that holds the main share. Volume events are
// the cursor pDrive follows to stay in sync.
func (protonDrive *ProtonDrive) VolumeID() string {
	if protonDrive.MainShare == nil {
		return ""
	}
	return protonDrive.MainShare.VolumeID
}

// ShareID returns the main share ID.
func (protonDrive *ProtonDrive) ShareID() string {
	if protonDrive.MainShare == nil {
		return ""
	}
	return protonDrive.MainShare.ShareID
}
