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
| `drive.go` | **Modified.** `listAllVolumes` and `getAllShares` now run concurrently in `NewProtonDrive`; they are independent, and the latter feeds only the main-share integrity check. Removes one round trip from every startup. |
| `common/keyring.go` | **Comment only.** Records why `GetUser` and `GetAddresses` must stay sequential — see below. |
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

### Why the first two API calls must stay sequential

`getAccountKRs` fetches the user and the addresses one after the other. They
are independent and each costs a round trip, so parallelising them looks like
free money. It is not, and this was measured rather than reasoned about.

They are the **first** API calls a process makes. If the stored access token
has expired, running them concurrently means both receive a 401 and each fires
its own `POST /auth/v4/refresh`. Proton rotates the refresh token and
invalidates the previous one on every refresh, so two refreshes in flight is a
race that can spend the token twice and leave the session dead with
`Code=10013` — the same failure that cost us a session in Phase 1.

Observed directly while benchmarking, with `PDRIVE_TRACE=1`:

```
[trace] #1  114ms  401 GET  /core/v4/users
[trace] #2  115ms  401 GET  /core/v4/addresses
[trace] #3  194ms  200 POST /auth/v4/refresh     <- two concurrent refreshes
[trace] #5  152ms  200 POST /auth/v4/refresh     <- on a rotating token
```

The change was reverted. The rule: **the first call of a process serialises the
token refresh; parallelism is only safe afterwards.** The concurrent
volumes/shares fetch in `drive.go` runs after the keyrings and is therefore
fine.

### Re-syncing with upstream

```bash
go mod download github.com/rclone/Proton-API-Bridge@vX.Y.Z
cp -r "$(go env GOMODCACHE)"/github.com/rclone/\!proton-\!a\!p\!i-\!bridge@vX.Y.Z/. third_party/Proton-API-Bridge/
# then re-apply the removals above; pdrive_access.go is untouched by the copy
```
