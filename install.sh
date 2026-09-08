#!/usr/bin/env bash
#
# pDrive installer — fetches prebuilt binaries from a GitHub release and
# installs the pdrived user service.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/YourDoritos/pDrive/main/install.sh | bash
#
# Pin a specific version:
#   PDRIVE_VERSION=v0.1.0 curl ... | bash
#
# Also install the optional listing gate (the one component needing root):
#   curl -fsSL ... | bash -s -- --with-gate
#
# Build from a clone instead of downloading:
#   ./install.sh --from-source
#
# Unlike pVPN's installer this one does NOT want root: the daemon runs as
# you, into ~/.local/bin. Only --with-gate escalates, and only for the gate.

set -euo pipefail

REPO="YourDoritos/pDrive"
BINDIR="${PDRIVE_BINDIR:-$HOME/.local/bin}"
USERUNITDIR="${PDRIVE_UNITDIR:-$HOME/.config/systemd/user}"

GATE_BINDIR="/usr/local/bin"
SYSUNITDIR="/etc/systemd/system"

WITH_GATE=0
FROM_SOURCE=0

log()  { printf '\033[1;34m::\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m!!\033[0m %s\n' "$*" >&2; exit 1; }

for arg in "$@"; do
    case "$arg" in
        --with-gate)   WITH_GATE=1 ;;
        --from-source) FROM_SOURCE=1 ;;
        -h|--help)
            # Spelled out rather than read back from $0: piped through curl
            # there is no $0 to read.
            cat <<'USAGE'
pDrive installer.

  install.sh                 install the latest release into ~/.local/bin
  install.sh --with-gate     also install pdrive-gate (asks for sudo)
  install.sh --from-source   build from this checkout instead of downloading

  PDRIVE_VERSION=v0.1.0      pin a release
  PDRIVE_BINDIR=...          install somewhere other than ~/.local/bin
USAGE
            exit 0 ;;
        *) die "unknown flag: $arg (see --help)" ;;
    esac
done

# --- preflight ---------------------------------------------------------------

[ "$(uname -s)" = "Linux" ] || die "pDrive only supports Linux (detected: $(uname -s))."

# The daemon, the TUI and the CLI all run as you and write into your home.
# Installing them as root would put the unit and the sync folder in /root.
if [ "${EUID:-$(id -u)}" -eq 0 ] && [ -z "${PDRIVE_ALLOW_ROOT:-}" ]; then
    die "Run this as your normal user, not root — pDrive installs per-user.
   Only --with-gate needs privilege, and it asks for sudo itself."
fi

command -v systemctl >/dev/null 2>&1 || die "systemd (systemctl) not found. pDrive requires systemd."

# --- source install ----------------------------------------------------------

if [ "$FROM_SOURCE" = "1" ]; then
    command -v go >/dev/null 2>&1 || die "Go is required to build pDrive: https://go.dev/dl/"
    cd "$(dirname "$0")"
    [ -f go.mod ] || die "--from-source must be run from a pDrive checkout."

    log "Building…"
    make build
    log "Installing to ${BINDIR}…"
    # Pass the prefix through so PDRIVE_BINDIR means the same thing on both
    # install paths, instead of the Makefile's default silently winning.
    make install BINDIR="$BINDIR" USERUNITDIR="$USERUNITDIR"

    if [ "$WITH_GATE" = "1" ]; then
        log "Installing pdrive-gate (needs root)…"
        sudo make install-gate
        sudo systemctl daemon-reload
        sudo systemctl enable --now pdrive-gate
    fi
else

# --- resolve version ---------------------------------------------------------

command -v curl       >/dev/null 2>&1 || die "curl is required but not installed."
command -v sha256sum  >/dev/null 2>&1 || die "sha256sum is required but not installed (coreutils)."

# Map host arch to the Go/release naming used in asset filenames
# (pdrive-linux-amd64, pdrive-linux-arm64).
HOST_ARCH=$(uname -m)
case "$HOST_ARCH" in
    x86_64|amd64)  GOARCH="amd64" ;;
    aarch64|arm64) GOARCH="arm64" ;;
    *) die "Unsupported architecture '$HOST_ARCH'. Published binaries: amd64, arm64. Build from source: https://github.com/${REPO}" ;;
esac

VERSION="${PDRIVE_VERSION:-}"
if [ -z "$VERSION" ]; then
    log "Looking up latest release…"
    # `|| true`: without it `set -e` plus `pipefail` kills the script on
    # curl's exit status and the user gets a bare "curl: (22)" instead of
    # the explanation below — which is the message that actually matters
    # before the first release is published.
    VERSION=$(curl -fsL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
        | grep -oE '"tag_name":[[:space:]]*"[^"]+"' \
        | sed -E 's/.*"tag_name":[[:space:]]*"([^"]+)"/\1/' \
        | head -n1 || true)
    [ -n "$VERSION" ] || die "No published release found for ${REPO}.
   Pin one with PDRIVE_VERSION=vX.Y.Z, or build from a clone:
       git clone https://github.com/${REPO}.git && cd pDrive && ./install.sh --from-source"
