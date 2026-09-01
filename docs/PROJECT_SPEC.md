# pdrive - Proton Drive Sync Client for Linux

> "A project is only as good as the plan" - Sun Tzu (probably)

## Overview

A Proton Drive sync client written in Go with a TUI, in the same shape as
[pVPN](https://github.com/YourDoritos/pvpn). One folder (`~/pdrive`), synced both ways,
continuously. Static binaries, zero runtime dependencies.

**Why this exists:** Proton has no Linux desktop client. The official `proton-drive` CLI
(TypeScript, v0.8.0) explicitly ships no sync engine - "only the applications include a full
synchronization engine that runs in the background." A GUI client was announced June 2026 for
"before the end of 2026", with no beta and no date. The only working option today is
`rclone bisync`: a scriptable transfer tool, not a sync daemon. No live freshness, no conflict
copies, no move detection, no TUI.

**Not an rclone wrapper.** We use rclone's *libraries* (`rclone/go-proton-api`,
`rclone/Proton-API-Bridge`) but own the sync engine, the state model, and the UX.

## Design Philosophy: It Should Feel Local

**The user should never press refresh, and should never think about the lag.**

- **Install:** `yay -S pdrive`
- **Run:** `pdrive` -> login once -> done
- **Use:** put a file in `~/pdrive`, it's in the cloud. `ls` on the other machine shows it
  *in that same `ls`* - not on the next one.

Conflicts never lose data. Deletions are recoverable on both sides. Always.

---

## Freshness: block the listing (VERIFIED)

Proton Drive has **no webhooks and no push channel** for third parties. The SDK mandates
event-cursor polling and forbids tree-walking:

> **Use event-based sync** - Do not poll the API or perform frequent recursive traversals of
> the file tree. Excessive polling or recursion may cause your application and your account to
> be rate-limited. -- ProtonDriveApps/sdk README

Timer-based polling means a stale window. We remove it by **intercepting the directory open**
and holding it until we've reconciled that directory's metadata.

### Measured on this machine (kernel 7.1.9-arch1-2)

A test daemon using `fanotify_init(FAN_CLASS_CONTENT)` + `fanotify_mark(FAN_OPEN_PERM |
FAN_ONDIR)` on a directory:

| Result | Measurement |
|---|---|
| `FAN_OPEN_PERM \| FAN_ONDIR` mark on a directory | **Accepted.** Directory permission events work. |
| `ls` triggers `OPEN_PERM`, daemon holds 300 ms, creates a file, then `FAN_ALLOW` | **The new file appears in that same `ls` output.** The block happens at `openat(O_DIRECTORY)`, before `getdents64`. |
| `python3 os.listdir` (what a file manager does) | Identical behaviour - blocked and saw the new entry |
| `cd` into the directory | **No event.** `chdir(2)` opens no fd, so `cd` is never blocked. |
| Overhead per listing, daemon allows immediately | baseline **0.013 ms** -> watched **0.084 ms** (+71 microseconds, p90 0.088 ms) |
| **Daemon killed while holding an event** | **`ls` returned immediately, exit 0.** Kernel closes the fanotify fd on process death and auto-allows all pending permission events. Crash = fail open, by design. |

Those two last rows are what make this acceptable: the tax on a listing with nothing to do is
~70 microseconds, and a dead daemon cannot wedge the filesystem.

### The rule that keeps the block bounded: metadata only

**The block never waits on file content.** During a held `OPEN_PERM` we do exactly one thing:
apply the Drive event delta for *that directory* to the local directory entries - create,
rename, remove. Content downloads are queued and run *after* `FAN_ALLOW`.

So the hold is O(1 API round trip), never O(bytes). Your 5 GB video does not get downloaded
before `ls` returns; its *name* shows up, and the bytes arrive behind it.

### Sequence

```
opendir("~/pdrive/foo")
  -> FAN_OPEN_PERM  -> pdrive-gate (root)  -> pdrived (user)
       pdrived:
         a) event poll succeeded < fresh_window (2s) ago?  -> ALLOW now, no network
         b) otherwise: one GetVolumeEvent delta call
            apply metadata for this dir only (entries, stubs, renames, removals)
            queue any content downloads
         c) hard deadline max_block_ms (default 400ms) -> ALLOW regardless
  -> FAN_ALLOW
getdents64()  -> sees the new entries
```

