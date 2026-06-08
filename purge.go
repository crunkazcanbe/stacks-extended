// purge.go — faithful Go port of stacks_purge.py.
//
// Delete a SERVICE or a whole STACK and clean up after it. Beyond removing the
// container/stack, this strips the networks it used out of the PROVISIONER
// (core_*) stacks + every other stack's top-level declaration — but ONLY
// networks that no remaining real service still uses. Provisioner containers
// (name starts 'provisioner') attach to networks purely to create them, so they
// DON'T count as real users.
//
// Safe: dry-run by default (report what it WOULD do). apply makes changes, and
// every edited file is backed up to ~/.config/stacks/purge-backups first.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const purgeDoc = `
stacks_purge.py — delete a SERVICE or a whole STACK and clean up after it.

Beyond just removing the container/stack, this also strips the networks (and
named volumes) it used out of the PROVISIONER (core_*) stacks + every other
stack's top-level declaration — but ONLY networks/volumes that no remaining real
service still uses. Provisioner containers (name starts 'provisioner') attach to
networks purely to create them, so they DON'T count as real users.

Safe: dry-run by default (report what it WOULD do). ` + "`--apply`" + ` makes changes, and
every edited file is backed up to ~/.config/stacks/purge-backups first.

Usage:
  stacks_purge.py service <stack> <container> [--apply]
  stacks_purge.py stack   <stack>             [--apply]
`

func purgeBackupDir() string  { return filepath.Join(configDir(), "purge-backups") }
func purgeArchiveDir() string { return filepath.Join(configDir(), "removed-stacks") }

// top-level single-line net/vol decl:  "  name: { ... }"
var purgeTopRE = regexp.MustCompile(`^(  )([A-Za-z0-9_.-]+):\s*\{(.*)\}\s*$`)

var (
	purgeServicesRE  = regexp.MustCompile(`^services:`)
	purgeLeftRE      = regexp.MustCompile(`^\S`)
	purgeSvcKeyRE    = regexp.MustCompile(`^  ([A-Za-z0-9_.-]+):\s*$`)
	purgeCNameRE     = regexp.MustCompile(`^\s+container_name:\s*"?([A-Za-z0-9_.-]+)`)
	purgeNetsHdrRE   = regexp.MustCompile(`^    networks:`)
	purgeNetEntryRE1 = regexp.MustCompile(`^      ([A-Za-z0-9_.-]+):`)
	purgeNetEntryRE2 = regexp.MustCompile(`^      -\s*([A-Za-z0-9_.-]+)`)
	purgeSvcLvlRE    = regexp.MustCompile(`^    [A-Za-z]`)
	purgeNextSvcRE   = regexp.MustCompile(`^  \S`)
	purgeTopHdrRE    = regexp.MustCompile(`^(networks|volumes):`)
	purge6spaceRE    = regexp.MustCompile(`^      ([A-Za-z0-9_.-]+):`)
	purge8spaceRE    = regexp.MustCompile(`^        \S`)
)

// purgeServiceBlock mirrors a single (key, container_name, start, end) tuple.
type purgeServiceBlock struct {
	key   string
	cname string // "" == None
	start int
	end   int
}

func purgeStackFiles(includeExt bool) []string {
	matches, _ := filepath.Glob(filepath.Join(stacksDir(), "*.yml"))
	sort.Strings(matches)
	if includeExt {
		return matches
	}
	var out []string
	for _, f := range matches {
		if !strings.HasSuffix(f, "-ext.yml") {
			out = append(out, f)
		}
	}
	return out
}

func purgeBackup(f string) (string, error) {
	if err := os.MkdirAll(purgeBackupDir(), 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(purgeBackupDir(), fmt.Sprintf("%s.%s.bak", filepath.Base(f), time.Now().Format("20060102-150405")))
	data, err := os.ReadFile(f)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", err
	}
	return dst, nil
}

func purgeReadLines(f string) []string {
	data, err := os.ReadFile(f)
	if err != nil {
		return []string{}
	}
	return strings.Split(string(data), "\n")
}

func purgeWriteJoined(f string, lines []string) error {
	body := strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
	return os.WriteFile(f, []byte(body), 0o644)
}

// ── parsing ──────────────────────────────────────────────────────────────────

