// fixdynamic.go — faithful Go port of stacks_fix_dynamic.py.
//
// Reconcile Traefik dynamic config files against the authoritative container
// names declared in the compose stacks.
//
// Two places in a dynamic reference a container name:
//  1. service backend URLs :  url: "http://<host>:<port>"
//  2. sablier middleware   :  names: "<c1>,<c2>"
//
// Rules (intentionally conservative — these are live routing files):
//   - IP-address hosts (e.g. 192.168.1.50) are LEFT ALONE.
//   - A token that already matches a real container_name is LEFT ALONE.
//   - A stale token is matched to a real name by separator-insensitive compare
//     (drop - _ . , lowercase). If EXACTLY ONE real name matches -> rewrite.
//   - Ambiguous (>1 match) or unmatched (orphan) tokens are LEFT ALONE and
//     REPORTED so the human can decide.
//
// A .bak-dynfix-<ts> backup is written before a file is changed.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// --- universal path helper (module-unique) ---------------------------------

// fixdynDynDir resolves the Traefik dynamics directory generically for any user.
// Priority: $DYNAMICS_DIR → conf DYNAMICS_DIR → $STACKS_DATA_DIR/Configs/Dynamics
// → ~/MyDocker/Configs/Dynamics.
func fixdynDynDir() string {
	if d := os.Getenv("DYNAMICS_DIR"); d != "" {
		return d
	}
	if d := confValue("DYNAMICS_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("STACKS_DATA_DIR"); d != "" {
		return filepath.Join(d, "Configs", "Dynamics")
	}
	return filepath.Join(home(), "MyDocker", "Configs", "Dynamics")
}

// --- regexes (mirror the Python module-level compiles) ---------------------

var (
	fixdynIPRE    = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)
	fixdynURLRE   = regexp.MustCompile(`(url:\s*["']?https?://)([A-Za-z0-9_.\-]+)(:\d+)`)
	fixdynNamesRE = regexp.MustCompile(`(names:\s*["'])([^"']+)(["'])`)
)

func fixdynNorm(name string) string {
	r := strings.NewReplacer("-", "", "_", "", ".", "")
	return strings.ToLower(r.Replace(name))
}