fi

log "Installing pDrive ${VERSION} for linux/${GOARCH}"

BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"
RAW_URL="https://raw.githubusercontent.com/${REPO}/${VERSION}"

# --- download & verify -------------------------------------------------------

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
cd "$TMP"

BINS=(pdrive pdrived pdrivectl)
[ "$WITH_GATE" = "1" ] && BINS+=(pdrive-gate)

log "Downloading binaries…"
for bin in "${BINS[@]}"; do
    asset="${bin}-linux-${GOARCH}"
    curl -fsSL -o "$bin" "${BASE_URL}/${asset}" \
        || die "Failed to fetch ${asset} from ${BASE_URL}"
done

log "Downloading checksums…"
if ! curl -fsSL -o SHA256SUMS "${BASE_URL}/SHA256SUMS"; then
    warn "SHA256SUMS not published for ${VERSION} — skipping checksum verification."
else
    log "Verifying checksums…"
    # SHA256SUMS lists files by their release-asset name (e.g.
    # pdrive-linux-amd64). Rewrite to the local filenames we just saved so
    # sha256sum -c finds them. pdrive-gate must be rewritten before pdrive,
    # or the shorter pattern eats the longer name.
    sed -i "s| pdrive-gate-linux-${GOARCH}\$| pdrive-gate|; \
            s| pdrivectl-linux-${GOARCH}\$| pdrivectl|; \
            s| pdrived-linux-${GOARCH}\$| pdrived|; \
            s| pdrive-linux-${GOARCH}\$| pdrive|" SHA256SUMS
    sha256sum -c --ignore-missing SHA256SUMS || die "Checksum verification failed."
fi

log "Downloading systemd units…"
curl -fsSL -o pdrived.service "${RAW_URL}/dist/pdrived.service" \
    || die "Failed to fetch pdrived.service"
if [ "$WITH_GATE" = "1" ]; then
    curl -fsSL -o pdrive-gate.service "${RAW_URL}/dist/pdrive-gate.service" \
        || die "Failed to fetch pdrive-gate.service"
fi

# --- install -----------------------------------------------------------------

log "Installing binaries to ${BINDIR}…"
install -Dm755 pdrive    "${BINDIR}/pdrive"
install -Dm755 pdrived   "${BINDIR}/pdrived"
install -Dm755 pdrivectl "${BINDIR}/pdrivectl"

log "Installing the pdrived user unit…"
sed "s|^ExecStart=.*|ExecStart=${BINDIR}/pdrived|" pdrived.service \
    | install -Dm644 /dev/stdin "${USERUNITDIR}/pdrived.service"

if [ "$WITH_GATE" = "1" ]; then
    log "Installing pdrive-gate (needs root)…"
    sudo install -Dm755 pdrive-gate "${GATE_BINDIR}/pdrive-gate"
    sed "s|^ExecStart=.*|ExecStart=${GATE_BINDIR}/pdrive-gate|" pdrive-gate.service \
        | sudo install -Dm644 /dev/stdin "${SYSUNITDIR}/pdrive-gate.service"
    sudo systemctl daemon-reload
    sudo systemctl enable --now pdrive-gate
fi

fi  # end release install

systemctl --user daemon-reload 2>/dev/null || true

# --- post-install hints ------------------------------------------------------

# pdrived exits with "not logged in" if it starts before `pdrive`, so unlike
# pVPN's installer this one does not enable the service for you.
case ":${PATH}:" in
    *":${BINDIR}:"*) ;;
    *) warn "${BINDIR} is not on your PATH — add it to your shell profile:"
       warn "    export PATH=\"${BINDIR}:\$PATH\"" ;;
esac

printf '\n\033[1;32m✓\033[0m pDrive installed.\n\n'
cat <<MSG
Next steps:
  1. Log in:            pdrive
  2. Start the daemon:  systemctl --user enable --now pdrived
  3. Keep it running
     when logged out:   loginctl enable-linger "\$USER"

Your files sync into ~/pdrive. Check on it with \`pdrive\` or \`pdrivectl status\`.

Binaries:  ${BINDIR}/{pdrive,pdrived,pdrivectl}
Config:    ~/.config/pdrive/config.toml
State:     ~/.local/state/pdrive/
Unit:      ${USERUNITDIR}/pdrived.service
Uninstall:
  curl -fsSL https://raw.githubusercontent.com/${REPO}/main/uninstall.sh | bash
MSG

if [ "$WITH_GATE" = "0" ]; then
    printf '\nFor directory listings that wait until they are current:\n'
    printf '    curl -fsSL https://raw.githubusercontent.com/%s/main/install.sh | bash -s -- --with-gate\n' "$REPO"
fi
