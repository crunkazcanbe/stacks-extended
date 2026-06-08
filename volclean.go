package main

// volclean.go — faithful Go port of stacks_volclean.py.
//
// Strip UNUSED top-level named-volume declarations.
//
// When a service is moved to a bind mount, its old top-level `volumes:` entry is
// left orphaned — declared but referenced by nothing — yet Compose still tries to
// create it. This finds declarations no service references and removes them,
// backing up every file first.
//
// SAFETY: a volume is only an orphan if BOTH (a) YAML analysis shows no service
// mounts it, AND (b) its name does not appear as a mount source anywhere in the
// file's text. A volume that is used is never removed. Removal is textual (only the
// orphan's block is deleted; the rest of the file is untouched), and if the whole
// top-level `volumes:` section becomes empty its header is removed too.
//
// CLI:
//     stacks volclean report [--json]
//     stacks volclean clean [--auto] [stack ...]
//     stacks volclean ensure [stack ...]

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	yaml "gopkg.in/yaml.v3"
)

// volcleanBackupDir mirrors the Python BACKUP_DIR (~/.config/stacks/volclean-backups).
func volcleanBackupDir() string {
	return filepath.Join(configDir(), "volclean-backups")
}

const volcleanDoc = `
stacks_volclean.py — strip UNUSED top-level named-volume declarations.

When a service is moved to a bind mount, its old top-level ` + "`volumes:`" + ` entry is
left orphaned — declared but referenced by nothing — yet Compose still tries to
create it. This finds declarations no service references and removes them, backing
up every file first.

SAFETY: a volume is only an orphan if BOTH (a) YAML analysis shows no service
mounts it, AND (b) its name does not appear as a mount source anywhere in the
file's text. A volume that is used is never removed. Removal is textual (only the
orphan's block is deleted; the rest of the file is untouched), and if the whole
top-level ` + "`volumes:`" + ` section becomes empty its header is removed too.

CLI:
    stacks_volclean.py report [--json]
    stacks_volclean.py clean [--auto] [stack ...]
`

// volcleanIsNamed: a volume mount source is a NAMED volume (not a bind mount/path).
func volcleanIsNamed(src string) bool {
	src = strings.TrimSpace(src)
	if src == "" {
		return false
	}
	for _, p := range []string{"/", ".", "~", "$"} {
		if strings.HasPrefix(src, p) {
			return false
		}
	}
	return true
}

