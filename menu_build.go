package main

// menu_build.go — Build tab: scaffold a new service into a stack (image/service/
// stack prompts → `stacks build …`), regenerate dynamics, generate the sablier
// groups file, and run fix/repair across all stacks. Mirrors BUILD_ITEMS.
//
// The original curses Build wizard was a multi-step interactive popup; the TUI
// runs the same non-interactive `stacks build <image> <service> <stack>` engine
// behind three input prompts (and exposes the generator/fix helpers it shared).

import (
	tea "github.com/charmbracelet/bubbletea"
)

var tuiBuildItems = []tuiAction{
	{"Build a service into a stack…", "build_into"},
	{"Generate dynamics from ALL stacks", "gen_dyn_all"},
	{"Force regen ALL dynamics", "gen_dyn_force"},
	{"Generate sablier groups config", "gen_groups"},
	{"Run stacks fix on ALL", "fix_all"},
	{"Run stacks repair on ALL", "repair_all"},
}

func (m menuModel) renderBuild() string {
	return tuiRenderActionList("BUILD", tuiBuildItems, m.sel, m.scroll, m.visibleRows(), m.width)
}

func (m menuModel) handleBuildKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "up", "k", "down", "j", "pgup", "pgdown", "home", "end":
		m.moveCursor(k, len(tuiBuildItems))
	case "enter":
		if m.sel < 0 || m.sel >= len(tuiBuildItems) {
			return m, nil
		}
		return m.doBuildAction(tuiBuildItems[m.sel].Value)
	}
	return m, nil
}

func (m menuModel) doBuildAction(action string) (menuModel, tea.Cmd) {
	switch action {
	case "build_into":
		// Chain three input prompts: image -> service -> stack, then run build.
		m.popup = tuiInputPopup("Build — image", "Docker image (e.g. nginx:latest):", "",
			func(image string) (menuModel, tea.Cmd) {
				if image == "" {
					return m, nil
				}
				m.popup = tuiInputPopup("Build — service", "Service / container name:", "",
					func(svc string) (menuModel, tea.Cmd) {
						if svc == "" || !tuiValidName(svc) {
							return m, nil
						}
						m.popup = tuiInputPopup("Build — stack", "Target stack name:", "",
							func(stack string) (menuModel, tea.Cmd) {
								if stack == "" || !tuiValidName(stack) {
									return m, nil
								}
								return m, tuiSelfCmd("Build "+svc+" → "+stack, "build", image, svc, stack)
							})
						return m, nil
					})
				return m, nil
			})
		return m, nil
	case "gen_dyn_all":
		return m, tuiSelfCmd("Gen ALL dynamics", "dynamics", "generate", "all")
	case "gen_dyn_force":
		return m, tuiSelfCmd("Force regen ALL", "dynamics", "generate", "all", "force")
	case "gen_groups":
		return m, tuiSelfCmd("Gen sablier groups", "__gensrvs")
	case "fix_all":
		return m, tuiSelfCmd("Fix ALL", "fix", "all")
	case "repair_all":
		return m, tuiSelfCmd("Repair ALL", "fix", "all", "repair")
	}
	return m, nil
}