// fixdynBuildAuth returns (auth_set, norm_map) from every container_name in the
// stacks. norm_map maps separator-stripped form -> sorted list of real names.
func fixdynBuildAuth(stacksDir string) (map[string]bool, map[string][]string) {
	auth := map[string]bool{}
	cnRE := regexp.MustCompile(`container_name:\s*(\S+)`)
	files, _ := filepath.Glob(filepath.Join(stacksDir, "*.yml"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		txt := string(data)
		for _, m := range cnRE.FindAllStringSubmatch(txt, -1) {
			cn := strings.Trim(strings.Trim(strings.TrimSpace(m[1]), `"`), `'`)
			auth[cn] = true
		}
	}
	normMap := map[string][]string{}
	for a := range auth {
		k := fixdynNorm(a)
		normMap[k] = append(normMap[k], a)
	}
	for k := range normMap {
		sort.Strings(normMap[k])
	}
	return auth, normMap
}

// fixdynResolve returns (status, value).
// status: "ok" (already valid), "ip", "map" (value=new name),
//
//	"orphan" (no match), "ambiguous" (value=candidates joined by comma).
func fixdynResolve(token string, auth map[string]bool, normMap map[string][]string) (string, string) {
	if fixdynIPRE.MatchString(token) {
		return "ip", token
	}
	if auth[token] {
		return "ok", token
	}
	cands := normMap[fixdynNorm(token)]
	if len(cands) == 1 && cands[0] != token {
		return "map", cands[0]
	}
	if len(cands) > 1 {
		return "ambiguous", strings.Join(cands, ",")
	}
	return "orphan", token
}

type fixdynChange struct{ kind, old, new string }
type fixdynOrphan struct{ kind, token, detail string }

// fixdynFixText returns (new_text, changes, orphans).
func fixdynFixText(text string, auth map[string]bool, normMap map[string][]string) (string, []fixdynChange, []fixdynOrphan) {
	var changes []fixdynChange
	var orphans []fixdynOrphan

	text = fixdynURLRE.ReplaceAllStringFunc(text, func(s string) string {
		m := fixdynURLRE.FindStringSubmatch(s)
		host := m[2]
		st, val := fixdynResolve(host, auth, normMap)
		if st == "map" {
			changes = append(changes, fixdynChange{"url", host, val})
			return m[1] + val + m[3]
		}
		if st == "orphan" {
			orphans = append(orphans, fixdynOrphan{"url", host, "no container"})
		} else if st == "ambiguous" {
			orphans = append(orphans, fixdynOrphan{"url", host, "ambiguous: " + val})
		}
		return m[0]
	})

	text = fixdynNamesRE.ReplaceAllStringFunc(text, func(s string) string {
		m := fixdynNamesRE.FindStringSubmatch(s)
		toks := strings.Split(m[2], ",")
		var out []string
		for _, t := range toks {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			st, val := fixdynResolve(t, auth, normMap)
			if st == "map" {
				changes = append(changes, fixdynChange{"names", t, val})
				out = append(out, val)
			} else {
				if st == "orphan" {
					orphans = append(orphans, fixdynOrphan{"names", t, "no container"})
				} else if st == "ambiguous" {
					orphans = append(orphans, fixdynOrphan{"names", t, "ambiguous: " + val})
				}
				out = append(out, t)
			}
		}
		return m[1] + strings.Join(out, ",") + m[3]
	})

	return text, changes, orphans
}

type fixdynResult struct {
	path    string
	err     string
	changes []fixdynChange
	orphans []fixdynOrphan
	wrote   bool
}

func fixdynFixFile(path string, auth map[string]bool, normMap map[string][]string, dryRun bool) fixdynResult {
	data, err := os.ReadFile(path)
	if err != nil {
		return fixdynResult{path: path, err: err.Error()}
	}
	original := string(data)
	newText, changes, orphans := fixdynFixText(original, auth, normMap)
	wrote := false
	if len(changes) > 0 && newText != original && !dryRun {
		fixdynCopy2(path, fmt.Sprintf("%s.bak-dynfix-%d", path, time.Now().Unix()))
		os.WriteFile(path, []byte(newText), 0644)
		wrote = true
	}
	return fixdynResult{path: path, changes: changes, orphans: orphans, wrote: wrote}
}

// fixdynCopy2 mirrors shutil.copy2: copies contents and preserves mode/mtime.
func fixdynCopy2(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	mt := st.ModTime()
	return os.Chtimes(dst, mt, mt)
}

// fixdynFindTargets maps user tokens (stack names / dynamic stems / files) to
// dynamic paths.
func fixdynFindTargets(names []string, dynDir string) []string {
	globbed, _ := filepath.Glob(filepath.Join(dynDir, "*.yml"))
	sort.Strings(globbed)
	var allf []string
	for _, f := range globbed {
		if !strings.Contains(filepath.Base(f), ".bak") {
			allf = append(allf, f)
		}
	}
	if len(names) == 0 || (len(names) == 1 && names[0] == "all") {
		return allf
	}
	var out []string
	for _, tok := range names {
		b := filepath.Base(tok)
		// exact file
		cand := filepath.Join(dynDir, b)
		if fixdynIsFile(cand) && !strings.Contains(b, ".bak") {
			out = append(out, cand)
			continue
		}
		// stack name (ai_0) or stem (ai0) -> <stem>-*.yml
		stem := strings.ReplaceAll(strings.ReplaceAll(b, ".yml", ""), "_", "")
		var m []string
		for _, f := range allf {
			if strings.SplitN(filepath.Base(f), "-", 2)[0] == stem {
				m = append(m, f)
			}
		}
		if len(m) > 0 {
			out = append(out, m...)
		} else {
			fmt.Fprintf(os.Stderr, "  no dynamic for '%s'\n", tok)
		}
	}
	// de-dup preserve order
	seen := map[string]bool{}
	var uniq []string
	for _, f := range out {
		if !seen[f] {
			seen[f] = true
			uniq = append(uniq, f)
		}
	}
	return uniq
}

func fixdynIsFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// fixDynamicMain is the faithful port of stacks_fix_dynamic.main(argv).
func fixDynamicMain(argv []string) int {
	stacksDir := stacksDir()
	dynDir := fixdynDynDir()
	dry := inList(argv, "--dry-run")
	var names []string
	for _, a := range argv {
		if !strings.HasPrefix(a, "--") {
			names = append(names, a)
		}
	}

	auth, normMap := fixdynBuildAuth(stacksDir)
	if len(auth) == 0 {
		fmt.Printf("  \033[1;31m✘ no container_name found in %s\033[0m\n", stacksDir)
		return 1
	}
	targets := fixdynFindTargets(names, dynDir)
	if len(targets) == 0 {
		fmt.Println("  no matching dynamic files")
		return 0
	}

	tag := ""
	if dry {
		tag = "[dry-run] "
	}
	totalCh := 0
	var orphanLines []string
	for _, path := range targets {
		r := fixdynFixFile(path, auth, normMap, dry)
		b := filepath.Base(path)
		if r.err != "" {
			fmt.Printf("  \033[1;31m✘ %s: %s\033[0m\n", b, r.err)
			continue
		}
		if len(r.changes) > 0 {
			totalCh += len(r.changes)
			// verb is "would fix" when dry, otherwise "fixed" regardless of wrote.
			verb := "fixed"
			if dry {
				verb = "would fix"
			}
			fmt.Printf("  \033[1;32m✔ %s%s %s (%d)\033[0m\n", tag, verb, b, len(r.changes))
			for _, c := range r.changes {
				fmt.Printf("      %-6s %s -> %s\n", c.kind, c.old, c.new)
			}
		}
		for _, o := range r.orphans {
			orphanLines = append(orphanLines,
				fmt.Sprintf("  \033[1;33m⚠ %-22s %-6s %s (%s)\033[0m", b, o.kind, o.token, o.detail))
		}
	}

	if len(orphanLines) > 0 {
		fmt.Println("\n\033[1;33m── Orphans / unresolved (left untouched, review manually) ──\033[0m")
		for _, ln := range orphanLines {
			fmt.Println(ln)
		}
	}

	fmt.Printf("\n\033[1;36m%sTotal name fixes: %d across %d file(s); %d orphan ref(s)\033[0m\n",
		tag, totalCh, len(targets), len(orphanLines))
	return 0
}
