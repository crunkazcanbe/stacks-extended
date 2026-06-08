package main

// menu_backup.go — Backup tab: run a full backup, take a pre-backup snapshot,
// clean old backups, and view the engine log files. Mirrors BACKUP_ITEMS /
// draw_backup_tab, wired to the Go backup + snapshot engines.

import (
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
)

var tuiBackupItems = []tuiAction{
	{"Run full backup now", "backup_full"},
	{"Run pre-backup snapshot", "backup_pre"},
	{"Clean old backups", "backup_clean"},
	{"View backup log", "view_backup_log"},
	{"View stacks up log", "view_up_log"},
	{"View stacks fix log", "view_fix_log"},
	{"View stacks build log", "view_build_log"},
}

func (m menuModel) renderBackup() string {
	return tuiRenderActionList("BACKUP & LOGS", tuiBackupItems, m.sel, m.scroll, m.visibleRows(), m.width)
}

func (m menuModel) handleBackupKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "up", "k", "down", "j", "pgup", "pgdown", "home", "end":
		m.moveCursor(k, len(tuiBackupItems))
	case "enter":
		if m.sel < 0 || m.sel >= len(tuiBackupItems) {
			return m, nil
		}
		return m.doBackupAction(tuiBackupItems[m.sel].Value)
	}
	return m, nil
}

func (m menuModel) doBackupAction(action string) (menuModel, tea.Cmd) {
	dataDir := dispDataDir()
	switch action {
	case "backup_full":
		return m, tuiSelfCmd("Full backup", "backup", "all")
	case "backup_pre":
		return m, tuiSelfCmd("Pre-backup snapshot", "snapshot")
	case "backup_clean":
		return m, tuiSelfCmd("Clean backups", "backup", "clean")
	case "view_backup_log":
		return m, tuiViewLog("backup log", filepath.Join(dataDir, "stacks_backup.log"))
	case "view_up_log":
		return m, tuiViewLog("up log", filepath.Join(dataDir, "stacks_up.log"))
	case "view_fix_log":
		return m, tuiViewLog("fix log", filepath.Join(dataDir, "stacks_fix.log"))
	case "view_build_log":
		return m, tuiViewLog("build log", filepath.Join(dataDir, "stacks_build.log"))
	}
	return m, nil
}

// tuiViewLog shows the tail of a log file in an output popup.
func tuiViewLog(title, path string) tea.Cmd {
	return tuiDockerCmd(title, func() string { return tuiTailFile(path, 400) })
}
