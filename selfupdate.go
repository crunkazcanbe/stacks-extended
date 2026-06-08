// selfupdate.go — faithful Go port of stacks_selfupdate.py.
//
// Check GitHub for updates to the stacks program (which includes the TUI menu,
// stacks_menu.py) and apply them on request.
//
// Model: the program is deployed from a git clone via its install.sh
// (`cp bin/stacks /usr/local/bin`, `cp lib/*.py /usr/local/lib`). "Update" =
// git fetch the clone, and if origin is ahead, `git pull --ff-only` then re-run
// install.sh. The installed files are ALWAYS backed up first (reversible), and if
// the installed copy has local edits not in the clone we warn before overwriting.
//
// CLI:
//
//	stacks_selfupdate.py check   [--json]   # fetch + report (default)
//	stacks_selfupdate.py apply              # pull + backup + install
//	stacks_selfupdate.py where              # print the detected repo dir
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	selfupdateInstallBin = "/usr/local/bin/stacks"
	selfupdateInstallLib = "/usr/local/lib"
)

var selfupdateCandidateRepos = []string{
	"~/stacks", "~/git/stacks", "~/src/stacks",
	"~/.local/share/stacks", "~/projects/stacks",
}

// selfupdateConfDir mirrors the module-level CONF_DIR computation. The Python
// hardcodes ~/.config/stacks via STACKS_CONFIG_DIR; we reuse configDir().
func selfupdateConfDir() string {
	return configDir()
}

func selfupdateBackupDir() string {
	return filepath.Join(selfupdateConfDir(), "selfupdate-backups")
}