// purgeServiceBlocks yields (key, container_name, start, end) for each service
// in file f. start/end are line indices of the 2-space `  key:` block (end excl).
func purgeServiceBlocks(f string) ([]string, []purgeServiceBlock) {
	lines := purgeReadLines(f)
	inservices := false
	var blocks []purgeServiceBlock
	var cur *purgeServiceBlock
	for i, ln := range lines {
		if purgeServicesRE.MatchString(ln) {
			inservices = true
			continue
		}
		if purgeLeftRE.MatchString(ln) { // left margin → left services:
			if inservices && cur != nil {
				cur.end = i
				blocks = append(blocks, *cur)
				cur = nil
			}
			inservices = false
		}
		if !inservices {
			continue
		}
		if m := purgeSvcKeyRE.FindStringSubmatch(ln); m != nil {
			if cur != nil {
				cur.end = i
				blocks = append(blocks, *cur)
			}
			cur = &purgeServiceBlock{key: m[1], cname: "", start: i, end: 0}
		} else if cur != nil && cur.cname == "" {
			if cm := purgeCNameRE.FindStringSubmatch(ln); cm != nil {
				cur.cname = cm[1]
			}
		}
	}
	if cur != nil {
		cur.end = len(lines)
		blocks = append(blocks, *cur)
	}
	return lines, blocks
}

// purgeNetworksOfBlock returns networks listed in a service block's `networks:`
// section. Service keys are 4-space; network entries 6-space; children 8-space.
func purgeNetworksOfBlock(lines []string, start, end int) map[string]bool {
	nets := map[string]bool{}
	innet := false
	for _, ln := range lines[start:end] {
		if purgeNetsHdrRE.MatchString(ln) {
			innet = true
			continue
		}
		if innet {
			var name string
			if m := purgeNetEntryRE1.FindStringSubmatch(ln); m != nil {
				name = m[1]
			} else if m := purgeNetEntryRE2.FindStringSubmatch(ln); m != nil {
				name = m[1]
			}
			if name != "" {
				nets[name] = true
			} else if purgeSvcLvlRE.MatchString(ln) || purgeNextSvcRE.MatchString(ln) {
				innet = false // next service-level key or next service
			}
		}
	}
	return nets
}

// purgeNetworkUsers mirrors network_users(): {net: set('stack:service')} across
// all stacks, EXCLUDING provisioner* services. skip = set of "stack\x00cname"
// to ignore (the ones being deleted).
func purgeNetworkUsers(skip map[string]bool) map[string]map[string]bool {
	if skip == nil {
		skip = map[string]bool{}
	}
	users := map[string]map[string]bool{}
	for _, f := range purgeStackFiles(false) {
		stack := strings.TrimSuffix(filepath.Base(f), ".yml")
		lines, blocks := purgeServiceBlocks(f)
		for _, b := range blocks {
			if b.cname != "" && strings.HasPrefix(b.cname, "provisioner") {
				continue
			}
			if skip[purgeSkipKey(stack, b.cname)] {
				continue
			}
			for n := range purgeNetworksOfBlock(lines, b.start, b.end) {
				if users[n] == nil {
					users[n] = map[string]bool{}
				}
				who := b.cname
				if who == "" {
					who = b.key
				}
				users[n][stack+":"+who] = true
			}
		}
	}
	return users
}

func purgeSkipKey(stack, cname string) string { return stack + "\x00" + cname }

// purgeTopLevelDecls mirrors _toplevel_decls(): [(file, line_index)] where net
// is declared at top level (networks/volumes).
func purgeTopLevelDecls(net string) [][2]interface{} {
	var hits [][2]interface{}
	for _, f := range purgeStackFiles(false) {
		lines := purgeReadLines(f)
		intop := false
		for i, ln := range lines {
			if purgeTopHdrRE.MatchString(ln) {
				intop = true
				continue
			}
			if purgeLeftRE.MatchString(ln) {
				intop = false
			}
			if intop {
				if m := purgeTopRE.FindStringSubmatch(ln); m != nil && m[2] == net {
					hits = append(hits, [2]interface{}{f, i})
				}
			}
		}
	}
	return hits
}

// ── mutation ──────────────────────────────────────────────────────────────────

func purgeRemoveBlock(f string, start, end int) {
	lines := purgeReadLines(f)
	if start < 0 {
		start = 0
	}
	if end > len(lines) {
		end = len(lines)
	}
	lines = append(lines[:start], lines[end:]...)
	purgeWriteJoined(f, lines)
}

