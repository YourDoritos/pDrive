# pDrive

A **Proton Drive sync client for Linux** with a terminal UI. Written in Go.

One folder — `~/pdrive` — synced both ways, continuously. Put a file in it and
it is in the cloud. Add a file on another machine and it shows up here, without
you asking.

> **This is a third-party application not officially supported by Proton.**
> pDrive is not affiliated with or endorsed by Proton AG.

## Why

Proton ships no Linux desktop client. The official `proton-drive` CLI has no
sync engine — by design, "only the applications include a full synchronization
engine that runs in the background." A GUI client was announced in June 2026
for "before the end of 2026", with no beta and no date.

The working alternative today is `rclone bisync`, which is a scriptable file
transfer tool rather than a sync daemon: no live freshness, no conflict copies,
no move detection, no UI. Its upload path has also been broken since September
2025 (see below).

pDrive uses rclone's excellent Proton *libraries* but owns the sync engine, the
state model, and the UX.

## What makes it feel local

Proton Drive offers no webhooks or push channel, so every third-party client
polls. Polling means a stale window — the thing that makes cloud folders on
Linux feel like cloud folders.

pDrive removes it by intercepting the directory open. An optional root helper
(`pdrive-gate`) holds `opendir(2)` via `fanotify` `FAN_OPEN_PERM` for a few
hundred milliseconds while the daemon reconciles that directory, then releases
it. The listing you get is the listing *after* the sync.

Measured on Linux 7.1.9:

| | |
|---|---|
| `ls` when the folder is already current | **2 ms** |
| `ls` when it has to refresh first | **305 ms** |
| Overhead of the gate with nothing to do | **+71 µs** |
| Gate killed mid-listing | `ls` returns immediately, exit 0 |
| File dropped in the folder | uploaded ~4 s later, no command needed |

The hold never waits on file *content*, only on directory metadata, so a 5 GB
video's name appears at once and the bytes stream in behind it.

The gate is optional. Without it, pDrive falls back to inotify hints plus
adaptive polling and still works, just with a stale window on the first
listing.

## Install

```bash
git clone https://github.com/YourDoritos/pDrive.git
cd pDrive

make install                          # ~/.local/bin + user systemd unit
systemctl --user daemon-reload
systemctl --user enable --now pdrived
loginctl enable-linger $USER          # keep syncing when logged out

pdrive                                # log in, then the TUI takes over
```

Optionally, for listings that wait until they are current:

```bash
sudo make install-gate
sudo systemctl enable --now pdrive-gate
```

Nothing but `pdrive-gate` needs root.

## Use

```
pdrive                    terminal UI: status, activity, conflicts, settings
pdrivectl status --short  one line, for waybar or tmux
pdrivectl sync            run a pass now and wait for it
pdrivectl pause / resume  stop and restart automatic syncing
pdrivectl conflicts       list preserved local copies
pdrivectl get <path>      download a file left as a stub by the size cap
pdrive backup             separate verified archive of the whole account
```

### Keys

`1`–`4` switch tabs, `←`/`→` cycle them, `esc` returns to Status. `s` syncs
now, `p` pauses, `r` refreshes, `q` quits. If the daemon is not running,
Status offers `s` to start it and `e` to start it and enable it at login —
no need to leave the TUI.

## Safety

A sync client's real job is not moving files, it is not losing them.

- **Nothing is ever permanently deleted remotely.** Local deletions become
  Proton trash entries.
- **Nothing is ever unlinked locally.** Remote deletions move the file to
  `~/.local/share/pdrive/trash/`.
- **A conflict never discards either version.** The remote version keeps the
  original name, yours is preserved beside it, and both are uploaded, so both
  exist on every device.
- **A pass that would delete a large share of your files is refused.**
- **An empty sync folder with a populated database stops the sync** — that is
  far more often an unmounted disk than an intentional wipe.
- **Symlinks are not followed.** A link to `~/.ssh/id_rsa` cannot pull it into
  your account.
- **Names from the server are validated, never trusted.** A file called
  `../../.bashrc` cannot escape the sync folder.

`pdrive backup` makes a separate verified copy of the account with a
`sha1sum(1)`-compatible manifest, so it can be checked without trusting pDrive:

```bash
pdrive backup && cd ~/pdrive-backup-* && sha1sum -c MANIFEST.sha1
```

## Upload verification

Proton's storage backend requires a per-block verification token in
`POST /drive/blocks`. Without it every upload is rejected with *"You are using
an outdated version of the app"*, which has left uploads broken in every Go
client, rclone included, since September 2025.

pDrive implements it, ported from Proton's own MIT-licensed SDK. See
[`third_party/VENDOR.md`](third_party/VENDOR.md). The patch is upstreamable to
rclone.

## Paths

| What | Where |
|---|---|
| Sync folder | `~/pdrive` |
| Config | `~/.config/pdrive/config.toml` |
| Session, state DB, log | `~/.local/state/pdrive/` |
| Local trash | `~/.local/share/pdrive/trash/` |

Most settings are editable in the TUI's Settings tab; the daemon picks them up
without a restart.

## Security

Proton offers no app passwords, service accounts, or scoped tokens, so an
unattended sync daemon necessarily holds full-account credentials. pDrive
stores its session encrypted at rest (Argon2id + XSalsa20-Poly1305, keyed from
`/etc/machine-id`) with `0600` permissions — which protects against the file
being copied elsewhere, not against someone who can already read your home
directory.

`pdrive-gate` runs as root because `fanotify` permission events require
`CAP_SYS_ADMIN`. It is deliberately tiny: no network, no cryptography, no
credentials. It never denies an open, only delays one, and fails open on every
error path.

See [SECURITY.md](SECURITY.md) for the full threat model.

## Development

```bash
make build      # all four binaries into ./bin
make test
make fmtcheck
make lint       # needs golangci-lint

test/two-machine.sh   # two-device convergence test against a live account
```

[`docs/PROJECT_SPEC.md`](docs/PROJECT_SPEC.md) is the design document: the
freshness mechanism and its measurements, the reconciler's rules, the eight
safety guards, and the phase plan.

## Status

Working and in daily use, but pre-1.0 and moving quickly. Bidirectional sync,
conflict handling, move detection, the daemon and the gate are all done and
tested against a live account.

Proton has announced a new Drive cryptographic model for end 2026 / early 2027;
clients implementing only the previous one will stop interoperating until
updated. All cryptography sits behind one package so that change lands in one
place.

## License

GPL-3.0. See [LICENSE](LICENSE).