Step (a) matters: it stops `ls` in a loop from hammering Proton. At most one event poll per
`fresh_window`, and the poll is a single cursor delta, not a tree walk.

### Split privilege: keep root tiny

Two processes. The root one must be small enough to audit in one sitting.

| Process | Runs as | Holds | Does NOT have |
|---|---|---|---|
| **`pdrive-gate`** | root (`CAP_SYS_ADMIN`) | the fanotify fd, the marks, a unix socket to `pdrived` | no network, no crypto, no credentials, no sqlite, no Proton code at all |
| **`pdrived`** | your user | credentials, sqlite, sync engine, all Proton I/O | no privileges |

`pdrive-gate` is a loop: read event -> ask `pdrived` -> `FAN_ALLOW`. Target under ~400 lines.

**Gate safety rules (all mandatory):**

1. **Marks are scoped.** `FAN_MARK_ADD` per directory inside the sync root, maintained as
   directories appear/disappear. **Never `FAN_MARK_FILESYSTEM`** - we do not intercept `$HOME`.
2. **Hard deadline.** `max_block_ms` (default 400). On expiry: `FAN_ALLOW`, log a freshness
   miss. Proton latency must never become filesystem latency.
3. **Self-exclusion.** `pdrived` sends its pid at handshake; events with that pid are allowed
   instantly without a round trip. Prevents the classic HSM self-deadlock when the daemon
   writes into the tree it is watching.
4. **Fail open, always.** Socket gone, `pdrived` unresponsive, heartbeat missed, queue
   overflow (`FAN_Q_OVERFLOW`) -> allow everything and drop all marks.
5. **Never `FAN_DENY`.** The gate has no reason to refuse an open. It only ever delays.
6. **Degrades cleanly.** No gate installed, or gate not running -> `pdrived` falls back to the
   inotify + adaptive-polling path below. The product still works, just with a stale window.

### Fallback tier (no root, or gate disabled)

Also verified with plain unprivileged inotify: `ls` and file-manager listings produce
`OPEN,ISDIR` + `ACCESS,ISDIR`; `cd` and `stat` produce nothing. So without the gate we still
get an "someone is looking" hint, just after the fact:

```
idle   (no activity > 5m) -> poll cursor every 60s
active (activity < 5m)    -> poll cursor every 5s
immediate poll (debounced 1/s) on: IN_OPEN|ISDIR, resume from suspend (D-Bus
  PrepareForSleep), network-up, TUI attach, pdrivectl sync
```

In this tier a GUI file manager still self-refreshes (it holds its own inotify watch), so the
file appears in an open window ~1s later. Terminal `ls` needs a second one.

### Rejected

| Approach | Why not |
|---|---|
| FUSE mount (`rclone mount` style) | Perfect freshness, but the folder stops being a real folder: no offline access, daemon crash unmounts your data, every other app inherits the failure mode. We want real files on disk. |
| `FAN_MARK_FILESYSTEM` | Intercepts every open on the whole filesystem. Unacceptable blast radius. |

---

## Large files: size cap and placeholders

500 GB of account against a laptop SSD means "download everything" is wrong by default.

### Phase 1 - hard size cap (no root required, always safe)

`max_auto_download_size` (default `0` = unlimited; suggested `512MiB`). Files above it are not
downloaded. In their place, a **distinctly named stub**:

```
big-video.mp4.pdrive-stub     <- small text file: real name, size, revision, how to fetch
```

A separate filename is deliberate. It can **never** be mistaken for the real file by any
program, at any time, daemon running or not. `pdrivectl get <path>` (or the TUI) fetches on
demand and replaces the stub with the real file.

### Phase 5 - transparent placeholders (needs the gate)

Replace stubs with **sparse files at the real name and the real size**, tagged
`user.pdrive.stub=1`, with a per-inode `FAN_OPEN_PERM` mark. Opening one blocks, downloads,
clears the mark, allows. This is textbook HSM and exactly what the kernel API is for.

`FAN_PRE_ACCESS` (Linux 6.14+, present in your headers) is the better long-term primitive: it
carries a `FAN_EVENT_INFO_TYPE_RANGE` record, so a video player can start on the first
megabytes instead of waiting for 20 GB. It needs `FAN_CLASS_PRE_CONTENT`, and it **disables
readahead** on watched files - fine for placeholders, not for the whole tree.

