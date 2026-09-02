#!/usr/bin/env bash
#
# Removes pDrive. Your synced files and your Proton Drive account are left
# alone — this only removes the program.
set -uo pipefail

cd "$(dirname "$0")"

echo "Stopping services…"
systemctl --user disable --now pdrived 2>/dev/null || true
if systemctl is-enabled pdrive-gate >/dev/null 2>&1; then
    sudo systemctl disable --now pdrive-gate 2>/dev/null || true
fi

echo "Removing binaries and units…"
make uninstall
if [ -f /usr/local/bin/pdrive-gate ]; then
    sudo make uninstall-gate
fi

systemctl --user daemon-reload 2>/dev/null || true

cat <<'MSG'

pDrive removed.

Left in place on purpose:
  ~/pdrive                       your synced files
  ~/.config/pdrive/              settings
  ~/.local/state/pdrive/         session and sync state
  ~/.local/share/pdrive/trash/   recoverable deletions

Delete those yourself if you want them gone. Your Proton Drive account is
untouched.
MSG
