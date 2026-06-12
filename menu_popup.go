package main

// menu_popup.go — the popup layer: action menus, confirm dialogs, single-line
// text input, scrollable output/detail boxes, and the rollback picker. Mirrors
// run_popup_action / _confirm_popup / _prompt_text / run_log_popup / show_message_box.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

type tuiPopupKind int

const (
	tuiPopupActions tuiPopupKind = iota // selectable list -> onSelect(value)
	tuiPopupConfirm                     // No/Yes -> onConfirm()
	tuiPopupInput                       // text input -> onSubmit(text)
	tuiPopupOutput                      // read-only output, any key closes
	tuiPopupDetail                      // scrollable read-only detail
)

// tuiAction is one selectable row: a label and an opaque value.
type tuiAction struct {
	Label string
	Value string
}

type tuiPopup struct {
	kind  tuiPopupKind
	title string

	// list / confirm
	actions []tuiAction
	sel     int
	scroll  int

	// input
	prompt string
	buf    string

	// output / detail
	lines     []string
	lineScrol int

	// callbacks (executed by the model; may return a tea.Cmd)
	onSelect  func(value string) (menuModel, tea.Cmd)
	onConfirm func() (menuModel, tea.Cmd)
	onSubmit  func(text string) (menuModel, tea.Cmd)
}

// ── Rendering ────────────────────────────────────────────────────────────────

func (p *tuiPopup) render(width int) string {
	// Size the box to the CONTENT (longest line) so it's a snug little popup
	// floating over the screen, not a near-fullscreen panel. All widths are
	// VISIBLE columns (emoji/wide-char aware) so the right border lines up.
	need := vw(" " + p.title + " ") // title needs w-2 ≥ this
	bump := func(textCols int) {    // a body line needs w-4 ≥ textCols
		if textCols+4 > need {
			need = textCols + 4
		}
		if textCols+2 > need {
			need = textCols + 2
		}
	}
	for _, a := range p.actions {
		bump(vw("  " + a.Label))
	}
	for _, l := range p.lines {
		bump(vw(l))
	}
	if p.kind == tuiPopupInput {
		bump(vw(p.prompt))
		bump(vw("> " + p.buf + "_"))
		bump(vw("ENTER = save   ESC = cancel"))
	}
	bump(vw("↑↓ scroll  (000/000)  any key / ESC close"))
	w := need + 2
	if w < 28 {
		w = 28
	}
	maxW := width - 4
	if maxW > 66 { // keep popups compact even on wide screens
		maxW = 66
	}
	if w > maxW {
		w = maxW
	}
	if w < 20 {
		w = 20
	}
	var b strings.Builder
	bot := "╚" + strings.Repeat("═", w-2) + "╝"
	title := vtrunc(" "+p.title+" ", w-2)
	tw := vw(title)
	tl := (w - 2 - tw) / 2
	if tl < 0 {
		tl = 0
	}
	tr := w - 2 - tw - tl
	topLine := "╔" + strings.Repeat("═", tl) + title + strings.Repeat("═", tr) + "╗"
	b.WriteString(tuiPopupBorder.Render(topLine))
	b.WriteString("\n")

	body := func(s string) {
		s = vtrunc(s, w-4)
		b.WriteString(tuiPopupBorder.Render("║"))
		b.WriteString(" " + s)
		pad := w - 4 - vw(s)
		if pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
		b.WriteString(" ")
		b.WriteString(tuiPopupBorder.Render("║"))
		b.WriteString("\n")
	}

	switch p.kind {
	case tuiPopupActions:
		vis := 14
		end := p.scroll + vis
		if end > len(p.actions) {
			end = len(p.actions)
		}
		for i := p.scroll; i < end; i++ {
			label := vtrunc(p.actions[i].Label, w-6)
			if i == p.sel {
				b.WriteString(tuiPopupBorder.Render("║"))
				b.WriteString(" " + tuiPopupSel.Render(padRight("  "+label, w-4)) + " ")
				b.WriteString(tuiPopupBorder.Render("║"))
				b.WriteString("\n")
			} else {
				body("  " + label)
			}
		}
		if p.scroll > 0 {
			body("▲ more above")
		}
		if end < len(p.actions) {
			body("▼ more below")
		}

	case tuiPopupConfirm:
		body("")
		for i, a := range p.actions {
			label := a.Label
			if i == p.sel {
				b.WriteString(tuiPopupBorder.Render("║"))
				b.WriteString(" " + tuiPopupSel.Render(padRight("  "+label, w-4)) + " ")
				b.WriteString(tuiPopupBorder.Render("║"))
				b.WriteString("\n")
			} else {
				body("  " + label)
			}
		}
		body("")
		body("↑↓ select  ENTER confirm  ESC cancel")

	case tuiPopupInput:
		body(p.prompt)
		body("> " + p.buf + "_")
		body("")
		body("ENTER = save   ESC = cancel")

	case tuiPopupOutput, tuiPopupDetail:
		vis := 16
		end := p.lineScrol + vis
		if end > len(p.lines) {
			end = len(p.lines)
		}
		for i := p.lineScrol; i < end; i++ {
			body(p.lines[i])
		}
		if len(p.lines) > vis {
			body(fmt.Sprintf("↑↓ scroll  (%d/%d)  any key / ESC close", end, len(p.lines)))
		} else {
			body("any key / ESC to close")
		}
	}

	b.WriteString(tuiPopupBorder.Render(bot))
	return b.String()
}

