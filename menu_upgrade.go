package main

// menu_upgrade.go — Upgrade tab: checks GitHub for a newer stacks program (via
// selfupdateStatus), shows the installed/latest commits + changelog, and ENTER
// applies the update (`stacks selfupdate apply [--force]`). Mirrors draw_upgrade_tab
// / do_upgrade_action. The status is cached so it isn't re-fetched every redraw.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// tuiUpgradeStatus caches the last self-update status snapshot.
var tuiUpgradeStatus map[string]interface{}
var tuiUpgradeFetched bool

func tuiEnsureUpgradeStatus() map[string]interface{} {
	if !tuiUpgradeFetched {
		tuiUpgradeStatus = selfupdateStatus()
		tuiUpgradeFetched = true
	}
	return tuiUpgradeStatus
}

func tuiUpgradeStr(st map[string]interface{}, key string) string {
	if v, ok := st[key].(string); ok {
		return v
	}
	return "?"
}

func (m menuModel) renderUpgrade() string {
	st := tuiEnsureUpgradeStatus()
	var b strings.Builder
	b.WriteString(tuiAccentStyle.Render("  UPGRADE — CHECK GITHUB FOR PROGRAM UPDATES"))
	b.WriteString("\n")
	b.WriteString(tuiDimStyle.Render("  " + strings.Repeat("─", maxInt(0, m.width-4))))
	b.WriteString("\n")

	if e, ok := st["error"].(string); ok && e != "" {
		b.WriteString(tuiYellowStyle.Render("  ⚠ " + e))
		b.WriteString("\n\n")
		b.WriteString(tuiDimStyle.Render("  Set the clone path:  stacks config STACKS_REPO_DIR /path/to/clone"))
		return b.String()
	}

	b.WriteString(tuiCyanStyle.Render("  Program     : stacks (includes this menu)"))
	b.WriteString("\n")
	b.WriteString(tuiDimStyle.Render(fmt.Sprintf("  Repo        : %s  (%s)",
		tuiUpgradeStr(st, "repo"), tuiUpgradeStr(st, "branch"))))
	b.WriteString("\n")
	b.WriteString(tuiNormalStyle.Render("  Installed   : " + tuiUpgradeStr(st, "current")))
	b.WriteString("\n")
	b.WriteString(tuiNormalStyle.Render("  On GitHub   : " + tuiUpgradeStr(st, "latest")))
	b.WriteString("\n")
	if fe, ok := st["fetch_error"].(string); ok && fe != "" {
		b.WriteString(tuiYellowStyle.Render("  ⚠ couldn't reach GitHub: " + truncate(fe, maxInt(0, m.width-30))))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	if up, _ := st["up_to_date"].(bool); up {
		b.WriteString(tuiGreenStyle.Render("  ✓ Up to date — nothing to install."))
		return b.String()
	}

	behind, _ := st["behind"].(int)
	b.WriteString(tuiYellowStyle.Render(fmt.Sprintf("  ⬆ %d update(s) available:", behind)))
	b.WriteString("\n")
	if cl, ok := st["changelog"].([]string); ok {
		limit := len(cl)
		max := maxInt(1, m.height-18)
		if limit > max {
			limit = max
		}
		for _, line := range cl[:limit] {
			b.WriteString(tuiNormalStyle.Render(truncate("    • "+line, m.width-2)))
			b.WriteString("\n")
		}
	}
	if dirty, _ := st["installed_dirty"].(bool); dirty {
		df, _ := st["dirty_files"].([]string)
		b.WriteString(tuiYellowStyle.Render(fmt.Sprintf(
			"  ⚠ installed copy has %d local change(s) that update would overwrite (a backup is made first).", len(df))))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(tuiAccentStyle.Render("  Press ENTER to update now."))
	return b.String()
}

func (m menuModel) handleUpgradeKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "r", "R":
		tuiUpgradeFetched = false
		return m, nil
	case "enter":
		st := tuiEnsureUpgradeStatus()
		if e, _ := st["error"].(string); e != "" {
			return m, nil
		}
		if up, _ := st["up_to_date"].(bool); up {
			return m, nil
		}
		force, _ := st["installed_dirty"].(bool)
		behind, _ := st["behind"].(int)
		label := "⬇  Yes — update now"
		if force {
			label += " (overwrite local)"
		}
		m.popup = tuiConfirmPopup(fmt.Sprintf("Install %d update(s) from GitHub?", behind), label,
			func() (menuModel, tea.Cmd) {
				tuiUpgradeFetched = false // force a re-check after applying
				if force {
					return m, tuiSelfCmd("Updating stacks", "selfupdate", "apply", "--force")
				}
				return m, tuiSelfCmd("Updating stacks", "selfupdate", "apply")
			})
		return m, nil
	}
	return m, nil
}