// selfupdateLoadConf mirrors load_conf(): parse stacks.conf KEY=VALUE lines, then
// overlay the structured config (stacks_config.load()).
func selfupdateLoadConf() map[string]string {
	cfg := map[string]string{}
	conf := filepath.Join(selfupdateConfDir(), "stacks.conf")
	if data, err := os.ReadFile(conf); err == nil {
		for _, line := range splitLines(string(data)) {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
				continue
			}
			k, v, _ := strings.Cut(line, "=")
			cfg[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	// overlay structured config (equivalent of `import stacks_config; cfg.update(_sc.load())`)
	for k, v := range configLoad() {
		cfg[k] = v
	}
	return cfg
}

// selfupdateGitResult mirrors the (returncode, stdout, stderr) of subprocess.run.
type selfupdateGitResult struct {
	returncode int
	stdout     string
	stderr     string
}

// selfupdateGit mirrors _git(repo, *args, timeout=60).
func selfupdateGit(repo string, timeout time.Duration, args ...string) selfupdateGitResult {
	cmdArgs := append([]string{"-C", repo}, args...)
	cmd := exec.Command("git", cmdArgs...)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		return selfupdateGitResult{returncode: 1, stdout: "", stderr: err.Error()}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		rc := 0
		if err != nil {
			rc = 1
			if ee, ok := err.(*exec.ExitError); ok {
				rc = ee.ExitCode()
			}
		}
		return selfupdateGitResult{returncode: rc, stdout: outBuf.String(), stderr: errBuf.String()}
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return selfupdateGitResult{returncode: 1, stdout: "", stderr: "timeout"}
	}
}

// selfupdateIsStacksRepo mirrors _is_stacks_repo(path).
func selfupdateIsStacksRepo(path string) bool {
	if st, err := os.Stat(filepath.Join(path, ".git")); err != nil || !st.IsDir() {
		return false
	}
	if st, err := os.Stat(filepath.Join(path, "install.sh")); err != nil || st.IsDir() {
		return false
	}
	if st, err := os.Stat(filepath.Join(path, "lib", "stacks_menu.py")); err != nil || st.IsDir() {
		return false
	}
	return true
}

// selfupdateRepoDir mirrors repo_dir(): locate the stacks git clone.
// (Named uniquely to avoid colliding with the project's repoDir() in config.go.)
func selfupdateRepoDir() string {
	cfg := selfupdateLoadConf()
	if p := strings.TrimSpace(cfg["STACKS_REPO_DIR"]); p != "" {
		p = expandUser(p)
		if selfupdateIsStacksRepo(p) {
			return p
		}
	}
	for _, cand := range selfupdateCandidateRepos {
		cand = expandUser(cand)
		if selfupdateIsStacksRepo(cand) {
			return cand
		}
	}
	return ""
}

// selfupdateBranchOf mirrors branch_of(repo).
func selfupdateBranchOf(repo string) string {
	cfg := selfupdateLoadConf()
	if b := strings.TrimSpace(cfg["STACKS_UPDATE_BRANCH"]); b != "" {
		return b
	}
	r := selfupdateGit(repo, 60*time.Second, "rev-parse", "--abbrev-ref", "HEAD")
	if r.returncode == 0 && strings.TrimSpace(r.stdout) != "" {
		return strings.TrimSpace(r.stdout)
	}
	return "master"
}

// selfupdateSame mirrors _same(a, b).
func selfupdateSame(a, b string) bool {
	da, err := os.ReadFile(a)
	if err != nil {
		return false
	}
	db, err := os.ReadFile(b)
	if err != nil {
		return false
	}
	return string(da) == string(db)
}

// selfupdateInstalledDirty mirrors _installed_dirty(repo). Returns (dirty, files).
func selfupdateInstalledDirty(repo string) (bool, []string) {
	var diffs []string
	rb := filepath.Join(repo, "bin", "stacks")
	if selfupdateIsFile(rb) && selfupdateIsFile(selfupdateInstallBin) {
		if !selfupdateSame(rb, selfupdateInstallBin) {
			diffs = append(diffs, "bin/stacks")
		}
	}
	matches, _ := filepath.Glob(filepath.Join(repo, "lib", "*.py"))
	for _, f := range matches {
		inst := filepath.Join(selfupdateInstallLib, filepath.Base(f))
		if selfupdateIsFile(inst) && !selfupdateSame(f, inst) {
			diffs = append(diffs, "lib/"+filepath.Base(f))
		}
	}
	return len(diffs) > 0, diffs
}

func selfupdateIsFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// selfupdateStatus mirrors status(): fetch origin and report.
func selfupdateStatus() map[string]interface{} {
	repo := selfupdateRepoDir()
	if repo == "" {
		return map[string]interface{}{
			"error": "No stacks git clone found. Set STACKS_REPO_DIR in stacks.conf " +
				"to the folder you installed from (the one with install.sh).",
		}
	}
	branch := selfupdateBranchOf(repo)
	f := selfupdateGit(repo, 45*time.Second, "fetch", "origin", branch)
	fetchErr := ""
	if f.returncode != 0 {
		s := f.stderr
		if s == "" {
			s = f.stdout
		}
		s = strings.TrimSpace(s)
		if len(s) > 160 {
			s = s[:160]
		}
		fetchErr = s
	}
	cur := strings.TrimSpace(selfupdateGit(repo, 60*time.Second, "rev-parse", "--short", "HEAD").stdout)
	latest := strings.TrimSpace(selfupdateGit(repo, 60*time.Second, "rev-parse", "--short", "origin/"+branch).stdout)
	behind := strings.TrimSpace(selfupdateGit(repo, 60*time.Second, "rev-list", "--count", "HEAD..origin/"+branch).stdout)
	if behind == "" {
		behind = "0"
	}
	ahead := strings.TrimSpace(selfupdateGit(repo, 60*time.Second, "rev-list", "--count", "origin/"+branch+"..HEAD").stdout)
	if ahead == "" {
		ahead = "0"
	}
	log := selfupdateGit(repo, 60*time.Second, "log", "--oneline", "--no-decorate", "HEAD..origin/"+branch)
	changelog := []string{}
	if log.returncode == 0 {
		for _, l := range strings.Split(strings.TrimSpace(log.stdout), "\n") {
			if strings.TrimSpace(l) != "" {
				changelog = append(changelog, l)
			}
		}
	}
	dirty, dirtyFiles := selfupdateInstalledDirty(repo)

	behindN, err := strconv.Atoi(behind)
	if err != nil {
		behindN = 0
	}
	aheadN := 0
	if selfupdateIsDigit(ahead) {
		aheadN, _ = strconv.Atoi(ahead)
	}
	if dirtyFiles == nil {
		dirtyFiles = []string{}
	}
	return map[string]interface{}{
		"repo": repo, "branch": branch,
		"current": cur, "latest": latest,
		"behind": behindN, "ahead": aheadN,
		"changelog":       changelog,
		"installed_dirty": dirty, "dirty_files": dirtyFiles,
		"fetch_error": fetchErr,
		"up_to_date":  behindN == 0 && fetchErr == "",
	}
}

// selfupdateIsDigit mirrors Python str.isdigit() for the ahead-count check.
func selfupdateIsDigit(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// selfupdateBackupInstalled mirrors _backup_installed().
func selfupdateBackupInstalled() string {
	dst := filepath.Join(selfupdateBackupDir(), time.Now().Format("20060102-150405"))
	_ = os.MkdirAll(filepath.Join(dst, "lib"), 0o755)
	func() {
		defer func() { recover() }()
		if selfupdateIsFile(selfupdateInstallBin) {
			_ = selfupdateCopy2(selfupdateInstallBin, filepath.Join(dst, "stacks"))
		}
		matches, _ := filepath.Glob(filepath.Join(selfupdateInstallLib, "stacks_*.py"))
		for _, f := range matches {
			_ = selfupdateCopy2(f, filepath.Join(dst, "lib", filepath.Base(f)))
		}
	}()
	return dst
}

// selfupdateCopy2 mirrors shutil.copy2 (copy contents + mode).
func selfupdateCopy2(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(src); err == nil {
		mode = st.Mode()
	}
	return os.WriteFile(dst, data, mode)
}

// selfupdateApply mirrors apply(allow_overwrite_local=False).
// Returns (ok, msg, log).
func selfupdateApply(allowOverwriteLocal bool) (bool, string, []string) {
	st := selfupdateStatus()
	if e, ok := st["error"].(string); ok && e != "" {
		return false, e, []string{}
	}
	repo := st["repo"].(string)
	branch := st["branch"].(string)
	var log []string
	behind := st["behind"].(int)
	fetchErr := st["fetch_error"].(string)
	installedDirty := st["installed_dirty"].(bool)
	dirtyFiles := st["dirty_files"].([]string)

	if behind == 0 && fetchErr == "" {
		return true, "Already up to date.", []string{}
	}
	if installedDirty && !allowOverwriteLocal {
		head := dirtyFiles
		if len(head) > 4 {
			head = head[:4]
		}
		return false, fmt.Sprintf(
			"Installed copy has %d local change(s) not in git "+
				"(e.g. %s). These would be overwritten. "+
				"Re-run with apply --force to proceed anyway (a backup is always made).",
			len(dirtyFiles), strings.Join(head, ", ")), dirtyFiles
	}
	bak := selfupdateBackupInstalled()
	log = append(log, fmt.Sprintf("backed up installed files → %s", bak))
	pull := selfupdateGit(repo, 120*time.Second, "pull", "--ff-only", "origin", branch)
	log = append(log, strings.TrimSpace(pull.stdout))
	if pull.returncode != 0 {
		s := pull.stderr
		if s == "" {
			s = pull.stdout
		}
		s = strings.TrimSpace(s)
		if len(s) > 160 {
			s = s[:160]
		}
		return false, "git pull failed: " + s, log
	}
	inst := exec.Command("sudo", "bash", filepath.Join(repo, "install.sh"))
	var instOut, instErr strings.Builder
	inst.Stdout = &instOut
	inst.Stderr = &instErr
	instRC := 0
	if err := selfupdateRunWithTimeout(inst, 180*time.Second); err != nil {
		instRC = 1
		if ee, ok := err.(*exec.ExitError); ok {
			instRC = ee.ExitCode()
		}
	}
	log = append(log, strings.TrimSpace(instOut.String()))
	if instRC != 0 {
		s := instErr.String()
		if s == "" {
			s = instOut.String()
		}
		s = strings.TrimSpace(s)
		if len(s) > 160 {
			s = s[:160]
		}
		return false, "install.sh failed: " + s, log
	}
	return true, fmt.Sprintf("Updated to %s (%d commit(s)). Restart the menu to load it.",
		st["latest"].(string), behind), log
}

// selfupdateRunWithTimeout runs an already-configured exec.Cmd with a timeout.
func selfupdateRunWithTimeout(cmd *exec.Cmd, timeout time.Duration) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("timeout")
	}
}