// padRight pads (or truncates) s to n VISIBLE columns — emoji/wide-char aware,
// so highlighted rows have the same width as the box and borders line up.
func padRight(s string, n int) string {
	w := vw(s)
	if w >= n {
		return vtrunc(s, n)
	}
	return s + strings.Repeat(" ", n-w)
}

// vw is the visible (display) column width of s — counts emoji + CJK as 2.
func vw(s string) int { return runewidth.StringWidth(s) }

// vtrunc truncates s to at most n visible columns (no tail).
func vtrunc(s string, n int) string {
	if n < 0 {
		n = 0
	}
	if vw(s) <= n {
		return s
	}
	return runewidth.Truncate(s, n, "")
}

// overlayCenter composites the fg box centered ON TOP of the bg view, so the
// menu pops up over the still-visible container list instead of a blank screen.
// ANSI- and wide-char-aware: keeps the background showing around the box.
func overlayCenter(bg, fg string, totalW, totalH int) string {
	bgLines := strings.Split(bg, "\n")
	for len(bgLines) < totalH {
		bgLines = append(bgLines, "")
	}
	fgLines := strings.Split(strings.TrimRight(fg, "\n"), "\n")
	fgW := 0
	for _, l := range fgLines {
		if x := ansi.StringWidth(l); x > fgW {
			fgW = x
		}
	}
	startRow := (totalH - len(fgLines)) / 2
	if startRow < 0 {
		startRow = 0
	}
	startCol := (totalW - fgW) / 2
	if startCol < 0 {
		startCol = 0
	}
	for i, fl := range fgLines {
		row := startRow + i
		if row < 0 || row >= len(bgLines) {
			continue
		}
		bl := bgLines[row]
		if x := ansi.StringWidth(bl); x < totalW {
			bl += strings.Repeat(" ", totalW-x)
		}
		left := ansi.Truncate(bl, startCol, "")
		right := ansi.TruncateLeft(bl, startCol+ansi.StringWidth(fl), "")
		bgLines[row] = left + fl + right
	}
	return strings.Join(bgLines, "\n")
}

// ── Key handling ─────────────────────────────────────────────────────────────

