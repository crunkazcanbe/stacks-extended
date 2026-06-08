package main

// menu_updates.go — Updates tab: available updates (from the update cache) plus
// the update history (newest-first), searchable with "/" + A-Z. ENTER opens a
// detail box; C/F/P trigger check / force-check / pull. Mirrors get_update_rows
// + draw_updates_tab + UPDATES_ACTIONS.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type tuiUpdateRow struct {
	Kind     string // "update" | "hist"
	Image    string
	Tag      string
	Stacks   []string
	Event    string
	TS       int64
	Old      string
	New      string
	OldShort string
	NewShort string
}

type tuiUpdateSummary struct {
	Updates int
	OK      int
	Errors  int
	Hist    int
}

// tuiBuildUpdateRows mirrors get_update_rows().
func tuiBuildUpdateRows() ([]tuiUpdateRow, tuiUpdateSummary) {
	var rows []tuiUpdateRow
	var sum tuiUpdateSummary
	for _, v := range updLoadCache() {
		switch {
		case v.HasUpdate:
			sum.Updates++
			rows = append(rows, tuiUpdateRow{
				Kind: "update", Image: v.Image, Tag: v.Tag, Stacks: v.Stacks,
				Old: v.LocalDigest, New: v.RemoteDigest, TS: v.Checked,
			})
		case v.Error != "":
			sum.Errors++
		default:
			sum.OK++
		}
	}
	for _, r := range updGetHistory(0) {
		sum.Hist++
		rows = append(rows, tuiUpdateRow{
			Kind: "hist", Image: r.Image, Tag: r.Tag, Stacks: r.Stacks,
			Event: r.Event, TS: r.TS, Old: r.Old, New: r.New,
			OldShort: r.OldShort, NewShort: r.NewShort,
		})
	}
	return rows, sum
}

func (m menuModel) filteredUpdateRows() []tuiUpdateRow {
	var out []tuiUpdateRow
	for _, r := range m.updateRows {
		if m.fltLetter != "" && !tuiMatchLetter(r.Image, m.fltLetter) {
			continue
		}
		if m.fltInline != "" && !tuiContains([]string{r.Image, r.Event}, m.fltInline) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func (m menuModel) renderUpdates() string {
	rows := m.filteredUpdateRows()
	s := m.updateSum
	var b strings.Builder
	b.WriteString(tuiYellowStyle.Render(fmt.Sprintf("  ⬆ Updates: %d   ✔ OK: %d   ✘ Err: %d   ⟳ History: %d",
		s.Updates, s.OK, s.Errors, s.Hist)))
	b.WriteString("\n")
	b.WriteString(tuiAccentStyle.Render(fmt.Sprintf("  %-13s %-10s %-40s %s", "WHEN", "EVENT", "IMAGE", "OLD → NEW")))
	b.WriteString("\n")
	b.WriteString(tuiDimStyle.Render("  " + strings.Repeat("─", maxInt(0, m.width-4))))
	b.WriteString("\n")
	if len(rows) == 0 {
		b.WriteString(tuiDimStyle.Render("  No updates or history yet. Press C to check for updates."))
		return b.String()
	}
	vis := m.visibleRows()
	end := m.scroll + vis
	if end > len(rows) {
		end = len(rows)
	}
	for i := m.scroll; i < end; i++ {
		r := rows[i]
		img := truncate(r.Image, 40)
		var when, ev, ver, mark string
		if r.Kind == "update" {
			when, ev, ver, mark = "now", "AVAILABLE", "update ready", "⬆"
		} else {
			when = tuiFmtTime(r.TS)
			ev = r.Event
			ver = fmt.Sprintf("%s → %s", orNone(r.OldShort, "—"), orNone(r.NewShort, "—"))
			mark = "⬆"
			if ev == "pulled" {
				mark = "⬇"
			}
		}
		line := fmt.Sprintf("%s %-13s %-10s %-40s %s", mark, when, ev, img, ver)
		if i == m.sel {
			b.WriteString(tuiSelectedStyle.Render(truncate("  "+line, m.width-2)))
		} else {
			b.WriteString(tuiNormalStyle.Render(truncate("  "+line, m.width-2)))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (m menuModel) handleUpdatesKey(k string) (tea.Model, tea.Cmd) {
	rows := m.filteredUpdateRows()
	if m.sel >= len(rows) {
		m.sel = maxInt(0, len(rows)-1)
	}
	switch k {
	case "up", "k", "down", "j", "pgup", "pgdown", "home", "end":
		m.moveCursor(k, len(rows))
	case "enter":
		if len(rows) > 0 {
			m.popup = &tuiPopup{kind: tuiPopupDetail, title: "Update detail", lines: tuiUpdateDetail(rows[m.sel])}
		}
	case "C":
		return m, tuiSelfCmd("Check updates", "update")
	case "F":
		return m, tuiSelfCmd("Force check", "update", "--force")
	case "P":
		return m, tuiSelfCmd("Pull updates", "update", "--pull")
	}
	return m, nil
}

func tuiUpdateDetail(r tuiUpdateRow) []string {
	lines := []string{"Image:   " + r.Image}
	if r.Tag != "" {
		lines = append(lines, "Tag:     "+r.Tag)
	}
	if len(r.Stacks) > 0 {
		lines = append(lines, "Stacks:  "+strings.Join(r.Stacks, ", "))
	}
	if r.Kind == "update" {
		lines = append(lines, "Status:  UPDATE AVAILABLE")
	} else {
		lines = append(lines, "Event:   "+r.Event, "When:    "+tuiFmtTime(r.TS))
	}
	lines = append(lines, "", "OLD (was):", "  "+orNone(r.Old, "—"), "NEW (now):", "  "+orNone(r.New, "—"))
	return lines
}
