package main

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/fatih/color"
	"github.com/rivo/uniseg"
)

// Shared terminal styling for durian's runtime output. This is a thin semantic
// wrapper over github.com/fatih/color so commands never reach for raw color
// attributes: use styHeader/styDim/styOK/styWarn/styErr/styAccent for text and
// calSwatch for a calendar's own color. It is meant to grow into the CLI-wide
// style vocabulary (today only the calendar commands use it; help.go colors the
// usage templates separately).
//
// Styling is suppressed whenever --json is set (machine output must stay
// clean); NO_COLOR and non-TTY output are already honored by fatih/color
// itself, so no isatty handling is needed here.

// styleEnabled reports whether ANSI styling should be emitted. It is false for
// --json output; fatih/color independently drops color for NO_COLOR / non-TTY.
func styleEnabled() bool { return !jsonOutput }

// colorize applies c to s, unless styling is disabled (then s is returned
// verbatim).
func colorize(s string, attrs ...color.Attribute) string {
	if !styleEnabled() {
		return s
	}
	return color.New(attrs...).Sprint(s)
}

// styHeader renders a section header (bold), matching help.go's header weight.
func styHeader(s string) string { return colorize(s, color.Bold) }

// styDim renders secondary/auxiliary text (times zones, hints, markers).
func styDim(s string) string { return colorize(s, color.Faint) }

// styOK renders success/affirmative text (green).
func styOK(s string) string { return colorize(s, color.FgGreen) }

// styWarn renders a warning (yellow).
func styWarn(s string) string { return colorize(s, color.FgYellow) }

// styErr renders an error (red).
func styErr(s string) string { return colorize(s, color.FgRed) }

// styAccent highlights a primary value, e.g. an event subject (bold cyan).
func styAccent(s string) string { return colorize(s, color.FgCyan, color.Bold) }

// calSwatch renders a calendar label prefixed with a filled bullet in the
// calendar's own color. hex is a "#RRGGBB" string (as stored in the vdir
// "color" file); when it is empty or unparseable, or styling is disabled, the
// label is returned with a plain bullet (styled output) or bare (plain output).
func calSwatch(hex, label string) string {
	if !styleEnabled() {
		return label
	}
	r, g, b, ok := parseHexColor(hex)
	if !ok {
		return styDim("●") + " " + label
	}
	return color.RGB(r, g, b).Sprint("●") + " " + label
}

// parseHexColor parses a "#RRGGBB" (or "RRGGBB") string into 8-bit components.
func parseHexColor(hex string) (r, g, b int, ok bool) {
	hex = strings.TrimPrefix(strings.TrimSpace(hex), "#")
	if len(hex) != 6 {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(hex, 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff), true
}

// glyph prefixes: plain-text status markers already used across the CLI
// (auth.go, send.go). Kept here so the calendar commands stay consistent.
const (
	glyphOK   = "✓"
	glyphWarn = "⚠"
	glyphErr  = "✗"
)

// okLine formats a success line "✓ <msg>" with the check and message in green.
func okLine(format string, a ...any) string {
	return styOK(glyphOK + " " + fmt.Sprintf(format, a...))
}

// MARK: - Column alignment for colored cells

// ansiSGRPattern matches ANSI SGR (color/style) escape sequences, so padding
// can be computed on the VISIBLE width of a styled cell. text/tabwriter counts
// the escape bytes as width and misaligns colored columns — never feed styled
// cells into a tabwriter; use printColumns instead.
var ansiSGRPattern = regexp.MustCompile("\x1b\\[[0-9;]*m")

// visibleWidth returns the display width (in runes) of s with ANSI SGR
// sequences stripped.
func visibleWidth(s string) int {
	return uniseg.StringWidth(ansiSGRPattern.ReplaceAllString(s, ""))
}

// humanText makes untrusted mail, contact, and calendar text safe to print to
// a terminal. Newlines are retained only for detail bodies; terminal controls
// and bidirectional overrides are rendered visibly instead of interpreted.
func humanText(s string, multiline bool) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' && multiline:
			b.WriteRune(r)
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r) || isBidiControl(r):
			fmt.Fprintf(&b, "⟦U+%04X⟧", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isBidiControl(r rune) bool {
	return r == '\u061c' || r == '\u200e' || r == '\u200f' ||
		(r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069')
}

func truncate(s string, maxWidth int) string {
	s = humanText(s, false)
	if maxWidth <= 0 || visibleWidth(s) <= maxWidth {
		return s
	}
	suffix := "..."
	target := maxWidth - visibleWidth(suffix)
	if target < 0 {
		target = maxWidth
		suffix = ""
	}
	var b strings.Builder
	width := 0
	graphemes := uniseg.NewGraphemes(s)
	for graphemes.Next() {
		cluster := graphemes.Str()
		clusterWidth := uniseg.StringWidth(cluster)
		if width+clusterWidth > target {
			break
		}
		b.WriteString(cluster)
		width += clusterWidth
	}
	return b.String() + suffix
}

// padVisible right-pads s with spaces to visible width w (styled text keeps
// its escapes; only the visible characters count).
func padVisible(s string, w int) string {
	if d := w - visibleWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// printColumns renders rows as columns separated by two spaces, padding each
// cell to the column's maximum VISIBLE width so colored and plain cells align
// identically. Rows with fewer than two cells (section headers, blank
// separator lines) are printed verbatim and excluded from width computation.
// The last cell of a row is never padded, so lines carry no trailing spaces.
func printColumns(w io.Writer, rows [][]string) {
	widths := map[int]int{}
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		for i, cell := range row[:len(row)-1] {
			if vw := visibleWidth(cell); vw > widths[i] {
				widths[i] = vw
			}
		}
	}
	for _, row := range rows {
		if len(row) < 2 {
			fmt.Fprintln(w, strings.Join(row, ""))
			continue
		}
		var b strings.Builder
		for i, cell := range row {
			if i < len(row)-1 {
				b.WriteString(padVisible(cell, widths[i]))
				b.WriteString("  ")
			} else {
				b.WriteString(cell)
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
}
