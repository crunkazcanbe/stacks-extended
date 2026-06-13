package lib

// families.go — Families tab: user-defined, arbitrary groups of containers (NOT
// tied to stack files). A family is just a name + a list of container names.
// Group actions: Start family (start each + DISABLE Zero Scale so members stay
// up, no idle-sleep), Stop family (stop each + RE-ENABLE Zero Scale so they're
// sleepable again), Edit members (multi-select picker), Delete family.
//
// Families are persisted to <configDir>/families.yaml — mirroring the
// zeroscale.yaml loader/saver pattern. IMPORTANT: families are NEVER added to
// the boot boss-list (boot.yaml), so they never auto-start at boot.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"gopkg.in/yaml.v3"
)

// ── Config model ─────────────────────────────────────────────────────────────

// family is one named group of containers.
type family struct {
	Name       string   `yaml:"name"`
	Containers []string `yaml:"containers"`
}

// familiesConfig is the top-level families.yaml document.
type familiesConfig struct {
	Families []*family `yaml:"families"`
}

func familiesPath() string { return filepath.Join(configDir(), "families.yaml") }

// loadFamilies reads families.yaml (mirrors loadZSConfig). Missing file = empty.
func loadFamilies() *familiesConfig {
	c := &familiesConfig{}
	data, err := os.ReadFile(familiesPath())
	if err == nil {
		_ = yaml.Unmarshal(data, c)
	}
	return c
}

// saveFamilies writes families.yaml (mirrors saveZSConfig).
func saveFamilies(c *familiesConfig) error {
	out, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(familiesPath(), out, 0644)
}

