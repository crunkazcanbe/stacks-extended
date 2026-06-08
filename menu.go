package main

// menu.go — the interactive TUI menu (the Go home for what stacks_menu.py does).
// The engine commands are being ported first; the full menu lands here later.
// Keeping it in its own file so the menu work never tangles with the engine.

import (
	"fmt"
	"os"
)

// cmdMenu launches the interactive Bubble Tea TUI (the Go home for the work that
// stacks_menu.py used to do). The model + tabs live in the menu_*.go siblings.
func cmdMenu(args []string) {
	if err := menuRun(); err != nil {
		fmt.Fprintln(os.Stderr, "menu:", err)
		os.Exit(1)
	}
}
