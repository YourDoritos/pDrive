# Vendored dependencies

## Proton-API-Bridge

Source: <https://github.com/rclone/Proton-API-Bridge> @ `v1.0.5` (MIT).

Wired in through a `replace` directive in the top-level `go.mod`, so the
import path stays `github.com/rclone/Proton-API-Bridge` and re-syncing with
upstream is a file copy.

### Why vendored

`docs/PROJECT_SPEC.md` anticipated this: the bridge's own README lists gaps we
have to fill (single share only, no parallel transfer, no moves/renames, no
event integration), and its crypto layer is the part Proton's announced
cryptographic migration will break. We need to be able to patch it on our own
schedule.

The immediate trigger was event access — see below.

### Local changes

| File | Change |
|---|---|
| `pdrive_access.go` | **Added.** Exposes `Client()`, `VolumeID()`, `ShareID()`. |
| `drive_test.go`, `drive_test_helper.go`, `cache_test.go`, `crypto_block_test.go` | **Removed.** Integration tests that require a live throwaway account and are destructive by design. |
| `.github/`, `testcase/` | **Removed.** Upstream CI and test fixtures. |

No upstream source file has been modified. Every local addition lives in
`pdrive_access.go`.

### Why `Client()` had to be exposed

The Drive event API (`GetLatestVolumeEventID`, `GetVolumeEvent`) lives on
`*proton.Client`, which the bridge holds unexported. Event-cursor sync is not
optional — the Proton Drive integration rules explicitly forbid frequent
recursive traversals of the file tree — so pdrive must reach it.

The alternative, constructing a second `proton.Client` from the same session,
would put two independent refresh loops on one rotating refresh token. Proton
invalidates the old token on every refresh, so the two clients would race and
log the session out. One client, exposed, is the correct answer.

### Re-syncing with upstream

```bash
go mod download github.com/rclone/Proton-API-Bridge@vX.Y.Z
cp -r "$(go env GOMODCACHE)"/github.com/rclone/\!proton-\!a\!p\!i-\!bridge@vX.Y.Z/. third_party/Proton-API-Bridge/
# then re-apply the removals above; pdrive_access.go is untouched by the copy
```
