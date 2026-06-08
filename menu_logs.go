package main

// menu_logs.go — Logs tab: lists the engine log files (stacks_*.log in the data
// dir) plus every live container; ENTER shows the recent tail in a scrollable
// output popup (reusing `docker logs` / file reads). Mirrors draw_logs_tab.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// tuiLogRow is one selectable Logs-tab entry.
type tuiLogRow struct {
	Label     string
	Path      string // non-empty for a log file
	Container string // non-empty for a live container
	SizeKB    int64
}

// tuiLogRows mirrors draw_logs_tab: the stacks_*.log files in the data dir,
// followed by the live containers (so per-container `docker logs` is reachable).
func tuiLogRows(d tuiData) []tuiLogRow {
	var rows []tuiLogRow
	dir := dispDataDir()
	matches, _ := filepath.Glob(filepath.Join(dir, "stacks_*.log"))
	sort.Strings(matches)
	for _, f := range matches {
		var kb int64
		if st, err := os.Stat(f); err == nil {
			kb = st.Size() / 1024
		}
		rows = append(rows, tuiLogRow{Label: filepath.Base(f), Path: f, SizeKB: kb})
	}
	for _, c := range d.Containers {
		rows = append(rows, tuiLogRow{Label: "▶ " + c.Name + " (docker logs)", Container: c.Name})
	}
	return rows
}

func (m menuModel) renderLogs() string {
	rows := tuiLogRows(m.data)
	var b strings.Builder
	b.WriteString(tuiAccentStyle.Render("  DOCKER LOGS — ENTER shows the recent tail"))
	b.WriteString("\n")
	b.WriteString(tuiDimStyle.Render("  " + strings.Repeat("─", maxInt(0, m.width-4))))
	b.WriteString("\n")
	if len(rows) == 0 {
		b.WriteString(tuiDimStyle.Render("  No log files or containers found."))
		return b.String()
	}
	vis := m.visibleRows()
	end := m.scroll + vis
	if end > len(rows) {
		end = len(rows)
	}
	for i := m.scroll; i < end; i++ {
		r := rows[i]
		sz := ""
		if r.Path != "" {
			sz = fmt.Sprintf("%dK", r.SizeKB)
		}
		line := fmt.Sprintf("%-44s %6s", truncate(r.Label, 44), sz)
		if i == m.sel {
			b.WriteString(tuiSelectedStyle.Render(truncate("  ▶ "+line, m.width-2)))
		} else {
			st := tuiNormalStyle
			if r.Container != "" {
				st = tuiGreenStyle
			}
			b.WriteString(st.Render(truncate("    "+line, m.width-2)))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (m menuModel) handleLogsKey(k string) (tea.Model, tea.Cmd) {
	rows := tuiLogRows(m.data)
	if m.sel >= len(rows) {
		m.sel = maxInt(0, len(rows)-1)
	}
	switch k {
	case "up", "k", "down", "j", "pgup", "pgdown", "home", "end":
		m.moveCursor(k, len(rows))
	case "enter", "tab":
		if len(rows) == 0 {
			return m, nil
		}
		r := rows[m.sel]
		if r.Container != "" {
			return m, tuiShellCmd("Logs: "+r.Container, "docker", "logs", "--tail", "200", r.Container)
		}
		// log file: show the last ~400 lines
		return m, tuiDockerCmd("Log: "+r.Label, func() string {
			return tuiTailFile(r.Path, 400)
		})
	}
	return m, nil
}

// tuiTailFile returns the last n lines of a file (best-effort, full read).
func tuiTailFile(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "Could not read " + path + ": " + err.Error()
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 1 && lines[0] == "" {
		return "(empty log)"
	}
	return strings.Join(lines, "\n")
}
