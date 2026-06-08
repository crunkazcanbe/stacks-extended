package main

// ui.go — faithful Go port of stacks_ui.py.
// Shared UI helpers for the loading bar / log display:
//   stripNoise(line) : clean a log line of ANSI/control chars
//   isNoise(line)    : true if the line should be skipped
//   cleanLogLine(raw): strip + filter, returning "" for noise
//   stacksArt        : the ASCII banner as a string

import (
	"regexp"
	"strings"
)

// stacksArt is STACKS_ART (single backslashes, exactly as Python renders it).
const stacksArt = `
  ____  _____  _    ____ _  _______
 / ___||_   _|/ \  / ___| |/ /  ___|
 \___ \  | | / _ \| |   | ' /|___ \
  ___) | | |/ ___ \ |___| . \ ___) |
 |____/  |_/_/   \_\____|_|\_\____/
`

// _NOISE: lines that are definitely noise — never shown in a log display.
var reNoise = regexp.MustCompile(
	`[\x1b\x00-\x1f\x7f]` + // control/escape chars
		`|[░█]{2,}` + // block chars from loading bars
		`|\[[\s#>\-=]{3,}` + // old loading bar brackets
		`|Press Ctrl` + // cancel hints
		`|=== ` + // sequence markers
		`|SEQUENCE` +
		`|____` + // ASCII art fragments
		`|\\___` +
		`|/ ___` +
		`|\|____`)

// _ART: art-specific lines to skip.
var reArt = regexp.MustCompile(`^[\s_/\\|.=\[\](){}#*\-]+$`)

// reStripEsc / reStripCtl mirror the two substitutions in strip_noise().
var reStripEsc = regexp.MustCompile(`\x1b[^a-zA-Z]*[a-zA-Z]`)
var reStripCtl = regexp.MustCompile(`[\x00-\x1f\x7f]`)

// rePct mirrors the pure percentage/progress check.
var rePct = regexp.MustCompile(`^[\d\s%]+$`)

// stripNoise strips ANSI codes and control characters from a log line.
func stripNoise(line string) string {
	line = reStripEsc.ReplaceAllString(line, "")
	line = reStripCtl.ReplaceAllString(line, "")
	return strings.TrimSpace(line)
}

// isNoise reports whether this line should NOT be shown in a log display.
func isNoise(line string) bool {
	if line == "" || len([]rune(line)) < 3 {
		return true
	}
	if reNoise.MatchString(line) {
		return true
	}
	if reArt.MatchString(line) {
		return true
	}
	if rePct.MatchString(line) { // pure percentage/progress lines
		return true
	}
	return false
}

// cleanLogLine strips and filters a raw log line; returns "" for noise.
func cleanLogLine(raw string) string {
	line := stripNoise(raw)
	if isNoise(line) {
		return ""
	}
	return line
}
