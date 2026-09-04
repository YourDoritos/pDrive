package main

import (
	"fmt"
	"strings"
)

// Terminal window metrics, in SVG user units.
const (
	cellW   = 8.4
	cellH   = 18.0
	padX    = 22.0
	padTop  = 40.0 // room for the title bar
	padBot  = 16.0
	radius  = 10.0
	fontPx  = 14.0
	titleH  = 28.0
	bgColor = "#14141f"
	fgColor = "#e8e8e8"
)

// trimBlankEdges drops fully blank rows from the top and bottom.
//
// Screens are centred inside the terminal height, so rendering one at a
// generous size leaves large empty margins. Trimming lets each screenshot fit
// its own content without hand-tuning a height per screen.
func trimBlankEdges(grid [][]cell) [][]cell {
	blank := func(line []cell) bool {
		return len(trimTrailing(line)) == 0
	}
	start := 0
	for start < len(grid) && blank(grid[start]) {
		start++
	}
	end := len(grid)
	for end > start && blank(grid[end-1]) {
		end--
	}
	grid = grid[start:end]

	// Screens are also centred vertically, which leaves a run of blank rows
	// between the tab bar and the panel. Collapse any run to one. Rows inside
	// a panel carry its border characters, so they are never blank and are
	// never touched.
	out := make([][]cell, 0, len(grid))
	prevBlank := false
	for _, line := range grid {
		isBlank := blank(line)
		if isBlank && prevBlank {
			continue
		}
		out = append(out, line)
		prevBlank = isBlank
	}
	return out
}

// renderSVG draws a terminal window containing the given screen.
func renderSVG(title string, grid [][]cell) string {
	grid = trimBlankEdges(grid)
	// One blank row top and bottom, so the content is not flush to the frame.
	grid = append([][]cell{nil}, append(grid, nil)...)

	cols := 0
	for _, line := range grid {
		if n := len(line); n > cols {
			cols = n
		}
	}
	if cols < 40 {
		cols = 40
	}

	w := float64(cols)*cellW + 2*padX
	h := float64(len(grid))*cellH + padTop + padBot

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" `+
		`viewBox="0 0 %.0f %.0f" font-family="JetBrainsMono Nerd Font, JetBrains Mono, monospace" `+
		"font-size=\"%.1f\">\n", w, h, w, h, fontPx)

	// Window frame.
	fmt.Fprintf(&b, `<rect x="0" y="0" width="%.0f" height="%.0f" rx="%.0f" fill="%s"/>`+"\n",
		w, h, radius, bgColor)
	fmt.Fprintf(&b, `<rect x="0" y="0" width="%.0f" height="%.0f" rx="%.0f" fill="#1d1d2b"/>`+"\n",
		w, titleH, radius)
	fmt.Fprintf(&b, `<rect x="0" y="%.0f" width="%.0f" height="%.0f" fill="#1d1d2b"/>`+"\n",
		radius, w, titleH-radius)
	for i, col := range []string{"#ff5f57", "#febc2e", "#28c840"} {
		fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="5" fill="%s"/>`+"\n",
			18+float64(i)*18, titleH/2, col)
	}
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="#8a8a9a" font-size="12" `+
		`text-anchor="middle">%s</text>`+"\n", w/2, titleH/2+4, escape(title))

	// Background runs first, so glyphs are never clipped by a later cell.
	for row, line := range grid {
		y := padTop + float64(row)*cellH
		for col, c := range line {
			if c.bg == "" {
				continue
			}
			fmt.Fprintf(&b, `<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" fill="%s"/>`+"\n",
				padX+float64(col)*cellW, y-cellH+5, cellW+0.5, cellH, c.bg)
		}
	}

	// Then glyphs, batching consecutive cells that share a style into one
	// text element — an SVG with one element per character is enormous.
	for row, line := range grid {
		line = trimTrailing(line)
		if len(line) == 0 {
			continue
		}
		y := padTop + float64(row)*cellH

		start := 0
		for i := 1; i <= len(line); i++ {
			if i < len(line) && sameStyle(line[i], line[start]) {
				continue
			}
			run := line[start:i]
			text := strings.TrimRight(string(runes(run)), " ")
			if text != "" {
				fmt.Fprintf(&b, `<text x="%.2f" y="%.2f" fill="%s"%s xml:space="preserve">%s</text>`+"\n",
					padX+float64(start)*cellW, y, colorOf(run[0]), weightOf(run[0]), escape(text))
			}
			start = i
		}
	}

	b.WriteString("</svg>\n")
	return b.String()
}

func sameStyle(a, b cell) bool {
	return a.fg == b.fg && a.bg == b.bg && a.bold == b.bold
}

func runes(cs []cell) []rune {
	out := make([]rune, len(cs))
	for i, c := range cs {
		out[i] = c.r
	}
	return out
}

func colorOf(c cell) string {
	if c.fg == "" {
		return fgColor
	}
	return c.fg
}

func weightOf(c cell) string {
	if c.bold {
		return ` font-weight="bold"`
	}
	return ""
}

func escape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
