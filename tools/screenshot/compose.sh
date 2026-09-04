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
need rsvg-convert
need magick

cd "$DIR"

for f in *.svg; do
    rsvg-convert -z "$ZOOM" "$f" -o "${f%.svg}.png"
done

# Caption font. Rather than probing the font list — whose output shape and
# exit status vary between ImageMagick builds and grep implementations — just
# try the font and fall back if the build does not have it.
CAPTION_FONT=${CAPTION_FONT:-JetBrainsMono-NF-Regular}

# Cells are equalised per row, not across the whole grid: padding every cell
# to the tallest of all four leaves the shorter row floating in empty space.
cell() {
    local src=$1 caption=$2 height=$3 out=$4
    local common=(
        "$src.png" -resize 880x -background "$BG"
        -gravity north -extent "880x$height"
        -bordercolor "$BG" -border 18
        -gravity south -background "$BG" -splice 0x54
        -pointsize 21 -fill '#b9b9c8'
    )
    magick "${common[@]}" -font "$CAPTION_FONT" -annotate +0+16 "$caption" "$out" 2>/dev/null \
        || magick "${common[@]}" -annotate +0+16 "$caption" "$out"
}

scaled_height() { magick identify -format '%h' "$1.png" | awk '{print int($1*880/1634)}'; }

row_height() {
    local a b
    a=$(scaled_height "$1"); b=$(scaled_height "$2")
    [ "$a" -gt "$b" ] && echo "$a" || echo "$b"
}

R1=$(row_height tui-status tui-activity)
R2=$(row_height tui-conflicts tui-settings)

cell tui-status    "Status — account, storage and freshness"          "$R1" c1.png
cell tui-activity  "Activity — live transfers over a durable history" "$R1" c2.png
cell tui-conflicts "Conflicts — both versions always kept"            "$R2" c3.png
cell tui-settings  "Settings — every option, no config file"          "$R2" c4.png

magick montage c1.png c2.png c3.png c4.png \
    -tile 2x2 -geometry +12+12 -background "$BG" tui-grid.png
magick tui-grid.png -bordercolor "$BG" -border 12 tui-grid.png
rm -f c1.png c2.png c3.png c4.png

echo "wrote $(pwd)/tui-grid.png ($(magick identify -format '%wx%h' tui-grid.png))"
