package main

// config.go — universal paths/settings. NOTHING is hardcoded to one machine:
// every location comes from an env var, the per-user stacks.conf, or a generic
// XDG/home default. Works on any user's computer.

import (
	"bufio"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

func home() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "."
}

// configDir resolves the config dir generically for any user (faithful port of
// stacks_config._resolve_conf_dir). Priority: $STACKS_CONFIG_DIR → the invoking
// user's home under sudo ($SUDO_USER) → $XDG_CONFIG_HOME/stacks → ~/.config/stacks.
func configDir() string {
	if d := os.Getenv("STACKS_CONFIG_DIR"); d != "" {
		return expandUser(d)
	}
	if su := os.Getenv("SUDO_USER"); su != "" && su != "root" {
		if u, err := user.Lookup(su); err == nil && u.HomeDir != "" {
			return filepath.Join(u.HomeDir, ".config", "stacks")
		}
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "stacks")
	}
	return filepath.Join(home(), ".config", "stacks")
}

// expandUser is the Go equivalent of os.path.expanduser for a leading ~.
func expandUser(p string) string {
	if p == "~" {
		return home()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home(), p[2:])
	}
	return p
}

// confValue reads a KEY=VALUE from the per-user stacks.conf ("" if absent).
func confValue(key string) string {
	f, err := os.Open(filepath.Join(configDir(), "stacks.conf"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// stacksDir: $STACKS_DIR → conf STACKS_DIR → $STACKS_DATA_DIR/Stacks → ~/MyDocker/Stacks.
func stacksDir() string {
	if d := os.Getenv("STACKS_DIR"); d != "" {
		return d
	}
	if d := confValue("STACKS_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("STACKS_DATA_DIR"); d != "" {
		return filepath.Join(d, "Stacks")
	}
	return filepath.Join(home(), "MyDocker", "Stacks")
}

func isGitRepo(d string) bool {
	if d == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(d, ".git"))
	return err == nil && st.IsDir()
}

// repoDir: where the git clone lives, for the version string. $STACKS_REPO_DIR →
// conf → the running binary's own dir (if a git repo) → "" (caller shows "dev").
func repoDir() string {
	if d := os.Getenv("STACKS_REPO_DIR"); d != "" {
		return d
	}
	if d := confValue("STACKS_REPO_DIR"); d != "" {
		return d
	}
	if exe, err := os.Executable(); err == nil {
		if d := filepath.Dir(exe); isGitRepo(d) {
			return d
		}
	}
	return ""
}
