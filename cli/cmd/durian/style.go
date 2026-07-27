package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/fatih/color"
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