// volcleanReferencedAsMount: textual safety net — does `name` appear as a mount
// SOURCE in a service?
func volcleanReferencedAsMount(text, name string) bool {
	n := regexp.QuoteMeta(name)
	pats := []string{
		`-\s*["']?` + n + `:`,                // short form:  - name:/path
		`source:\s*["']?` + n + `["']?(\s|$)`, // long form:  source: name
	}
	for _, p := range pats {
		re := regexp.MustCompile("(?m)" + p)
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// volcleanReadFile reads a file replacing invalid bytes loosely (Python used
// errors="replace"). Go strings tolerate arbitrary bytes, so a plain read is fine.
func volcleanReadFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// volcleanAnalyze returns (declared, used, orphans-sorted) for one compose file.
func volcleanAnalyze(path string) (declared map[string]bool, used map[string]bool, orphans []string) {
	declared = map[string]bool{}
	used = map[string]bool{}
	orphans = []string{}

	raw, err := volcleanReadFile(path)
	if err != nil {
		return declared, used, orphans
	}
	var data map[string]interface{}
	if err := yaml.Unmarshal([]byte(raw), &data); err != nil {
		return declared, used, orphans
	}
	if data == nil {
		return declared, used, orphans
	}

	topvols, ok := data["volumes"].(map[string]interface{})
	if !ok {
		// Python: data.get("volumes") or {}; if not a dict → empty.
		return declared, used, orphans
	}
	for k := range topvols {
		declared[k] = true
	}

	services, _ := data["services"].(map[string]interface{})
	for _, sv := range services {
		body, ok := sv.(map[string]interface{})
		if !ok {
			continue
		}
		vols, ok := body["volumes"].([]interface{})
		if !ok {
			continue
		}
		for _, v := range vols {
			switch vv := v.(type) {
			case string:
				src := strings.TrimSpace(strings.SplitN(vv, ":", 2)[0])
				if volcleanIsNamed(src) {
					used[src] = true
				}
			case map[string]interface{}:
				if s, ok := vv["source"]; ok && s != nil {
					srcStr := fmt.Sprintf("%v", s)
					if volcleanIsNamed(srcStr) {
						used[srcStr] = true
					}
				}
			}
		}
	}

	for n := range declared {
		if !used[n] && !volcleanReferencedAsMount(raw, n) {
			orphans = append(orphans, n)
		}
	}
	sort.Strings(orphans)
	return declared, used, orphans
}

// volcleanBackup copies path into the backup dir with a timestamp suffix.
func volcleanBackup(path string) (string, error) {
	dir := volcleanBackupDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, fmt.Sprintf("%s.%s.bak",
		filepath.Base(path), time.Now().Format("20060102-150405")))
	src, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer src.Close()
	out, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	if info, e := os.Stat(path); e == nil {
		_ = os.Chtimes(dst, time.Now(), info.ModTime())
		_ = os.Chmod(dst, info.Mode())
	}
	return dst, nil
}

var (
	volcleanReVolumesHdr  = regexp.MustCompile(`^volumes:\s*$`)
	volcleanReTopKey      = regexp.MustCompile(`^\S`)
	volcleanReEntry       = regexp.MustCompile(`^  ([A-Za-z0-9._-]+):`)
	volcleanReContinuation = regexp.MustCompile(`^    `)
	volcleanReHasEntry    = regexp.MustCompile(`^  \S`)
)

// volcleanStripOrphans removes orphan entries from the top-level volumes: section.
// Returns (removedCount, backupPath).
func volcleanStripOrphans(path string, orphans []string) (int, string) {
	if len(orphans) == 0 {
		return 0, ""
	}
	orphSet := map[string]bool{}
	for _, o := range orphans {
		orphSet[o] = true
	}
	raw, err := volcleanReadFile(path)
	if err != nil {
		return 0, ""
	}
	lines := strings.Split(raw, "\n")

	vstart := -1
	for i, l := range lines {
		if volcleanReVolumesHdr.MatchString(l) {
			vstart = i
			break
		}
	}
	if vstart == -1 {
		return 0, ""
	}

	vend := len(lines)
	for j := vstart + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "" {
			continue
		}
		if volcleanReTopKey.MatchString(lines[j]) { // next top-level key
			vend = j
			break
		}
	}

	section := lines[vstart+1 : vend]
	newSection := []string{}
	removed := 0
	i := 0
	for i < len(section) {
		m := volcleanReEntry.FindStringSubmatch(section[i])
		if m != nil && orphSet[m[1]] {
			i++
			for i < len(section) && volcleanReContinuation.MatchString(section[i]) { // 4+ space continuation
				i++
			}
			removed++
		} else {
			newSection = append(newSection, section[i])
			i++
		}
	}
	if removed == 0 {
		return 0, ""
	}

	bak, err := volcleanBackup(path)
	if err != nil {
		return 0, ""
	}

	hasEntry := false
	for _, l := range newSection {
		if volcleanReHasEntry.MatchString(l) {
			hasEntry = true
			break
		}
	}

	var rebuilt []string
	if hasEntry {
		rebuilt = append(rebuilt, lines[:vstart]...)
		rebuilt = append(rebuilt, lines[vstart])
		rebuilt = append(rebuilt, newSection...)
		rebuilt = append(rebuilt, lines[vend:]...)
	} else { // volumes section now empty → drop header
		rebuilt = append(rebuilt, lines[:vstart]...)
		rebuilt = append(rebuilt, lines[vend:]...)
	}
	out := strings.TrimRight(strings.Join(rebuilt, "\n"), "\n") + "\n"
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return 0, ""
	}
	return removed, bak
}

// volcleanRow holds one stack's scan result with orphans.
type volcleanRow struct {
	stack    string
	path     string
	declared map[string]bool
	used     map[string]bool
	orphans  []string
}

// volcleanGlobYaml lists sorted *.yml files in the stacks dir.
func volcleanGlobYaml() []string {
	matches, _ := filepath.Glob(filepath.Join(stacksDir(), "*.yml"))
	sort.Strings(matches)
	return matches
}

// volcleanScanAll returns rows for every stack with orphans.
func volcleanScanAll() []volcleanRow {
	rows := []volcleanRow{}
	for _, path := range volcleanGlobYaml() {
		declared, used, orphans := volcleanAnalyze(path)
		if len(orphans) > 0 {
			base := filepath.Base(path)
			stack := strings.TrimSuffix(base, ".yml")
			rows = append(rows, volcleanRow{stack, path, declared, used, orphans})
		}
	}
	return rows
}

// volcleanEnsureNamedDecls is the inverse of strip: add a top-level declaration
// for every NAMED volume a service references but that isn't declared.
// Returns (addedCount, backupPath).
func volcleanEnsureNamedDecls(path string) (int, string) {
	declared, used, _ := volcleanAnalyze(path)
	missing := []string{}
	for u := range used {
		if !declared[u] {
			missing = append(missing, u)
		}
	}
	sort.Strings(missing)
	if len(missing) == 0 {
		return 0, ""
	}

	bak, err := volcleanBackup(path)
	if err != nil {
		return 0, ""
	}
	raw, err := volcleanReadFile(path)
	if err != nil {
		return 0, ""
	}
	lines := strings.Split(raw, "\n")
	add := make([]string, 0, len(missing))
	for _, n := range missing {
		add = append(add, "  "+n+":")
	}

	vstart := -1
	for i, l := range lines {
		if volcleanReVolumesHdr.MatchString(l) {
			vstart = i
			break
		}
	}
	if vstart == -1 { // no volumes: section → append one
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, "volumes:")
		lines = append(lines, add...)
	} else { // insert at end of existing section
		vend := len(lines)
		for j := vstart + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "" {
				continue
			}
			if volcleanReTopKey.MatchString(lines[j]) {
				vend = j
				break
			}
		}
		newLines := []string{}
		newLines = append(newLines, lines[:vend]...)
		newLines = append(newLines, add...)
		newLines = append(newLines, lines[vend:]...)
		lines = newLines
	}
	out := strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return 0, ""
	}
	return len(missing), bak
}

