package lib

import (
	_ "embed"
	"os"
	"path/filepath"
)

// default_stacks.conf is a generic, machine-neutral copy of the full annotated
// config — every option the menu can show, with descriptions intact and all
// machine-specific values blanked (so the program auto-detects them). It's
// seeded into the config dir the first time the program runs without one, so the
// Settings tab is never blank on a fresh machine (the VPS, anyone's install).
//
//go:embed default_stacks.conf
var defaultConf string

// ensureConf writes the embedded default stacks.conf into the config dir if no
// conf exists yet. Returns the conf path. Never overwrites an existing file.
func ensureConf() string {
	p := filepath.Join(configDir(), "stacks.conf")
	if _, err := os.Stat(p); err == nil {
		return p // already there — leave the user's config alone
	}
	_ = os.MkdirAll(configDir(), 0755)
	_ = os.WriteFile(p, []byte(defaultConf), 0644)
	return p
}