// ────────────────────────────── CLI ──────────────────────────────

const selfupdateDoc = `
stacks_selfupdate.py — check GitHub for updates to the stacks program (which
includes the TUI menu, stacks_menu.py) and apply them on request.

Model: the program is deployed from a git clone via its install.sh
(` + "`cp bin/stacks /usr/local/bin`" + `, ` + "`cp lib/*.py /usr/local/lib`" + `). "Update" =
git fetch the clone, and if origin is ahead, ` + "`git pull --ff-only`" + ` then re-run
install.sh. The installed files are ALWAYS backed up first (reversible), and if
the installed copy has local edits not in the clone we warn before overwriting.

CLI:
    stacks_selfupdate.py check   [--json]   # fetch + report (default)
    stacks_selfupdate.py apply              # pull + backup + install
    stacks_selfupdate.py where              # print the detected repo dir
`

// selfupdateMain mirrors main(): the CLI dispatcher.
func selfupdateMain(args []string) {
	cmd := "check"
	if len(args) > 0 {
		cmd = args[0]
	}

	if cmd == "where" {
		r := selfupdateRepoDir()
		if r == "" {
			fmt.Println("(no stacks git clone found)")
		} else {
			fmt.Println(r)
		}
		return
	}

	if cmd == "check" || cmd == "status" {
		st := selfupdateStatus()
		if e, ok := st["error"].(string); ok && e != "" {
			fmt.Println("⚠ " + e)
			os.Exit(2)
		}
		if inList(args, "--json") {
			b, _ := json.MarshalIndent(selfupdateOrderedStatus(st), "", "  ")
			fmt.Println(string(b))
			return
		}
		fmt.Printf("\n\033[1;35m⬆ stacks self-update\033[0m   repo: %s  (%s)\n",
			st["repo"].(string), st["branch"].(string))
		fmt.Printf("  installed commit: %s   latest on GitHub: %s\n",
			st["current"].(string), st["latest"].(string))
		if fe := st["fetch_error"].(string); fe != "" {
			fmt.Printf("  \033[1;33m⚠ couldn't reach GitHub: %s\033[0m\n", fe)
		}
		if st["up_to_date"].(bool) {
			fmt.Print("  \033[1;32m✓ Up to date.\033[0m\n")
		} else {
			behind := st["behind"].(int)
			changelog := st["changelog"].([]string)
			fmt.Printf("  \033[1;33m⬆ %d update(s) available:\033[0m\n", behind)
			limit := len(changelog)
			if limit > 15 {
				limit = 15
			}
			for _, line := range changelog[:limit] {
				fmt.Printf("      %s\n", line)
			}
			if len(changelog) > 15 {
				fmt.Printf("      … +%d more\n", len(changelog)-15)
			}
			if st["installed_dirty"].(bool) {
				fmt.Printf("  \033[1;31m⚠ installed copy has local changes (%d files) "+
					"that update would overwrite — use 'apply --force'.\033[0m\n",
					len(st["dirty_files"].([]string)))
			}
			fmt.Print("\n  Update:  stacks update apply\n")
		}
		return
	}

	if cmd == "apply" {
		force := inList(args, "--force") || inList(args, "-f")
		fmt.Println("Updating stacks from GitHub…")
		ok, msg, log := selfupdateApply(force)
		for _, l := range log {
			if l != "" {
				fmt.Println("  " + l)
			}
		}
		prefix := "✗ "
		if ok {
			prefix = "✓ "
		}
		fmt.Println(prefix + msg)
		if ok {
			os.Exit(0)
		}
		os.Exit(1)
	}

	fmt.Print(selfupdateDoc)
}

// selfupdateOrderedStatus mirrors json.dumps(st, indent=2) which preserves the
// dict insertion order. Go maps are unordered, so we project into an ordered
// representation matching the Python key order.
func selfupdateOrderedStatus(st map[string]interface{}) json.Marshaler {
	return selfupdateOrderedMap{st: st, keys: []string{
		"repo", "branch", "current", "latest", "behind", "ahead",
		"changelog", "installed_dirty", "dirty_files", "fetch_error", "up_to_date",
	}}
}

type selfupdateOrderedMap struct {
	st   map[string]interface{}
	keys []string
}

func (m selfupdateOrderedMap) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	first := true
	// include only keys present, preserving order; any extra keys (e.g. error) appended.
	used := map[string]bool{}
	emit := func(k string) {
		v, ok := m.st[k]
		if !ok {
			return
		}
		used[k] = true
		if !first {
			b.WriteByte(',')
		}
		first = false
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(v)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	for _, k := range m.keys {
		emit(k)
	}
	// append any remaining keys deterministically
	var extra []string
	for k := range m.st {
		if !used[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		emit(k)
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}
