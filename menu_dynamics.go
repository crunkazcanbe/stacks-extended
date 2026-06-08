package main

// menu_dynamics.go — Dynamics tab: lists the Traefik dynamic config files, ENTER
// shows the file content, "a" opens a per-file action popup (Art inject/strip,
// Repair, Regenerate, Force regen), "g" regenerates ALL. Mirrors draw_dynamics_tab.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type tuiDynRow struct {
	Name   string // basename
	Stack  string // name without extension
	Path   string
	SizeKB int64
}

func tuiDynRows() []tuiDynRow {
	dir := dispDynamicsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var rows []tuiDynRow
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(n, ".yml") && !strings.HasSuffix(n, ".yaml") {
			continue
		}
		p := filepath.Join(dir, n)
		var kb int64
		if st, err := os.Stat(p); err == nil {
			kb = st.Size() / 1024
		}
		stack := strings.TrimSuffix(strings.TrimSuffix(n, ".yml"), ".yaml")
		rows = append(rows, tuiDynRow{Name: n, Stack: stack, Path: p, SizeKB: kb})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

func (m menuModel) renderDynamics() string {
	rows := tuiDynRows()
	var b strings.Builder
	b.WriteString(tuiAccentStyle.Render("  DYNAMIC CONFIGS — ENTER view · a Actions · g Gen ALL"))
	b.WriteString("\n")
	b.WriteString(tuiDimStyle.Render("  " + strings.Repeat("─", maxInt(0, m.width-4))))
	b.WriteString("\n")
	if len(rows) == 0 {
		b.WriteString(tuiDimStyle.Render("  No dynamic configs found. Press g to generate from all stacks."))
		return b.String()
	}
	vis := m.visibleRows()
	end := m.scroll + vis
	if end > len(rows) {
		end = len(rows)
	}
	for i := m.scroll; i < end; i++ {
		r := rows[i]
		line := fmt.Sprintf("%-44s %6s", truncate(r.Name, 44), fmt.Sprintf("%dK", r.SizeKB))
		if i == m.sel {
			b.WriteString(tuiSelectedStyle.Render(truncate("  ▶ "+line, m.width-2)))
		} else {
			b.WriteString(tuiNormalStyle.Render(truncate("    "+line, m.width-2)))
		}
		b.WriteString("\n")
	}
	return b.String()
}

var tuiDynActions = []tuiAction{
	{"🎨  Art Inject", "art_inject"},
	{"🧹  Art Strip", "art_strip"},
	{"🔧  Repair", "repair"},
	{"⚙  Regenerate", "gen"},
	{"⚙  Force Regen", "gen_force"},
	{"✕  Cancel", ""},
}

func (m menuModel) handleDynamicsKey(k string) (tea.Model, tea.Cmd) {
	rows := tuiDynRows()
	if m.sel >= len(rows) {
		m.sel = maxInt(0, len(rows)-1)
	}
	switch k {
	case "up", "k", "down", "j", "pgup", "pgdown", "home", "end":
		m.moveCursor(k, len(rows))
	case "enter":
		if len(rows) == 0 {
			return m, nil
		}
		r := rows[m.sel]
		return m, tuiDockerCmd("Dynamic: "+r.Name, func() string { return tuiTailFile(r.Path, 400) })
	case "a", "A":
		if len(rows) == 0 {
			return m, nil
		}
		r := rows[m.sel]
		m.popup = tuiActionPopup("Dynamic: "+truncate(r.Stack, 22), tuiDynActions,
			func(action string) (menuModel, tea.Cmd) { return m.doDynamicsAction(r, action) })
	case "g", "G":
		return m, tuiSelfCmd("Gen ALL dynamics", "dynamics", "generate", "all")
	case "f", "F":
		return m, tuiSelfCmd("Force regen ALL", "dynamics", "generate", "all", "force")
	}
	return m, nil
}

func (m menuModel) doDynamicsAction(r tuiDynRow, action string) (menuModel, tea.Cmd) {
	switch action {
	case "", "cancel":
		return m, nil
	case "art_inject":
		return m, tuiSelfCmd("Art inject "+r.Stack, "art", "dynamic", "inject", r.Path)
	case "art_strip":
		return m, tuiSelfCmd("Art strip "+r.Stack, "art", "dynamic", "strip", r.Path)
	case "repair":
		return m, tuiSelfCmd("Repair "+r.Stack, "dynamics", "repair", r.Stack)
	case "gen":
		return m, tuiSelfCmd("Gen "+r.Stack, "dynamics", "generate", r.Stack)
	case "gen_force":
		return m, tuiSelfCmd("Force gen "+r.Stack, "dynamics", "generate", r.Stack, "force")
	}
	return m, nil
}
