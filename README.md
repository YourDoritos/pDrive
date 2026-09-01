# pdrive

A **Proton Drive sync client for Linux** with a terminal UI. Written in Go.

One folder — `~/pdrive` — synced both ways, continuously, in the background.
Put a file in it and it's in the cloud. Add a file on another machine and it
shows up in your *first* `ls`, not the next one.

> **This is a third-party application not officially supported by Proton.**
> pdrive is not affiliated with or endorsed by Proton AG.

## Why

Proton ships no Linux desktop client. The official `proton-drive` CLI has no
sync engine — by design, "only the applications include a full synchronization
engine that runs in the background." A GUI client was announced in June 2026
for "before the end of 2026", with no beta and no date.

The working alternative today is `rclone bisync`, which is a scriptable file
transfer tool rather than a sync daemon: no live freshness, no conflict copies,
no move detection, no UI.

pdrive uses rclone's excellent Proton *libraries* but owns the sync engine, the
state model, and the UX.

## What makes it feel local

Proton Drive offers no webhooks or push channel, so every third-party client
polls. Polling means a stale window — the thing that makes cloud folders on
Linux feel like cloud folders.

pdrive removes it by intercepting the directory open. An optional root helper
(`pdrive-gate`) holds `opendir(2)` via `fanotify` `FAN_OPEN_PERM` for a few
hundred milliseconds while the daemon reconciles that directory's metadata,
then releases it. The listing you get is the listing *after* the sync.

Measured on Linux 7.1.9:

| | |
|---|---|
| Overhead per listing, nothing to do | 0.013 ms → 0.084 ms (+71 µs) |
| Daemon killed mid-hold | `ls` returns immediately, exit 0 (kernel fails open) |
| `cd` into a synced folder | never blocked — `chdir(2)` opens no fd |

The hold never waits on file content — only on directory metadata — so a 5 GB
video's *name* appears instantly and the bytes stream in behind it.

The gate is optional. Without it, pdrive falls back to inotify hints plus
adaptive polling and still works, just with a stale window.

## Status

**Phase 0 — authentication.** SRP login with TOTP and two-password accounts,
encrypted session persistence, and verification that the PGP key hierarchy
actually unlocks.

**Phase 0.5 — verified backup.** `pdrive backup` mirrors the whole account to
disk and re-hashes every file. Read-only against Proton by construction. It
writes both a `manifest.json` and a `sha1sum(1)`-compatible `MANIFEST.sha1`, so
the copy can be re-checked with standard tools without trusting pdrive:

```bash
pdrive backup                     # ~/pdrive-backup-<date>, then verifies
pdrive verify <dir>               # re-check offline, any time later
cd <dir> && sha1sum -c MANIFEST.sha1
```

**Phase 1 — read-only mirror.** `pdrive sync` mirrors the account into
`~/pdrive`: a full walk on first run, then event-cursor replay. Modification
times are preserved, files over `max_auto_download_size` land as visible
`.pdrive-stub` placeholders, and remote deletions go to a local trash rather
than being unlinked.

```bash
pdrive sync              # bring the folder up to date
pdrive get <path>        # fetch a file left as a stub
pdrive status            # what the mirror holds (offline)
```

Still download-only — nothing writes to your account.

See [`docs/PROJECT_SPEC.md`](docs/PROJECT_SPEC.md) for the full design and the
phase plan.

## Build

```bash
make build      # static binary into ./bin
make install    # installs to ~/.local/bin (no sudo — pdrive runs as you)
make test
pdrive          # terminal UI
```

## Paths

| What | Where |
|---|---|
| Sync folder | `~/pdrive` |
| Config | `~/.config/pdrive/config.toml` |
| Session + state | `~/.local/state/pdrive/` |
| Local trash | `~/.local/share/pdrive/trash/` |

## Security notes

Proton offers no app passwords, service accounts, or scoped tokens, so an
unattended sync daemon necessarily holds full-account credentials. pdrive
stores its session encrypted at rest (Argon2id + NaCl secretbox, keyed from
`/etc/machine-id`) with `0600` permissions. That protects against a copied
file, not against someone who already has read access to your home directory.

`pdrive-gate` runs as root because `fanotify` permission events require
`CAP_SYS_ADMIN`. It is deliberately tiny — it holds a fanotify fd and a socket,
performs no network I/O, no cryptography, and never touches your credentials.
It never denies an open, only delays one, and it fails open on every error path.

## License

GPL-3.0. See [LICENSE](LICENSE).
