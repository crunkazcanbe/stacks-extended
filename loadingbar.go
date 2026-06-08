package main

// loadingbar.go — the TERMINAL loading UI for stacks commands (NOT the menu):
//   * banner()      — the STACKS ascii word-logo splash + version (top of every run)
//   * progressBar() — the little [████░░░] loading bar shown during up/fix/etc.
//   * stacksVersion() — commit-count·short-sha from the git clone (shown bottom-left)
// This is the one place to tweak how the loading art/bar looks.

import (
	"fmt"
	"os/exec"
	"strings"
)

// stacksVersion = "v<commitcount>·<shortsha>" from the git clone, else "dev".
// repoDir() is resolved universally (env / conf / binary's own dir) — no hardcoded path.
func stacksVersion() string {
	repo := repoDir()
	if repo == "" {
		return "dev"
	}
	sha := gitOut(repo, "rev-parse", "--short", "HEAD")
	if sha == "" {
		return "dev"
	}
	cnt := gitOut(repo, "rev-list", "--count", "HEAD")
	if cnt == "" {
		cnt = "0"
	}
	return "v" + cnt + "·" + sha
}

func gitOut(repo string, args ...string) string {
	full := append([]string{"-C", repo}, args...)
	out, err := exec.Command("git", full...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// banner prints the STACKS word-logo (trans-flag colors) + version, bottom-left.
func banner() {
	art := []string{
		"  ____  _____  _    ____ _  _______ ",
		" / ___||_   _|/ \\  / ___| |/ /  ___|",
		" \\___ \\  | | / _ \\| |   | ' /|___ \\",
		"  ___) | | |/ ___ \\ |___| . \\ ___) |",
		" |____/  |_/_/   \\_\\____|_|\\_\\____/ ",
	}
	cols := []int{117, 218, 231, 218, 117} // blue pink white pink blue
	for i, line := range art {
		fmt.Printf("\x1b[38;5;%dm%s\x1b[0m\n", cols[i], line)
	}
	fmt.Printf("\x1b[38;5;245m %s\x1b[0m\n", stacksVersion())
}

// progressBar draws the in-place loading bar: [██████░░░░] 60% <label>.
func progressBar(label string, pct int) {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	const w = 30
	filled := w * pct / 100
	bar := strings.Repeat("█", filled) + strings.Repeat("░", w-filled)
	fmt.Printf("\r\x1b[36m[%s]\x1b[0m %3d%%  %s\x1b[K", bar, pct, label)
}
