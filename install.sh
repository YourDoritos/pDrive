#!/usr/bin/env bash
#
# Installs pDrive for the current user.
#
# The daemon runs as you, so nothing here needs root. pdrive-gate is the one
# component that does, and it is optional — pass --with-gate to install it too.
set -euo pipefail

WITH_GATE=0
[ "${1:-}" = "--with-gate" ] && WITH_GATE=1

if ! command -v go >/dev/null 2>&1; then
    echo "Go is required to build pDrive: https://go.dev/dl/" >&2
    exit 1
fi

cd "$(dirname "$0")"

echo "Building…"
make build

echo "Installing to ~/.local/bin…"
make install

if [ "$WITH_GATE" = "1" ]; then
    echo
    echo "Installing pdrive-gate (needs root)…"
    sudo make install-gate
    sudo systemctl daemon-reload
    sudo systemctl enable --now pdrive-gate
fi

cat <<'MSG'

Installed.

  1. Log in:            pdrive
  2. Start the daemon:  systemctl --user daemon-reload
                        systemctl --user enable --now pdrived
  3. Keep it running
     when logged out:   loginctl enable-linger "$USER"

Your files sync into ~/pdrive. Check on it with `pdrive` or `pdrivectl status`.
MSG

if [ "$WITH_GATE" = "0" ]; then
    echo "For directory listings that wait until they are current:"
    echo "    ./install.sh --with-gate"
fi
