#!/usr/bin/env bash
#
# pDrive uninstaller — removes the binaries, the user unit, and (if it is
# installed) the pdrive-gate system service. Handles both installs done via
# install.sh and via `make install`; both land in ~/.local/bin.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/YourDoritos/pDrive/main/uninstall.sh | bash
#
# Delete your settings, sync state and local trash as well:
#   curl -fsSL .../uninstall.sh | bash -s -- --purge
#
# --purge never touches ~/pdrive itself, and never touches your Proton Drive
# account. Your files are yours; delete the folder by hand if you want it gone.
#
# The script is idempotent: running it twice, or on a machine where pDrive
# was never installed, exits 0 with a "nothing to remove" summary.

set -uo pipefail

BINDIR="${PDRIVE_BINDIR:-$HOME/.local/bin}"
USERUNITDIR="${PDRIVE_UNITDIR:-$HOME/.config/systemd/user}"

GATE_BINDIR="/usr/local/bin"
SYSUNITDIR="/etc/systemd/system"

PURGE=0

log()  { printf '\033[1;34m::\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m!!\033[0m %s\n' "$*" >&2; exit 1; }

for arg in "$@"; do
    case "$arg" in
        --purge) PURGE=1 ;;
        -h|--help)
            cat <<'USAGE'
pDrive uninstaller.

  uninstall.sh            remove the program, keep your files and settings
  uninstall.sh --purge    also delete settings, sync state and local trash

~/pdrive and your Proton Drive account are never touched.
USAGE
            exit 0 ;;
        *) die "unknown flag: $arg (see --help)" ;;
    esac
done

# Refuse if pDrive came from the AUR — let pacman handle removal.
if command -v pacman >/dev/null 2>&1 && pacman -Qi pdrive >/dev/null 2>&1; then
    die "pDrive was installed via the AUR. Uninstall with: sudo pacman -Rns pdrive"
fi

if [ "${EUID:-$(id -u)}" -eq 0 ] && [ -z "${PDRIVE_ALLOW_ROOT:-}" ]; then
    die "Run this as your normal user, not root — pDrive is installed per-user.
   It asks for sudo itself if pdrive-gate needs removing."
fi

removed_anything=0

# --- stop & disable services -------------------------------------------------

# Whether a unit is installed is decided by the unit file being on disk, not
# by parsing `systemctl list-unit-files`: that command's output and exit
# status vary between systemd versions, and getting it wrong here means
# asking for sudo, and stopping a service, on a machine that never had one.
GATE_INSTALLED=0
[ -e "${GATE_BINDIR}/pdrive-gate" ] && GATE_INSTALLED=1
[ -e "${SYSUNITDIR}/pdrive-gate.service" ] && GATE_INSTALLED=1

if command -v systemctl >/dev/null 2>&1; then
    if [ -e "${USERUNITDIR}/pdrived.service" ]; then
        log "Stopping and disabling pdrived…"
        systemctl --user disable --now pdrived 2>/dev/null || true
        removed_anything=1
    fi
    if [ "$GATE_INSTALLED" = "1" ]; then
        log "Stopping and disabling pdrive-gate (needs root)…"
        sudo systemctl disable --now pdrive-gate 2>/dev/null || true
    fi
fi

# --- remove files ------------------------------------------------------------

for bin in pdrive pdrived pdrivectl; do
    if [ -e "${BINDIR}/${bin}" ]; then
        log "Removing ${BINDIR}/${bin}"
        rm -f "${BINDIR}/${bin}"
        removed_anything=1
    fi
done

if [ -e "${USERUNITDIR}/pdrived.service" ]; then
    log "Removing ${USERUNITDIR}/pdrived.service"
    rm -f "${USERUNITDIR}/pdrived.service"
    removed_anything=1
fi

if [ "$GATE_INSTALLED" = "1" ]; then
    log "Removing pdrive-gate (needs root)…"
    sudo rm -f "${GATE_BINDIR}/pdrive-gate" "${SYSUNITDIR}/pdrive-gate.service"
    sudo systemctl daemon-reload 2>/dev/null || true
    removed_anything=1
fi

systemctl --user daemon-reload 2>/dev/null || true

# --- purge user data ---------------------------------------------------------

if [ "$PURGE" -eq 1 ]; then
    log "Purging settings, sync state and local trash…"
    rm -rf "${XDG_CONFIG_HOME:-$HOME/.config}/pdrive" \
           "${XDG_STATE_HOME:-$HOME/.local/state}/pdrive" \
           "${XDG_DATA_HOME:-$HOME/.local/share}/pdrive" \
           "${XDG_CACHE_HOME:-$HOME/.cache}/pdrive"
    warn "Your synced files in ~/pdrive were kept — delete them yourself if you want them gone."
fi

# --- summary -----------------------------------------------------------------

echo ""
if [ $removed_anything -eq 0 ] && [ $PURGE -eq 0 ]; then
    log "Nothing to remove — pDrive does not appear to be installed."
    exit 0
fi

printf '\033[1;32m✓\033[0m pDrive removed.\n\n'
if [ $PURGE -eq 0 ]; then
    cat <<MSG
Left in place on purpose:
  ~/pdrive                       your synced files
  ~/.config/pdrive/              settings
  ~/.local/state/pdrive/         session and sync state
  ~/.local/share/pdrive/trash/   recoverable deletions

To wipe everything but the synced files, re-run with --purge:

  curl -fsSL https://raw.githubusercontent.com/YourDoritos/pDrive/main/uninstall.sh \\
    | bash -s -- --purge

Your Proton Drive account is untouched.
MSG
fi