// find returns the family with the given name (case-sensitive) or nil.
func (c *familiesConfig) find(name string) *family {
	for _, f := range c.Families {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// remove drops the family with the given name; returns true if one was removed.
func (c *familiesConfig) remove(name string) bool {
	for i, f := range c.Families {
		if f.Name == name {
			c.Families = append(c.Families[:i], c.Families[i+1:]...)
			return true
		}
	}
	return false
}

// ── Zero Scale per-member toggle ─────────────────────────────────────────────
//
// These mirror the per-container "toggle" branch of doZeroScaleAction: ensure a
// site keyed by the container name exists, then flip its enabled flag. Starting
// a family DISABLES Zero Scale (Enabled=false) so members stay up; stopping a
// family RE-ENABLES it (Enabled=true) so they're sleepable again.

// familyEnsureSite finds (or creates, keyed by name) the Zero Scale site for a
// single container — the same shape doZeroScaleAction.ensure() builds.
func familyEnsureSite(c *zsConfig, name string) *zsSite {
	key, s := c.siteForContainer(name)
	if s == nil {
		key = name
		s = &zsSite{
			Host:       []string{name + ".loveiznothin.com"},
			Containers: []string{name},
			Service:    name + "-svc@file",
			Display:    name,
		}
		c.Sites[key] = s
	}
	return s
}

// setFamilyZeroScale flips wake-on-visit for every member and persists once.
// enabled=false → members stay up (used on Start); enabled=true → sleepable
// again (used on Stop). No-op (other than load) if Zero Scale isn't configured.
func setFamilyZeroScale(members []string, enabled bool) {
	c := loadZSConfig()
	if c.Sites == nil {
		c.Sites = map[string]*zsSite{}
	}
	v := enabled
	for _, name := range members {
		s := familyEnsureSite(c, name)
		s.Enabled = &v
	}
	_ = saveZSConfig(c)
}

// ── Tab rendering ────────────────────────────────────────────────────────────

func (m menuModel) renderFamilies() string {
	cfg := loadFamilies()
	var b strings.Builder
	b.WriteString(tuiAccentStyle.Render("  FAMILIES — custom container groups (group start/stop + auto Zero Scale)"))
	b.WriteString("\n")
	b.WriteString(tuiDimStyle.Render("  " + strings.Repeat("─", maxInt(0, m.width-4))))
	b.WriteString("\n")

	if len(cfg.Families) == 0 {
		b.WriteString("\n")
		b.WriteString(tuiDimStyle.Render("  No families yet. Press n to create one (name + pick containers)."))
		b.WriteString("\n")
		return b.String()
	}

	// running-state lookup from the live snapshot
	running := map[string]bool{}
	for _, c := range m.data.Containers {
		if strings.EqualFold(c.State, "running") {
			running[c.Name] = true
		}
	}

	vis := m.visibleRows()
	end := m.scroll + vis
	if end > len(cfg.Families) {
		end = len(cfg.Families)
	}
	for i := m.scroll; i < end; i++ {
		f := cfg.Families[i]
		up := 0
		for _, cn := range f.Containers {
			if running[cn] {
				up++
			}
		}
		line := fmt.Sprintf("%-22s — %d containers (%d up)", truncate(f.Name, 22), len(f.Containers), up)
		ind := "○"
		indStyle := tuiStoppedStyle
		if up > 0 {
			ind = "●"
			indStyle = tuiRunningStyle
		}
		if i == m.sel {
			b.WriteString(tuiSelectedStyle.Render(truncate(ind+" "+line, m.width-2)))
		} else {
			b.WriteString(indStyle.Render(ind) + " ")
			b.WriteString(tuiNormalStyle.Render(truncate(line, m.width-4)))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// ── Tab key handling ─────────────────────────────────────────────────────────

func (m menuModel) handleFamiliesKey(k string) (tea.Model, tea.Cmd) {
	cfg := loadFamilies()
	if m.sel >= len(cfg.Families) {
		m.sel = maxInt(0, len(cfg.Families)-1)
	}
	switch k {
	case "up", "k", "down", "j", "pgup", "pgdown", "home", "end":
		m.moveCursor(k, len(cfg.Families))
	case "n", "N":
		return m.openFamilyCreate()
	case "enter", "tab":
		if len(cfg.Families) == 0 {
			return m.openFamilyCreate()
		}
		f := cfg.Families[m.sel]
		return m.openFamilyActions(f.Name)
	}
	return m, nil
}

// ── Create / edit (name prompt → multi-select picker) ────────────────────────

// openFamilyCreate prompts for a name, then opens the container picker.
func (m menuModel) openFamilyCreate() (menuModel, tea.Cmd) {
	m.popup = tuiInputPopup("New family", "Family name:", "",
		func(name string) (menuModel, tea.Cmd) {
			name = strings.TrimSpace(name)
			if name == "" {
				return m, nil
			}
			cfg := loadFamilies()
			if cfg.find(name) != nil {
				m.popup = tuiOutputPopup("New family", []string{"A family named " + name + " already exists."})
				return m, nil
			}
			return m.openFamilyPicker(name, nil)
		})
	return m, nil
}

// openFamilyPicker shows the scrollable checkbox list of ALL containers, with
// preselect ticked, and on ENTER writes the family to families.yaml.
func (m menuModel) openFamilyPicker(name string, preselect []string) (menuModel, tea.Cmd) {
	// Build the item list from the live container snapshot (same list the
	// Containers tab builds). Sorted, deduped; include any preselected member
	// that no longer exists so it isn't silently dropped on edit.
	seen := map[string]bool{}
	var items []string
	for _, c := range m.data.Containers {
		if !seen[c.Name] {
			seen[c.Name] = true
			items = append(items, c.Name)
		}
	}
	sel := map[string]bool{}
	for _, cn := range preselect {
		sel[cn] = true
		if !seen[cn] {
			seen[cn] = true
			items = append(items, cn) // keep a member whose container vanished
		}
	}
	sort.Strings(items)

	m.popup = &tuiPopup{
		kind:     tuiPopupMulti,
		title:    "Pick containers — " + truncate(name, 24),
		items:    items,
		selected: sel,
		onMulti: func(picked []string) (menuModel, tea.Cmd) {
			cfg := loadFamilies()
			if f := cfg.find(name); f != nil {
				f.Containers = picked
			} else {
				cfg.Families = append(cfg.Families, &family{Name: name, Containers: picked})
			}
			if err := saveFamilies(cfg); err != nil {
				m.popup = tuiOutputPopup("Family", []string{"Save failed: " + err.Error()})
				return m, nil
			}
			m.popup = tuiOutputPopup("Family saved",
				[]string{fmt.Sprintf("%q now has %d container(s).", name, len(picked))})
			return m, nil
		},
	}
	return m, nil
}

// ── Per-family action popup ──────────────────────────────────────────────────

func (m menuModel) openFamilyActions(name string) (menuModel, tea.Cmd) {
	acts := []tuiAction{
		{"▶  Start family", "start"},
		{"⏹  Stop family", "stop"},
		{"✎  Edit members", "edit"},
		{"🗑  Delete family", "delete"},
		{"✕  Cancel", "cancel"},
	}
	m.popup = tuiActionPopup("Family: "+truncate(name, 24), acts,
		func(action string) (menuModel, tea.Cmd) {
			return m.doFamilyAction(name, action)
		})
	return m, nil
}

func (m menuModel) doFamilyAction(name, action string) (menuModel, tea.Cmd) {
	cfg := loadFamilies()
	f := cfg.find(name)
	if f == nil {
		return m, nil
	}
	switch action {
	case "", "cancel":
		return m, nil

	case "start":
		members := append([]string{}, f.Containers...)
		return m, tuiDockerCmd("Start family — "+name, func() string {
			// DISABLE Zero Scale first so members stay up (no idle-sleep).
			setFamilyZeroScale(members, false)
			var out []string
			for _, cn := range members {
				if !containerExists(cn) {
					out = append(out, "— "+cn+": missing (skipped)")
					continue
				}
				if startContainer(cn) {
					out = append(out, "▶ "+cn+": started  (Zero Scale OFF — stays up)")
				} else {
					out = append(out, "✗ "+cn+": failed to start")
				}
			}
			if len(out) == 0 {
				out = append(out, "(family is empty)")
			}
			return strings.Join(out, "\n")
		})

	case "stop":
		members := append([]string{}, f.Containers...)
		return m, tuiDockerCmd("Stop family — "+name, func() string {
			var out []string
			for _, cn := range members {
				if !containerExists(cn) {
					out = append(out, "— "+cn+": missing (skipped)")
					continue
				}
				if stopContainer(cn, 30) {
					out = append(out, "⏹ "+cn+": stopped")
				} else {
					out = append(out, "✗ "+cn+": failed to stop")
				}
			}
			// RE-ENABLE Zero Scale so members are sleepable again.
			setFamilyZeroScale(members, true)
			out = append(out, "", "Zero Scale RE-ENABLED for all members (sleepable again).")
			return strings.Join(out, "\n")
		})

	case "edit":
		return m.openFamilyPicker(name, f.Containers)

	case "delete":
		m.popup = tuiConfirmPopup("Delete family "+truncate(name, 20)+"?",
			"🗑  YES — delete this family (containers untouched)", func() (menuModel, tea.Cmd) {
				cfg := loadFamilies()
				cfg.remove(name)
				if err := saveFamilies(cfg); err != nil {
					m.popup = tuiOutputPopup("Delete family", []string{"Save failed: " + err.Error()})
					return m, nil
				}
				m.sel = 0
				m.popup = tuiOutputPopup("Family deleted", []string{name + " removed. Its containers were left alone."})
				return m, nil
			})
		return m, nil
	}
	return m, nil
}