// purgeRemoveTopLevel removes every top-level declaration of net. Returns the
// list of basenames touched.
func purgeRemoveTopLevel(net string) []string {
	var touched []string
	for _, hit := range purgeTopLevelDecls(net) {
		f := hit[0].(string)
		lines := purgeReadLines(f)
		var newLines []string
		for _, ln := range lines {
			m := purgeTopRE.FindStringSubmatch(ln)
			if m != nil && m[2] == net {
				continue
			}
			newLines = append(newLines, ln)
		}
		if len(newLines) != len(lines) {
			purgeBackup(f)
			purgeWriteJoined(f, newLines)
			touched = append(touched, filepath.Base(f))
		}
	}
	return touched
}

// purgeStripFromProvisioners removes net from every provisioner service's
// networks: block. Returns basenames touched.
func purgeStripFromProvisioners(net string) []string {
	var touched []string
	for _, f := range purgeStackFiles(false) {
		lines := purgeReadLines(f)
		_, blocks := purgeServiceBlocks(f)
		type span struct{ s, e int }
		var prov []span
		for _, b := range blocks {
			if b.cname != "" && strings.HasPrefix(b.cname, "provisioner") {
				prov = append(prov, span{b.start, b.end})
			}
		}
		if len(prov) == 0 {
			continue
		}
		// first pass: mark dropped net-header lines with a sentinel
		const sentinel = "\x00__DROP_NET__\x00"
		var out []string
		for i, ln := range lines {
			inProv := false
			for _, p := range prov {
				if p.s <= i && i < p.e {
					inProv = true
					break
				}
			}
			if inProv {
				if m := purge6spaceRE.FindStringSubmatch(ln); m != nil && m[1] == net {
					out = append(out, sentinel)
					continue
				}
			}
			out = append(out, ln)
		}
		// second pass: drop 8-space children following a dropped net header
		var final []string
		dropping := false
		for _, ln := range out {
			if ln == sentinel {
				dropping = true
				continue
			}
			if dropping {
				if purge8spaceRE.MatchString(ln) { // 8-space child of the net
					continue
				}
				dropping = false
			}
			final = append(final, ln)
		}
		if !purgeSliceEqual(final, lines) {
			purgeBackup(f)
			purgeWriteJoined(f, final)
			touched = append(touched, filepath.Base(f))
		}
	}
	return touched
}

// purgeMove mirrors shutil.move: rename if possible, else copy+delete so it
// works across filesystems (archive dir may live on a different device).
func purgeMove(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return err
	}
	return os.Remove(src)
}

func purgeSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func purgeNetworks(orphans []string, apply bool, log *[]string) {
	sorted := append([]string(nil), orphans...)
	sort.Strings(sorted)
	for _, net := range sorted {
		if !apply {
			*log = append(*log, fmt.Sprintf("  would remove network '%s': top-level decls + provisioner refs + docker network", net))
			continue
		}
		t1 := purgeRemoveTopLevel(net)
		t2 := purgeStripFromProvisioners(net)
		// docker network rm only if it exists and is empty
		removedDocker := false
		for _, row := range networkTable() {
			if row.Name == net && row.Count == 0 {
				removedDocker = removeNetwork(row.ID)
				break
			}
		}
		decls := purgeUniqSorted(append(append([]string(nil), t1...), t2...))
		declStr := "none"
		if len(decls) > 0 {
			declStr = strings.Join(decls, ",")
		}
		dockerStr := ""
		if removedDocker {
			dockerStr = "; docker net rm"
		}
		*log = append(*log, fmt.Sprintf("  removed network '%s' (decls: %s%s)", net, declStr, dockerStr))
	}
}

func purgeUniqSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func purgeSortedSet(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// purgeFmtNetList mirrors `sorted(nets) or '[]'`.
func purgeFmtNetList(m map[string]bool) string {
	s := purgeSortedSet(m)
	if len(s) == 0 {
		return "[]"
	}
	return fmt.Sprintf("[%s]", purgePyList(s))
}

// purgePyList renders a slice the way Python prints a list of strings.
func purgePyList(s []string) string {
	parts := make([]string, len(s))
	for i, v := range s {
		parts[i] = "'" + v + "'"
	}
	return strings.Join(parts, ", ")
}

// ── public ops ────────────────────────────────────────────────────────────────

func purgeService(stack, container string, apply bool) []string {
	var log []string
	f := filepath.Join(stacksDir(), stack+".yml")
	if st, err := os.Stat(f); err != nil || st.IsDir() {
		return []string{"no such stack: " + stack}
	}
	lines, blocks := purgeServiceBlocks(f)
	var target *purgeServiceBlock
	for i := range blocks {
		if blocks[i].cname == container {
			target = &blocks[i]
			break
		}
	}
	if target == nil {
		return []string{fmt.Sprintf("service '%s' not found in %s", container, stack)}
	}
	svcNets := purgeNetworksOfBlock(lines, target.start, target.end)
	log = append(log, fmt.Sprintf("service %s (key %s) in %s: networks %s", container, target.key, stack, purgeFmtNetList(svcNets)))
	// who still uses those nets once this service is gone?
	users := purgeNetworkUsers(map[string]bool{purgeSkipKey(stack, container): true})
	var orphans, keep []string
	for n := range svcNets {
		if users[n] == nil {
			orphans = append(orphans, n)
		} else {
			keep = append(keep, n)
		}
	}
	if len(keep) > 0 {
		sort.Strings(keep)
		log = append(log, "  kept (still used): "+strings.Join(keep, ", "))
	}
	if apply {
		purgeBackup(f)
		purgeRemoveBlock(f, target.start, target.end)
		log = append(log, fmt.Sprintf("  removed service block from %s.yml", stack))
		removeContainer(container, true, false)
		log = append(log, "  docker rm "+container)
	} else {
		log = append(log, fmt.Sprintf("  would remove service block from %s.yml + docker rm %s", stack, container))
	}
	purgeNetworks(orphans, apply, &log)
	return log
}

func purgeStackOp(stack string, apply bool) []string {
	var log []string
	f := filepath.Join(stacksDir(), stack+".yml")
	if st, err := os.Stat(f); err != nil || st.IsDir() {
		return []string{"no such stack: " + stack}
	}
	lines, blocks := purgeServiceBlocks(f)
	var real []purgeServiceBlock
	for _, b := range blocks {
		if b.cname != "" && !strings.HasPrefix(b.cname, "provisioner") {
			real = append(real, b)
		}
	}
	allNets := map[string]bool{}
	for _, b := range real {
		for n := range purgeNetworksOfBlock(lines, b.start, b.end) {
			allNets[n] = true
		}
	}
	skip := map[string]bool{}
	for _, b := range real {
		skip[purgeSkipKey(stack, b.cname)] = true
	}
	users := purgeNetworkUsers(skip)
	var orphans, keep []string
	for n := range allNets {
		if users[n] == nil {
			orphans = append(orphans, n)
		} else {
			keep = append(keep, n)
		}
	}
	log = append(log, fmt.Sprintf("stack %s: %d service(s); networks %s", stack, len(real), purgeFmtNetList(allNets)))
	if len(keep) > 0 {
		sort.Strings(keep)
		log = append(log, "  kept (used elsewhere): "+strings.Join(keep, ", "))
	}
	if apply {
		// down -v + archive the file
		cli("compose", "-p", stack, "--project-directory", stacksDir(), "-f", f, "down", "-v")
		os.MkdirAll(purgeArchiveDir(), 0o755)
		dst := filepath.Join(purgeArchiveDir(), fmt.Sprintf("%s.yml.%s", stack, time.Now().Format("20060102-150405")))
		purgeMove(f, dst)
		log = append(log, fmt.Sprintf("  down -v + archived %s.yml", stack))
	} else {
		log = append(log, fmt.Sprintf("  would down -v + archive %s.yml", stack))
	}
	purgeNetworks(orphans, apply, &log)
	return log
}

// purgeMain mirrors the __main__ entrypoint: argv-style dispatch, prints output.
func purgeMain(argv []string) {
	apply := inList(argv, "--apply")
	var a []string
	for _, x := range argv {
		if x != "--apply" {
			a = append(a, x)
		}
	}
	var out []string
	if len(a) >= 3 && a[0] == "service" {
		out = purgeService(a[1], a[2], apply)
	} else if len(a) >= 2 && a[0] == "stack" {
		out = purgeStackOp(a[1], apply)
	} else {
		out = []string{purgeDoc}
	}
	fmt.Println(strings.Join(out, "\n"))
}