// volcleanMissingRow holds a stack referencing undeclared named volumes.
type volcleanMissingRow struct {
	stack   string
	path    string
	missing []string
}

// volcleanScanMissing returns stacks referencing undeclared named volumes.
func volcleanScanMissing() []volcleanMissingRow {
	rows := []volcleanMissingRow{}
	for _, path := range volcleanGlobYaml() {
		declared, used, _ := volcleanAnalyze(path)
		missing := []string{}
		for u := range used {
			if !declared[u] {
				missing = append(missing, u)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			base := filepath.Base(path)
			stack := strings.TrimSuffix(base, ".yml")
			rows = append(rows, volcleanMissingRow{stack, path, missing})
		}
	}
	return rows
}

// volcleanMain is the CLI entry point (faithful port of main()).
func volcleanMain(args []string) {
	cmd := "report"
	if len(args) > 0 {
		cmd = args[0]
	}
	rows := volcleanScanAll()

	switch cmd {
	case "report":
		if inList(args, "--json") {
			obj := map[string][]string{}
			for _, r := range rows {
				obj[r.stack] = r.orphans
			}
			b, _ := json.MarshalIndent(obj, "", "  ")
			fmt.Println(string(b))
			return
		}
		if len(rows) == 0 {
			fmt.Println("✓ No unused top-level named volumes.")
			return
		}
		total := 0
		for _, r := range rows {
			total += len(r.orphans)
		}
		fmt.Printf("⚠ %d unused top-level named-volume declaration(s) in %d stack(s):\n\n", total, len(rows))
		for _, r := range rows {
			preview := r.orphans
			ellipsis := ""
			if len(preview) > 6 {
				preview = preview[:6]
				ellipsis = " …"
			}
			fmt.Printf("  %-14s %3d declared → %d unused: %s%s\n",
				r.stack, len(r.declared), len(r.orphans), strings.Join(preview, ", "), ellipsis)
		}
		fmt.Println("\nClean up:  stacks volclean clean          (interactive, per-stack)")
		fmt.Println("           stacks volclean clean --auto   (strip them all, with backups)")

	case "clean":
		if len(rows) == 0 {
			fmt.Println("✓ Nothing to clean.")
			return
		}
		auto := inList(args, "--auto")
		only := map[string]bool{}
		for _, a := range args[1:] {
			if !strings.HasPrefix(a, "-") {
				only[a] = true
			}
		}
		reader := bufio.NewReader(os.Stdin)
		total := 0
		for _, r := range rows {
			if len(only) > 0 && !only[r.stack] {
				continue
			}
			if !auto {
				fmt.Printf("\n%s: %d unused → %s\n", r.stack, len(r.orphans), strings.Join(r.orphans, ", "))
				fmt.Print("  strip these? [y/N/q]: ")
				line, _ := reader.ReadString('\n')
				ans := strings.ToLower(strings.TrimSpace(line))
				if ans == "q" {
					break
				}
				if ans != "y" {
					fmt.Println("  skipped.")
					continue
				}
			}
			n, bak := volcleanStripOrphans(r.path, r.orphans)
			total += n
			fmt.Printf("  ✓ %s: stripped %d  (backup: %s)\n", r.stack, n, bak)
		}
		fmt.Printf("\nDone — removed %d unused volume declaration(s). Backups in %s\n", total, volcleanBackupDir())

	case "ensure":
		miss := volcleanScanMissing()
		if len(miss) == 0 {
			fmt.Println("✓ Every referenced named volume is already declared.")
			return
		}
		only := map[string]bool{}
		for _, a := range args[1:] {
			if !strings.HasPrefix(a, "-") {
				only[a] = true
			}
		}
		total := 0
		for _, r := range miss {
			if len(only) > 0 && !only[r.stack] {
				continue
			}
			n, bak := volcleanEnsureNamedDecls(r.path)
			total += n
			fmt.Printf("  ✓ %s: added %d declaration(s)  (backup: %s)\n", r.stack, n, bak)
		}
		fmt.Printf("\nDone — added %d top-level volume declaration(s).\n", total)

	default:
		fmt.Print(volcleanDoc)
	}
}
