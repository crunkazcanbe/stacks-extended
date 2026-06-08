package main

// menu_keys.go — keyboard handling for the TUI model. Mirrors the curses main()
// loop: tab switching, per-tab cursor movement, the "/" + A-Z filter, and the
// ENTER/TAB action popups.

import (
	tea "github.com/charmbracelet/bubbletea"
)

func (m menuModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Popup intercepts all keys while open.
	if m.popup != nil {
		return m.handlePopupKey(msg)
	}

	k := msg.String()

	// ── Inline-search typing mode (filterable tabs) ──────────────────────────
	if tuiFilterableTab(m.tab) && m.fltMode {
		switch k {
		case "enter":
			m.fltMode = false
		case "esc":
			m.fltMode = false
			m.fltInline = ""
			m.sel, m.scroll = 0, 0
		case "backspace":
			if len(m.fltInline) > 0 {
				m.fltInline = m.fltInline[:len(m.fltInline)-1]
			}
			m.sel, m.scroll = 0, 0
		default:
			if len(msg.Runes) == 1 && msg.Runes[0] >= 32 && msg.Runes[0] < 127 {
				m.fltInline += string(msg.Runes)
				m.sel, m.scroll = 0, 0
			}
		}
		return m, nil
	}

	// ── Filter activation keys (filterable tabs) ─────────────────────────────
	if tuiFilterableTab(m.tab) {
		switch k {
		case "/":
			m.fltMode = true
			m.fltInline = ""
			return m, nil
		case "#":
			if m.fltLetter == "#" {
				m.fltLetter = ""
			} else {
				m.fltLetter = "#"
			}
			m.sel, m.scroll = 0, 0
			return m, nil
		}
		// lowercase a-z = letter jump (uppercase stays a command on Updates/Network)
		if len(msg.Runes) == 1 {
			r := msg.Runes[0]
			if r >= 'a' && r <= 'z' {
				ch := string(r)
				if m.fltLetter == ch {
					m.fltLetter = ""
				} else {
					m.fltLetter = ch
				}
				m.sel, m.scroll = 0, 0
				return m, nil
			}
		}
		if k == "esc" && (m.fltLetter != "" || m.fltInline != "") {
			m.fltLetter, m.fltInline = "", ""
			m.sel, m.scroll = 0, 0
			return m, nil
		}
	}

	// ── Global keys ──────────────────────────────────────────────────────────
	switch k {
	case "q", "Q", "esc", "ctrl+c":
		m.quit = true
		return m, tea.Quit
	case "right", "l":
		m.tab = (m.tab + 1) % len(tuiTabs)
		m.resetTab()
		return m, nil
	case "left", "h":
		m.tab = (m.tab - 1 + len(tuiTabs)) % len(tuiTabs)
		m.resetTab()
		return m, nil
	}

	// ── Per-tab keys ─────────────────────────────────────────────────────────
	switch m.tab {
	case tabContainers:
		return m.handleContainersKey(k)
	case tabStacks:
		return m.handleStacksKey(k)
	case tabLogs:
		return m.handleLogsKey(k)
	case tabDynamics:
		return m.handleDynamicsKey(k)
	case tabArt:
		return m.handleArtKey(k)
	case tabBackup:
		return m.handleBackupKey(k)
	case tabBuild:
		return m.handleBuildKey(k)
	case tabConfigs:
		return m.handleConfigsKey(k)
	case tabNetwork:
		return m.handleNetworkKey(k)
	case tabUpdates:
		return m.handleUpdatesKey(k)
	case tabSettings:
		return m.handleSettingsKey(k)
	case tabUpgrade:
		return m.handleUpgradeKey(k)
	}
	return m, nil
}

func (m *menuModel) resetTab() {
	m.sel, m.scroll = 0, 0
	m.fltLetter, m.fltInline, m.fltMode = "", "", false
	if m.tab == tabUpdates {
		m.updateRows, m.updateSum = tuiBuildUpdateRows()
	}
	if m.tab == tabNetwork {
		m.netCache = nil
	}
}

// tuiVisibleRows is how many list rows fit below the header/tabs/headers.
func (m menuModel) visibleRows() int {
	v := m.height - 9
	if v < 1 {
		v = 1
	}
	return v
}

func (m *menuModel) moveCursor(k string, n int) {
	vis := m.visibleRows()
	switch k {
	case "up", "k":
		if m.sel > 0 {
			m.sel--
		}
		if m.sel < m.scroll {
			m.scroll = m.sel
		}
	case "down", "j":
		if m.sel < n-1 {
			m.sel++
		}
		if m.sel >= m.scroll+vis {
			m.scroll = m.sel - vis + 1
		}
	case "pgup":
		m.sel -= vis
		if m.sel < 0 {
			m.sel = 0
		}
		if m.sel < m.scroll {
			m.scroll = m.sel
		}
	case "pgdown":
		m.sel += vis
		if m.sel > n-1 {
			m.sel = n - 1
		}
		if m.sel >= m.scroll+vis {
			m.scroll = m.sel - vis + 1
		}
	case "home":
		m.sel, m.scroll = 0, 0
	case "end":
		m.sel = n - 1
		if m.sel < 0 {
			m.sel = 0
		}
		if m.sel >= m.scroll+vis {
			m.scroll = m.sel - vis + 1
		}
	}
}
