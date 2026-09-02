# Security Policy

## Reporting a Vulnerability

If you find a security vulnerability in pDrive — especially anything affecting
credential storage, the Proton Drive cryptographic layer, `pdrive-gate` (which
runs as root), the daemon's IPC surface, or path handling — please report it
privately rather than opening a public issue.

Email: **paul.goessmann@proton.me**

Please include:

- A description of the issue and its impact
- Steps to reproduce (or a proof of concept)
- The pDrive version (`pdrivectl --version`) and your distro
- Whether the issue is already public

You will get an acknowledgement within a few days. Once the issue is
understood and a fix is available, a coordinated disclosure timeline will be
agreed on before any public advisory.

## Supported Versions

Only the latest released version receives security fixes. pDrive is pre-1.0
and moves quickly — please upgrade before reporting.

---

## Threat model

pDrive is a third-party client. It is not affiliated with or endorsed by
Proton AG, and it has no special standing with Proton's services.

### What pDrive holds

**Full account credentials.** Proton offers no app passwords, service
accounts, or scoped tokens, so an unattended sync client necessarily holds
credentials that grant complete access to the account. There is no weaker
credential available to store instead.

Stored in `~/.local/state/pdrive/session.enc`, mode `0600`:

- the Proton session UID and access/refresh tokens
- the derived PGP key passphrase, which is what actually decrypts your files

The file is encrypted with XSalsa20-Poly1305, keyed by Argon2id over
`/etc/machine-id`. **This protects against the file being copied to another
machine. It does not protect against anyone who can already read your home
directory**, because the key material is derivable from data that reader also
has. Treat `session.enc` as equivalent to your Proton password.

If 2FA is enabled and you want the daemon to survive reboots unattended, the
TOTP *secret* must be stored, not just a code. That is a real weakening of
2FA. The alternative is entering a code after each restart.

### What runs as root

`pdrive-gate` only. It exists because `fanotify` permission events require
`CAP_SYS_ADMIN`, and it is deliberately minimal:

- no network access (`RestrictAddressFamilies=AF_UNIX`)
- no cryptography, no credentials, no access to the state database
- it never issues `FAN_DENY`; it can only *delay* an open, never refuse one
- every error path drops all marks, so failure means "behave as if not
  installed"

Its socket is world-writable by necessity — any user's daemon must be able to
register its own folder. Authorisation is therefore by **peer credentials**:
`SO_PEERCRED` yields the caller's uid, and the requested sync root must be
owned by that uid. Without this check, any local user could ask a root process
to intercept opens in another user's directories and stall them.

`pdrive-gate` marks only directories inside the registered root. It never uses
`FAN_MARK_FILESYSTEM`.

Killing `pdrive-gate` is always safe. The kernel releases every outstanding
permission event when the descriptor closes.

### What pDrive can destroy

It can delete files, locally and in your Proton Drive account. Several
deliberate limits:

- **Nothing is ever permanently deleted remotely.** Local deletions become
  Proton *trash* entries.
- **Nothing is ever unlinked locally in response to a remote change.** Remote
  deletions move the local file to `~/.local/share/pdrive/trash/`.
- **A conflict never discards either version.** Both are kept and both are
  uploaded.
- **A pass that would delete a large share of tracked files is refused**
  outright, with a floor so the guard is not trained away by false positives.
- **An empty sync folder with a populated database stops the sync**, because
  that is far more often an unmounted disk than an intentional wipe.

### Handling of untrusted input

Filenames come from the server and are chosen by anyone who can write to the
account, including via a share. They are validated, never trusted: absolute
paths, `..` components, path separators, embedded NULs, and names colliding
with pDrive's own bookkeeping suffixes are all rejected before a name becomes
a local path.

Symlinks inside the sync folder are **not followed**. A link pointing at
`~/.ssh/id_rsa` cannot pull it into your account.

### Vendored cryptographic code

`third_party/` contains vendored copies of `rclone/Proton-API-Bridge` and
`rclone/go-proton-api` (both MIT), with local changes documented in
`third_party/VENDOR.md`. pDrive's own additions there include the per-block
upload verification ported from Proton's published SDK.

Proton has announced a new Drive cryptographic model for end 2026 / early
2027. Clients implementing only the previous model will stop interoperating.

### Not in scope

- An attacker who already has read access to your user account. They can read
  `session.enc`, and everything follows from that.
- The security of Proton Drive itself.
- Malicious content *inside* your files. pDrive transfers bytes; it does not
  inspect them.