**The corruption risk this introduces, and the rule that removes it:** a sparse placeholder
read while nothing is populating it returns **zeros, silently**. That is data loss the moment
someone copies it to a backup. Therefore:

- `pdrived` creates sparse placeholders **only while the gate is confirmed alive**.
- On gate loss or clean shutdown, every placeholder is converted back to a `.pdrive-stub`
  before the mark disappears.
- Every placeholder carries the xattr, so recovery after a hard crash is a scan, not a guess.
- `pdrivectl doctor` finds and repairs orphaned placeholders.

Phase 1's ugly-but-safe stubs ship first and remain the fallback forever.

---

## Architecture

```
+------------------+     +---------------------+
|   TUI (pdrive)   |     |   CLI (pdrivectl)   |
+--------+---------+     +----------+----------+
         |  unix socket (JSON IPC, reused from pVPN)  |
         +--------------------+----------------------+
                              v
                 +------------------------------+
                 |   pdrived    (your user)     |
                 |                              |
                 |  sync engine / reconciler    |----> Proton Drive API
                 |  state db (sqlite)           |      go-proton-api
                 |  event poller (cursor)       |      Proton-API-Bridge
                 |  watcher (inotify)           |
                 |  xfer (blocks, rate limit)   |
                 +--------------+---------------+
                                ^
                                | unix socket: "dir opened, hold or go?"
                                | (deadline max_block_ms, fail open)
                 +--------------+---------------+
                 |   pdrive-gate  (root, tiny)  |
                 |   fanotify FAN_OPEN_PERM     |
                 |   ~400 lines, no network     |
                 +------------------------------+
```

| Component | Responsibility |
|---|---|
| **`pdrive`** | TUI: login, status, activity, conflicts, selective sync, settings |
| **`pdrivectl`** | `status`, `sync`, `get`, `pause`, `resume`, `conflicts`, `doctor`. Waybar/tmux friendly |
| **`pdrived`** | Unprivileged. Sync loop, credentials, state. `systemd --user` + linger |
| **`pdrive-gate`** | Root. fanotify only. `systemd` system unit, `Restart=always` |
| **Sync engine** | Three-way reconciliation, conflicts, moves, safety guards. **The actual product** |
| **State DB** | sqlite. Single source of truth for "what did we last agree on" |

---

## Dependencies

| Module | Purpose | License |
|---|---|---|
| `github.com/rclone/go-proton-api` | API client incl. `event_drive.go` (`GetLatestVolumeEventID`, `GetVolumeEvent`) | MIT |
| `github.com/rclone/Proton-API-Bridge` | Drive crypto: shares, node keys, block up/download | MIT |
| `github.com/ProtonMail/go-srp` | SRP login. **Already in pVPN** | MIT |
| `github.com/ProtonMail/gopenpgp/v3` | PGP key hierarchy | MIT |
| `charmbracelet/bubbletea`+`bubbles`+`lipgloss` | TUI, same versions as pVPN | MIT |
| `github.com/fsnotify/fsnotify` | inotify (fallback tier) | BSD-3 |
| `modernc.org/sqlite` | State DB. **Pure Go, no cgo** - keeps static binaries and AUR clean | BSD-3 |
| `golang.org/x/sys/unix` | `fanotify_init`/`fanotify_mark`, xattr | BSD-3 |
| `golang.org/x/time/rate` | Bandwidth limiting | BSD-3 |
| `github.com/godbus/dbus/v5` | Suspend/resume, network signals. Already in pVPN | BSD-2 |
| `github.com/BurntSushi/toml` | Config, same as pVPN | MIT |

**Vendor `Proton-API-Bridge`** when we start extending it. Its own README lists the gaps we
must fill: single share only, no parallel transfer, no moves/renames, no event integration.
Upstream (`henrybear327/*`) was archived Jan 2025; rclone forked it and maintains it (last push
Aug 2026).

**The "no 2FA login" gap does not apply to us.** The bridge accepts a
`common.ReusableCredentialData{UID, AccessToken, RefreshToken, SaltedKeyPass}` with
`UseReusableLogin: true` — exactly what `internal/api` already produces. pdrive performs its
own SRP + TOTP login and hands the finished session over, so 2FA accounts work through a
library that cannot do 2FA itself. Verified in Phase 0.5 against a live 2FA account.