func (m menuModel) handlePopupKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.popup
	k := msg.String()

	switch p.kind {
	case tuiPopupActions:
		switch k {
		case "up", "k":
			if p.sel > 0 {
				p.sel--
			}
			if p.sel < p.scroll {
				p.scroll = p.sel
			}
		case "down", "j":
			if p.sel < len(p.actions)-1 {
				p.sel++
			}
			if p.sel >= p.scroll+14 {
				p.scroll = p.sel - 13
			}
		case "pgup":
			p.sel -= 14
			if p.sel < 0 {
				p.sel = 0
			}
			p.scroll = p.sel
		case "pgdown":
			p.sel += 14
			if p.sel > len(p.actions)-1 {
				p.sel = len(p.actions) - 1
			}
		case "enter":
			val := p.actions[p.sel].Value
			// "Cancel" just closes the popup — its callback is a no-op and the
			// closure could re-show a stale popup, which left the user stuck.
			if val == "cancel" || strings.EqualFold(p.actions[p.sel].Label, "Cancel") {
				m.popup = nil
				return m, nil
			}
			cb := p.onSelect
			m.popup = nil
			if cb != nil {
				return cb(val)
			}
			return m, nil
		case "esc", "q", "ctrl+c":
			m.popup = nil
		}
		return m, nil

	case tuiPopupConfirm:
		switch k {
		case "up", "k", "down", "j", "left", "right", "tab":
			p.sel = 1 - p.sel
		case "y", "Y":
			p.sel = 1
			fallthrough
		case "enter":
			yes := p.actions[p.sel].Value == "yes"
			cb := p.onConfirm
			m.popup = nil
			if yes && cb != nil {
				return cb()
			}
			return m, nil
		case "n", "N", "esc", "ctrl+c":
			m.popup = nil
		}
		return m, nil

	case tuiPopupInput:
		switch k {
		case "enter":
			text := strings.TrimSpace(p.buf)
			cb := p.onSubmit
			m.popup = nil
			if cb != nil {
				return cb(text)
			}
			return m, nil
		case "esc", "ctrl+c":
			m.popup = nil
		case "backspace":
			if len(p.buf) > 0 {
				p.buf = p.buf[:len(p.buf)-1]
			}
		default:
			if len(msg.Runes) == 1 && msg.Runes[0] >= 32 && msg.Runes[0] < 127 {
				p.buf += string(msg.Runes)
			}
		}
		return m, nil

	case tuiPopupOutput, tuiPopupDetail:
		switch k {
		case "up", "k":
			if p.lineScrol > 0 {
				p.lineScrol--
			}
		case "down", "j":
			if p.lineScrol < len(p.lines)-1 {
				p.lineScrol++
			}
		case "pgup":
			p.lineScrol -= 14
			if p.lineScrol < 0 {
				p.lineScrol = 0
			}
		case "pgdown":
			p.lineScrol += 14
			if p.lineScrol > len(p.lines)-1 {
				p.lineScrol = len(p.lines) - 1
			}
		default:
			m.popup = nil
		}
		return m, nil
	}
	return m, nil
}

// ── Constructors ─────────────────────────────────────────────────────────────

func tuiActionPopup(title string, actions []tuiAction, onSelect func(string) (menuModel, tea.Cmd)) *tuiPopup {
	return &tuiPopup{kind: tuiPopupActions, title: title, actions: actions, onSelect: onSelect}
}

func tuiConfirmPopup(title, dangerLabel string, onConfirm func() (menuModel, tea.Cmd)) *tuiPopup {
	return &tuiPopup{
		kind:  tuiPopupConfirm,
		title: title,
		actions: []tuiAction{
			{Label: "✕  No — cancel", Value: "no"},
			{Label: dangerLabel, Value: "yes"},
		},
		onConfirm: onConfirm,
	}
}

func tuiInputPopup(title, prompt, def string, onSubmit func(string) (menuModel, tea.Cmd)) *tuiPopup {
	return &tuiPopup{kind: tuiPopupInput, title: title, prompt: prompt, buf: def, onSubmit: onSubmit}
}

func tuiOutputPopup(title string, lines []string) *tuiPopup {
	return &tuiPopup{kind: tuiPopupOutput, title: title, lines: lines}
}
