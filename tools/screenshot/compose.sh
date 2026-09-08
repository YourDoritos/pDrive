#!/usr/bin/env bash
#
# Turns the SVGs written by `go run ./tools/screenshot` into the PNGs the
# README uses, including the 2x2 grid.
#
# Kept as a script rather than done by hand so the images can be regenerated
# after a UI change instead of slowly going stale.
set -euo pipefail

DIR=${1:-assets}
BG='#0d0d14'
ZOOM=2

need() { command -v "$1" >/dev/null || { echo "need $1" >&2; exit 1; }; }
need magick

# Rasteriser. Chromium is preferred because librsvg renders colour emoji as
# flat white silhouettes — pVPN's country flags come out as glyph-shaped
# blocks under it. pDrive's screens have no emoji, so rsvg-convert is a fine
# fallback here and the two projects still share this script.
RASTERISER=""
if command -v chromium >/dev/null; then
    RASTERISER=chromium
elif command -v google-chrome-stable >/dev/null; then
    RASTERISER=google-chrome-stable
elif command -v rsvg-convert >/dev/null; then
    RASTERISER=rsvg-convert
else
    echo "need chromium or rsvg-convert" >&2
    exit 1
fi

CHROME_PROFILE=""
cleanup() { [ -n "$CHROME_PROFILE" ] && rm -rf "$CHROME_PROFILE"; }
trap cleanup EXIT

# rasterise SVG -> PNG at ZOOM, honouring the SVG's own width/height.
rasterise() {
    local svg=$1 png=$2 w h
    if [ "$RASTERISER" = "rsvg-convert" ]; then
        rsvg-convert -z "$ZOOM" "$svg" -o "$png"
        return
    fi
    read -r w h < <(sed -n '1s/.*width="\([0-9]*\)" height="\([0-9]*\)".*/\1 \2/p' "$svg")
    if [ -z "${w:-}" ] || [ -z "${h:-}" ]; then
        echo "could not read the size of $svg" >&2
        exit 1
    fi
    # A throwaway profile: never touch the user's real browser data.
    [ -n "$CHROME_PROFILE" ] || CHROME_PROFILE=$(mktemp -d)
    "$RASTERISER" --headless --disable-gpu --no-sandbox --hide-scrollbars \
        --user-data-dir="$CHROME_PROFILE" \
        --default-background-color=00000000 \
        --force-device-scale-factor="$ZOOM" \
        --window-size="$w,$h" --screenshot="$png" "$svg" >/dev/null 2>&1
}

cd "$DIR"

for f in *.svg; do
    rasterise "$f" "${f%.svg}.png"
done

# Caption font. Rather than probing the font list — whose output shape and
# exit status vary between ImageMagick builds and grep implementations — just
# try the font and fall back if the build does not have it.
CAPTION_FONT=${CAPTION_FONT:-JetBrainsMono-NF-Regular}

# The screenshot tool draws every screen in one fixed frame, so all four
# sources are already the same size and no per-cell padding is needed here.
# Check rather than assume: a mismatch would show up as a ragged grid.
if [ "$(magick identify -format '%wx%h\n' \
        tui-status.png tui-activity.png tui-conflicts.png tui-settings.png \
        | sort -u | wc -l)" != "1" ]; then
    echo "panels are not all the same size — check frameRows in tools/screenshot/svg.go" >&2
    magick identify -format '  %f %wx%h\n' tui-*.png >&2
    exit 1
fi

cell() {
    local src=$1 caption=$2 out=$3
    local common=(
        "$src.png" -resize 880x -background "$BG"
        -bordercolor "$BG" -border 18
        -gravity south -background "$BG" -splice 0x54
        -pointsize 21 -fill '#b9b9c8'
    )
    magick "${common[@]}" -font "$CAPTION_FONT" -annotate +0+16 "$caption" "$out" 2>/dev/null \
        || magick "${common[@]}" -annotate +0+16 "$caption" "$out"
}

cell tui-status    "Status — account, storage and freshness"          c1.png
cell tui-activity  "Activity — live transfers over a durable history" c2.png
cell tui-conflicts "Conflicts — both versions always kept"            c3.png
cell tui-settings  "Settings — every option, no config file"          c4.png

magick montage c1.png c2.png c3.png c4.png \
    -tile 2x2 -geometry +12+12 -background "$BG" tui-grid.png
magick tui-grid.png -bordercolor "$BG" -border 12 tui-grid.png
rm -f c1.png c2.png c3.png c4.png

echo "wrote $(pwd)/tui-grid.png ($(magick identify -format '%wx%h' tui-grid.png)) via $RASTERISER"