**Reference, do not link:** `ProtonDriveApps/sdk` (MIT, TypeScript + C#) is the *official*
implementation and our specification. `cli/src/` has `credentials/`, `events/`, `cache/`
modules. Nothing needs reverse engineering; porting TS semantics to Go is mechanical.

### Reused from pVPN

| pVPN source | Reuse |
|---|---|
| `internal/api/auth.go` | SRP + 2FA login, near-verbatim |
| `internal/api/session.go` | Argon2id + NaCl secretbox session store, verbatim |
| `internal/api/client.go` | Token refresh, retry, backoff |
| `internal/ipc/` | Unix socket JSON protocol, verbatim |
| `internal/tui/` | Screen shell, theme, keybinds, login form |
| `internal/network/` | Network-up detection |
| `dist/`, `install.sh` | Packaging (adapted: one `--user` unit + one system unit) |

**Do not share the pVPN session.** Drive needs the PGP key passphrase pVPN never holds, and
two processes sharing one session race on refresh-token rotation and log each other out.

---

## Proton Drive API Surface

Auth: same `/core/v4/auth/info` -> SRP -> `/core/v4/auth` -> `/core/v4/auth/2fa` as pVPN, then
unlock user keys with the key passphrase, then address keys.

```
account password + key salt -> key passphrase
  -> user key -> address key -> share key -> node key -> content key -> blocks (~4 MB)
                                  + hash key (HMAC for child-name lookup)
```

| Area | Calls |
|---|---|
| Bootstrap | list volumes, main share, root link |
| Listing | list children (paginated), get link |
| Upload | create file, request block URLs, upload blocks, commit revision |
| Download | get revision, fetch blocks, decrypt + verify |
| Mutation | create folder, move, rename, trash, delete |
| **Sync** | `GetLatestVolumeEventID`, `GetVolumeEvent` -> Create/Update/UpdateMetadata/Delete |

### Mandatory compliance (SDK README - non-negotiable)
- `x-pm-appversion: external-drive-pdrive@0.1.0-alpha` on every request, honest, never
  masquerading as first-party. Non-compliant clients "may be limited or blocked."
- Official endpoints only. Event-based sync only, no recursive polling.
- No Proton branding, logos, or trademarks.
- Login screen must display: *"This is a third-party application not officially supported by
  Proton."*
- Personal / non-commercial, which is what this is.

---

## Sync Engine

### State DB (`~/.local/state/pdrive/state.db`)

```sql
CREATE TABLE nodes (
  path           TEXT PRIMARY KEY,   -- relative to sync root
  node_id        TEXT NOT NULL,      -- Proton link ID
  parent_id      TEXT,
  is_dir         INTEGER NOT NULL,
  revision_id    TEXT,               -- last synced remote revision
  content_hash   TEXT,               -- SHA1 of plaintext, the agreed value
  size           INTEGER,
  local_mtime_ns INTEGER,            -- fast-path hint only, NOT authoritative
  local_inode    INTEGER,            -- move detection
  materialized   INTEGER,            -- 0 = stub/placeholder, 1 = real bytes on disk
  synced_at      INTEGER
);
CREATE INDEX nodes_by_node_id ON nodes(node_id);
CREATE INDEX nodes_by_inode   ON nodes(local_inode);

CREATE TABLE meta      (key TEXT PRIMARY KEY, value TEXT);
  -- event_cursor, volume_id, share_id, root_link_id, schema_version
CREATE TABLE pending   (path TEXT PRIMARY KEY, direction TEXT, revision_id TEXT,
                        blocks_done INTEGER, total_blocks INTEGER, updated_at INTEGER);
CREATE TABLE conflicts (path TEXT, kept_local TEXT, remote_revision TEXT, at INTEGER);
```

`nodes` is the **baseline**: the last state both sides agreed on.

### Reconciliation (three-way, per path)

```
base  = nodes row
local = stat + hash (hash only when size or mtime differs from base)
remote = event delta applied to last known remote state

 !local_changed && !remote_changed -> no-op
  local_changed && !remote_changed -> upload (new revision)
 !local_changed &&  remote_changed -> download (atomic: temp + fsync + rename)
  local_changed &&  remote_changed -> hashes equal ? converge baseline : CONFLICT
```

### Conflict policy: never lose data

Remote keeps the canonical path; your local version is preserved beside it and also uploaded:

```
report.pdf
report (conflict 2026-09-01 20-15 hostname).pdf
```

No silent-overwrite mode. No last-writer-wins. Logged to `conflicts`, surfaced in TUI and
`pdrivectl status`.

### Move / rename detection
Before treating a disappearance as a delete, look for an untracked local file with the same
`local_inode`, or the same `content_hash` + size. If found, issue a remote move/rename instead
of delete + re-upload.

### Safety guards (where OneDrive actually fails)

1. **Deletion cliff** - a pass that would delete more than `deletion_guard_percent` (default
   25%) of tracked nodes on either side: **stop**, notify, require `--confirm-deletions`.
   **Also requires an absolute floor of 10 deletions.** A percentage alone is useless on a
   small account — removing one file of two is 50%, so the guard would fire during completely
   ordinary use, users would learn to bypass it reflexively, and it would protect nothing when
   it mattered. Small accounts are not left exposed: every removal still goes to the local
   trash under guard 4.
2. **Missing root** - root absent, not a directory, or empty while the DB has entries:
   **stop**. An unmounted or renamed folder is not "the user deleted everything."
3. **Local deletes -> Proton trash.** Never permanent-delete remotely.
4. **Remote deletes -> local trash** (`~/.local/share/pdrive/trash/`, `trash_retention_days`
   default 30). Never bare `unlink`.
5. **Atomic writes** - download to `.pdrive-tmp/` on the same filesystem, `fsync`, `rename(2)`.
   A crashed download never leaves a truncated file at the real path.
6. **Single instance** - flock on the state DB.
7. **Partial-listing abort** - a listing that errors mid-page aborts the pass. Never reconcile
   against a partial view of the remote.
8. **Placeholder integrity** - sparse placeholders only while the gate is alive; converted back
   to stubs otherwise; xattr-tagged; `pdrivectl doctor` repairs orphans.

### Ignore rules
Built in: `.pdrive-tmp/`, `*.part`, `*.crdownload`, `.goutputstream-*`, `~$*`, `.~lock.*`,
`*.swp`, `.DS_Store`, `Thumbs.db`. Plus `.pdriveignore` (gitignore syntax) at the root.

### Filenames
Proton forbids duplicate names in a folder; invalid UTF-8 replaced, edge spaces stripped.
Normalize on upload, keep the mapping in `nodes.path`, detect case-collisions before they
become a sync loop.

### Modification times — RESOLVED, they are available

rclone's backend documentation says Proton Drive does not support mtime. **That is out of
date.** Verified against a live account in Phase 0.5: Proton stores the modification time in
the file's encrypted extended attribute, and the bridge surfaces it as
`FileSystemAttrs.ModificationTime` with sub-second precision:

```
"modified": "2026-07-14T09:20:01.455Z"
"modified": "2026-02-23T14:14:59.6666406Z"
```

The same xattr also carries **Proton's own SHA1 of the plaintext**
(`FileSystemAttrs.Digests`), which gives us an end-to-end integrity check that does not depend
on our download path being correct. Both files in the Phase 0.5 backup matched it exactly.

Design consequence: mtime is usable in the reconciler, not merely a hint. The state DB stays
authoritative and `size + SHA1` remains the tiebreak, because the attribute block is documented
as sometimes absent ("Might return nil when xattr is missing"). A file without it still syncs;
it just costs a hash.

---

## Project Structure

```
pdrive/
├── cmd/
│   ├── pdrive/          # TUI
│   ├── pdrived/         # user daemon
│   ├── pdrivectl/       # CLI
│   └── pdrive-gate/     # root fanotify helper (small, auditable)
├── internal/
│   ├── api/             # auth, session, client        <- from pVPN
│   ├── drive/           # Drive API + crypto (vendored bridge + extensions)
│   ├── sync/            # reconciler, conflicts, moves, guards
│   ├── state/           # sqlite store + migrations
│   ├── gate/            # fanotify syscalls, mark management, gate protocol
│   ├── watcher/         # inotify (fallback tier + local change detection)
│   ├── poller/          # event cursor, adaptive cadence
│   ├── xfer/            # block up/download, resume, rate limiting, placeholders
│   ├── ipc/             # unix socket protocol         <- from pVPN
│   ├── config/          # TOML + XDG paths             <- from pVPN
│   └── tui/             # screens                      <- from pVPN
├── dist/                # PKGBUILD, pdrived.service (user), pdrive-gate.service (system)
└── docs/PROJECT_SPEC.md
```

| What | Where |
|---|---|
| Sync root | `~/pdrive` (configurable) |
| Config | `~/.config/pdrive/config.toml` |
| State DB + session | `~/.local/state/pdrive/{state.db,session.enc}` |
| Local trash | `~/.local/share/pdrive/trash/` |
| Logs | `~/.local/state/pdrive/pdrive.log` (rotated) |
| Scratch | `~/pdrive/.pdrive-tmp/` (same fs -> atomic rename) |
| Sockets | `$XDG_RUNTIME_DIR/pdrive.sock`, `/run/pdrive-gate.sock` |

---

## Configuration

```toml
[sync]
root                   = "~/pdrive"
deletion_guard_percent = 25
trash_retention_days   = 30
max_auto_download_size = 0        # 0 = unlimited; e.g. "512MiB"

[freshness]
gate            = true    # fanotify blocking (needs pdrive-gate, root)
max_block_ms    = 400     # hard ceiling on how long a listing may be held
fresh_window    = "2s"    # skip the network if we polled this recently
idle_interval   = "60s"   # fallback tier
active_interval = "5s"
active_window   = "5m"

[limits]
upload_kbps            = 0
download_kbps          = 0
max_parallel_transfers = 4

[selective]
exclude = []
```

---

## Known Limitations & Risks

| Risk | Severity | Mitigation |
|---|---|---|
| **Proton crypto model change, end 2026 / early 2027.** Clients on the old crypto "will not interoperate until upgraded." | **High** | All crypto behind `internal/drive` interfaces from day one. Watch the SDK changelog. Hits rclone identically. |
| Gate runs as root | Medium | ~400 lines, no network, no crypto, no credentials, never `FAN_DENY`, fail-open on every error path. Optional - product works without it. |
| Live-but-stuck `pdrived` wedging listings | Medium | `max_block_ms` hard deadline in the gate, enforced by the gate, not by `pdrived`. Verified: gate *death* already fails open at kernel level. |
| Sparse placeholders read as zeros with no populator | Medium | Phase 5 only; xattr tag, gate-alive precondition, convert-to-stub on shutdown, `doctor` repair. Phase 1 stubs have a different filename and cannot be misread. |
| No app passwords / scoped tokens - daemon holds full account credentials | Medium | Same tradeoff pVPN already makes. Argon2id + secretbox at rest; optional TOTP prompt instead of a stored secret. |
| Rate limiting shared with first-party clients | Medium | Cursor deltas only, `fresh_window` throttle, exponential backoff, honest `x-pm-appversion`. |
| Bridge gaps (2FA, single share, no moves, no parallelism) | Medium | Vendored and extended. Real work, budget for it. |
| mtime possibly unsupported | Low | Already treated as a hint. |
| Proton could restrict third-party clients | Low | Comply with every published rule. |

---

## Development Phases

### Phase 0 - Skeleton — DONE
Repo, module, Makefile, golangci mirroring pVPN. Port `internal/api`, `internal/config`,
`internal/ipc`, TUI shell. Login screen with the third-party disclosure. `x-pm-appversion`
wired in.
**Done when:** `pdrive` logs in with SRP + 2FA and prints the account's storage quota.
**-> You log in here, then we have a live account for everything after.**

### Phase 0.5 - Back up the account (HARD GATE) — DONE
Full recursive download of the account to `~/pdrive-backup-<date>/`, with a SHA1 manifest,
verified. Re-verify before Phase 2.

Shipped as `pdrive backup` / `pdrive verify`. Read-only against Proton by construction: it
lists and downloads, and calls nothing that mutates. Writes `manifest.json` plus a
sha1sum(1)-compatible `MANIFEST.sha1`, so the copy can be re-checked with standard tools
without trusting pdrive. Every server-supplied name is validated before it becomes a path
(`internal/backup/safepath.go`) — a name like `../../.bashrc` can never escape the destination.
Atomic writes throughout; `--resume` skips files already present and intact; refuses to write
into a non-empty directory without it.

First run: 2 files / 528.4 KiB, verified clean by our own re-hash, by `sha1sum -c`, and against
Proton's own stored digests.

### Phase 1 - Read-only mirror (cloud -> local) — DONE
Bridge vendored (`third_party/`, see VENDOR.md). State DB with migrations. Full mirror on
first run, event replay thereafter. `max_auto_download_size` + `.pdrive-stub` + `pdrive get`.

Shipped as `pdrive sync` / `pdrive get` / `pdrive status`. Download-only: it calls nothing
that mutates the account.

The reconciler talks to a `mirror.Source` interface rather than to Proton, so the whole engine
runs against an in-memory fake with no network — which is what makes the adversarial tests
below possible, and what Phase 2 will lean on heavily.

Verified against the live account: full mirror, event replay with no walk, mtime preserved to
sub-second precision, size cap producing a stub, `pdrive get` materialising it byte-identically
to the Phase 0.5 backup.

Confirmed end to end: a file added through the Proton Drive web UI (`harvy.js`) arrived in
`~/pdrive` on the next `pdrive sync` through **event replay with no tree walk**, with its
remote modification time intact.

### Phase 2 - Bidirectional
Local scan + hashing, inotify change detection, debounced upload. Block upload + revision
commit; extend the bridge for 2FA-authed sessions. Full reconciler, conflict copies, move
detection. All eight safety guards, with tests that actively try to destroy data.
**Done when:** two machines converge under concurrent edits and nothing is ever lost.

### Phase 3 - Daemon + gate
Split `pdrived` / `pdrive` / `pdrivectl`; `systemd --user` + linger. Build `pdrive-gate`:
fanotify marks over the tree, gate protocol, deadline, self-exclusion, fail-open, system unit.
Fallback tier (inotify + adaptive cadence) for gate-less installs. `pdrivectl status` in
waybar/tmux format for your Hyprland bar.
**Done when:** a file added on machine A shows up in the *first* `ls` on machine B, and
`systemctl stop pdrive-gate` degrades to the fallback without a hiccup.

### Phase 4 - Polish & ship
Bandwidth limits, parallel transfers, selective sync, `.pdriveignore`. Conflict browser +
activity log in the TUI, desktop notifications. Log rotation, `pdrivectl doctor`. PKGBUILD,
AUR, README with screenshots.
**Done when:** `yay -S pdrive` works on a clean machine.

### Phase 5 - Transparent placeholders
Sparse placeholders at real names + xattr + per-inode `FAN_OPEN_PERM`; then `FAN_PRE_ACCESS`
range-populate so large media starts playing before it finishes downloading. All of the
placeholder-integrity rules above.
Also: thumbnails, photos/albums, share links, multiple shares.

---

## Testing Strategy

- Reconciler unit tests over all 9 (local x remote) state combinations
- A fake Drive backend behind the `internal/drive` interface - sync engine testable with no
  network and no account
- **Gate tests:** hold-then-allow ordering, deadline expiry, self-exclusion, socket loss,
  queue overflow, `SIGKILL` mid-hold (must fail open - already verified by hand)
- **Adversarial data-loss suite:** kill mid-upload, mid-download, mid-rename; unmount the root;
  corrupt the state DB; return partial listings; inject a mass-delete event. Every case must
  end with zero user data lost.
- Two-machine convergence against the real account, after Phase 0.5's backup

## Build & Run

```bash
make build           # static binaries into ./bin
make install         # binaries + systemd units + linger
sudo systemctl enable --now pdrive-gate     # optional, for blocking freshness
pdrive               # TUI
pdrivectl status     # scriptable
```

## References

- [ProtonDriveApps/sdk](https://github.com/ProtonDriveApps/sdk) - official SDK + CLI source, MIT
- [rclone/Proton-API-Bridge](https://github.com/rclone/Proton-API-Bridge) - Drive crypto in Go, MIT
- [rclone/go-proton-api](https://github.com/rclone/go-proton-api) - API client + drive events, MIT
- [Proton Drive CLI announcement](https://proton.me/blog/proton-drive-cli)
- [Drive SDK update, Jan 2026](https://proton.me/blog/drive-sdk-january-2026)
- [fanotify pre-content hooks (LWN)](https://lwn.net/Articles/985013/)
- [An update on fanotify (LWN)](https://lwn.net/Articles/1075829/)
