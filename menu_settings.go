package main

// menu_settings.go — Settings tab: parse stacks.conf into KEY / VALUE / comment
// rows; ENTER toggles a 0/1 switch or edits any value, then saves into stacks.conf
// in place AND mirrors the change into stacks.yaml (via yamlSetScalar / yamlSetList
// for mapped keys). Mirrors get_settings_items / _settings_save / draw_settings_tab.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type tuiSetting struct {
	Key  string
	Val  string
	Desc string
}

func tuiSettingsConf() string { return filepath.Join(configDir(), "stacks.conf") }

// tuiLoadSettings mirrors get_settings_items().
func tuiLoadSettings() []tuiSetting {
	var items []tuiSetting
	data, err := os.ReadFile(tuiSettingsConf())
	if err != nil {
		return items
	}
	desc := ""
	for _, raw := range strings.Split(string(data), "\n") {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if strings.HasPrefix(s, "#") {
			desc = strings.TrimSpace(strings.TrimLeft(s, "#"))
			continue
		}
		if k, v, ok := strings.Cut(s, "="); ok {
			items = append(items, tuiSetting{
				Key:  strings.TrimSpace(k),
				Val:  strings.Trim(strings.TrimSpace(v), `"'`),
				Desc: desc,
			})
			desc = ""
		}
	}
	return items
}

// tuiSettingsReverseMap mirrors _settings_reverse_map: internal KEY -> (friendly, kind).
func tuiSettingsReverseMap() map[string][2]string {
	rev := map[string][2]string{}
	for fk, ik := range scalarMap {
		rev[ik] = [2]string{fk, "scalar"}
	}
	for fk, lj := range listMap {
		rev[lj.key] = [2]string{fk, "list"}
	}
	return rev
}

var tuiListSplitRe = regexp.MustCompile(`[,\s]+`)
var tuiSpaceSplitRe = regexp.MustCompile(`\s+`)

// tuiSettingsSave mirrors _settings_save: write into stacks.conf, mirror to yaml.
func tuiSettingsSave(key, value string) {
	// 1) stacks.conf in place.
	if data, err := os.ReadFile(tuiSettingsConf()); err == nil {
		lines := strings.Split(string(data), "\n")
		found := false
		for i, l := range lines {
			st := strings.TrimSpace(l)
			if st != "" && !strings.HasPrefix(st, "#") && strings.Contains(st, "=") {
				if k, _, _ := strings.Cut(st, "="); strings.TrimSpace(k) == key {
					qv := value
					if value != "" && strings.ContainsAny(value, " \t#") &&
						!(strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`)) {
						qv = `"` + value + `"`
					}
					lines[i] = key + "=" + qv
					found = true
				}
			}
		}
		if !found {
			lines = append(lines, key+"="+value)
		}
		_ = os.WriteFile(tuiSettingsConf(), []byte(strings.Join(lines, "\n")), 0644)
	}
	// 2) yaml overlay for mapped keys.
	rev := tuiSettingsReverseMap()
	if pair, ok := rev[key]; ok {
		fk, kind := pair[0], pair[1]
		if kind == "scalar" {
			yamlSetScalar(fk, value)
		} else {
			join := " "
			if lj, ok := listMap[fk]; ok {
				join = lj.join
			}
			splitter := tuiSpaceSplitRe
			if join == "," {
				splitter = tuiListSplitRe
			}
			var parts []string
			for _, p := range splitter.Split(value, -1) {
				if p != "" {
					parts = append(parts, p)
				}
			}
			yamlSetList(fk, parts)
		}
	}
}

func (m menuModel) renderSettings() string {
	items := tuiLoadSettings()
	var b strings.Builder
	b.WriteString(tuiAccentStyle.Render("  SETTINGS — ENTER toggles a switch or edits a value (auto-saves)"))
	b.WriteString("\n")
	b.WriteString(tuiDimStyle.Render("  " + strings.Repeat("─", maxInt(0, m.width-4))))
	b.WriteString("\n")
	if len(items) == 0 {
		b.WriteString(tuiDimStyle.Render("  No settings found in stacks.conf."))
		return b.String()
	}
	vis := m.visibleRows()
	end := m.scroll + vis
	if end > len(items) {
		end = len(items)
	}
	for i := m.scroll; i < end; i++ {
		it := items[i]
		isBool := it.Val == "0" || it.Val == "1"
		shown := orNone(truncate(it.Val, 24), "—")
		if isBool {
			if it.Val == "1" {
				shown = "● ON "
			} else {
				shown = "○ OFF"
			}
		}
		line := fmt.Sprintf("%-30s %-26s %s", it.Key, shown, it.Desc)
		if i == m.sel {
			b.WriteString(tuiSelectedStyle.Render(truncate("▶ "+line, m.width-2)))
		} else {
			b.WriteString(tuiNormalStyle.Render(fmt.Sprintf("  %-30s ", it.Key)))
			vc := tuiCyanStyle
			if isBool {
				if it.Val == "1" {
					vc = tuiGreenStyle
				} else {
					vc = tuiDimStyle
				}
			}
			b.WriteString(vc.Render(fmt.Sprintf("%-26s ", shown)))
			b.WriteString(tuiDimStyle.Render(truncate(it.Desc, maxInt(0, m.width-63))))
		}
		b.WriteString("\n")
	}
	b.WriteString(tuiDimStyle.Render(fmt.Sprintf("  %d/%d", m.sel+1, len(items))))
	return b.String()
}

func (m menuModel) handleSettingsKey(k string) (tea.Model, tea.Cmd) {
	items := tuiLoadSettings()
	if m.sel >= len(items) {
		m.sel = maxInt(0, len(items)-1)
	}
	switch k {
	case "up", "k", "down", "j", "pgup", "pgdown", "home", "end":
		m.moveCursor(k, len(items))
	case "enter":
		if len(items) == 0 {
			return m, nil
		}
		it := items[m.sel]
		if it.Val == "0" || it.Val == "1" {
			nv := "1"
			if it.Val == "1" {
				nv = "0"
			}
			tuiSettingsSave(it.Key, nv)
		} else {
			prompt := it.Desc
			if prompt == "" {
				prompt = "Value:"
			}
			m.popup = tuiInputPopup("Edit "+it.Key, truncate(prompt, 46), it.Val,
				func(nv string) (menuModel, tea.Cmd) {
					if nv != it.Val {
						tuiSettingsSave(it.Key, nv)
					}
					return m, nil
				})
		}
	}
	return m, nil
}
