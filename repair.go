// repair.go — faithful Go port of stacks_repair.py.
//
// Deep compose file repair based on learned patterns from dev_1.yml.
// Fixes structural corruption, missing keys, bad indentation, and injection
// artifacts. Called by `stacks fix` as Phase 0.5 corruption repair.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── Templates learned from dev_1.yml (perfect reference file) ────────────────
var repairTemplates = map[string]string{
	"blkio_config": "    blkio_config: {weight: 500, device_read_bps: [{path: /dev/nvme0n1, rate: 500mb}], device_write_bps: [{path: /dev/nvme0n1, rate: 500mb}]}",
	"ulimits":      "    ulimits: {memlock: {soft: -1, hard: -1}, nofile: {soft: 65535, hard: 65535}, nproc: 65535}",
	"storage_opt":  "    storage_opt: {size: 10G}",
	"deploy":       "    deploy: {placement: {constraints: [node.labels.priority == high]}, resources: {limits: {memory: 1G, cpus: '0.2', pids: 1000}, reservations: {memory: 100M, cpus: '0.05'}}}",
}

const (
	repairLabelIndent   = "      " // 6 spaces
	repairServiceIndent = "  "     // 2 spaces
)

var repairNetworkPriorities = map[string]int{"traefik_net": 1000}

const repairDefaultNetPriority = 500

// _states_health → repairStatesHealth.
// {name: (status, health)} for every container in ONE Docker API call.
type repairStateHealth struct {
	status string
	health string
}

func repairStatesHealth() map[string]repairStateHealth {
	out := map[string]repairStateHealth{}
	for n, i := range containerInfo() {
		out[n] = repairStateHealth{status: i.State, health: i.Health}
	}
	return out
}

// snapConf mirrors _snap_conf().
type repairSnapConf struct {
	dir       string
	keep      int
	require   string
	onSuccess bool
	use       bool
}

// repairConfPath returns the global_inject.conf path (universal).
func repairConfPath() string {
	return filepath.Join(configDir(), "global_inject.conf")
}

// repairReadConf reads a simple KEY=VALUE conf file (skipping blanks/comments).
func repairReadConf(path string) map[string]string {
	cfg := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		cfg[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	return cfg
}

func repairBoolish(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "1" || s == "true"
}

func _snap_conf() repairSnapConf {
	cfg := repairReadConf(repairConfPath())
	// YAML master overlay
	for k, v := range loadNamed("global_inject") {
		cfg[k] = v
	}
	get := func(k, def string) string {
		if v, ok := cfg[k]; ok {
			return v
		}
		return def
	}
	keep := 5
	if v := strings.TrimSpace(get("SNAPSHOT_KEEP", "5")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			keep = n
		}
	}
	dir := get("SNAPSHOT_DIR", filepath.Join(configDir(), "snapshots"))
	dir = repairExpandUser(dir)
	return repairSnapConf{
		dir:       dir,
		keep:      keep,
		require:   get("SNAPSHOT_REQUIRE", "none-failed"),
		onSuccess: repairBoolish(get("SNAPSHOT_ON_SUCCESS", "1")),
		use:       repairBoolish(get("REPAIR_USE_SNAPSHOT", "1")),
	}
}

// repairExpandUser approximates os.path.expanduser for leading ~ / ~user.
func repairExpandUser(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	// ~loveiznothin/... or ~/...  -> map any leading ~user onto the universal home.
	rest := p[1:]
	if rest == "" {
		return home()
	}
	if strings.HasPrefix(rest, "/") {
		return filepath.Join(home(), rest[1:])
	}
	// ~user/path — strip the username component, splice onto home().
	if idx := strings.Index(rest, "/"); idx >= 0 {
		return filepath.Join(home(), rest[idx+1:])
	}
	return home()
}

// _validate — True if `docker compose config` succeeds.
func _validate(path string) bool {
	return cli("compose", "-f", path, "config").exitCode == 0
}

// _stack_services
func _stack_services(path string) []string {
	r := cli("compose", "-f", path, "config", "--services")
	if r.exitCode != 0 {
		return nil
	}
	var out []string
	for _, x := range strings.Fields(r.stdout) {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

// _stack_state_ok
func _stack_state_ok(path, require string) bool {
	svcs := _stack_services(path)
	if len(svcs) == 0 {
		return false
	}
	bad := map[string]bool{"restarting": true, "dead": true, "removing": true}
	info := repairStatesHealth()
	for _, svc := range svcs {
		sh, ok := info[svc]
		if !ok {
			if require == "all-healthy" {
				return false
			}
			continue // not created = sleeping/Sablier, OK for none-failed
		}
		status, health := sh.status, sh.health
		if bad[status] {
			return false
		}
		if status != "running" {
			if require == "all-healthy" {
				return false
			}
			continue
		}
		// running:
		if require == "all-healthy" && health != "" && health != "healthy" {
			return false
		}
	}
	return true
}

// snapshot_if_proven
func snapshot_if_proven(path string) string {
	c := _snap_conf()
	if !c.onSuccess {
		return ""
	}
	if !_validate(path) {
		return ""
	}
	if !_stack_state_ok(path, c.require) {
		return ""
	}
	os.MkdirAll(c.dir, 0o755)
	stack := repairStripExt(filepath.Base(path))
	snap := filepath.Join(c.dir, fmt.Sprintf("%s.good.%d", stack, time.Now().Unix()))
	repairCopy2(path, snap)
	repairPruneSnapshots(c.dir, stack, c.keep)
	return snap
}

// _snapshots_for — this stack's .good snapshots, newest first.
func _snapshots_for(stack string) []string {
	c := _snap_conf()
	g, _ := filepath.Glob(filepath.Join(c.dir, fmt.Sprintf("%s.good.*", stack)))
	sort.Sort(sort.Reverse(sort.StringSlice(g)))
	return g
}

// _deploy_health_ok
func _deploy_health_ok(path, require string, settle int) bool {
	svcs := _stack_services(path)
	if len(svcs) == 0 {
		return false
	}
	settleN := settle
	if settleN < 1 {
		settleN = 1
	}
	deadline := time.Now().Add(time.Duration(settleN) * time.Second)
	bad := map[string]bool{"restarting": true, "dead": true, "removing": true}
	for {
		pending := false
		ok := true
		info := repairStatesHealth()
		for _, svc := range svcs {
			sh, exists := info[svc]
			if !exists {
				continue // not created — not part of what came up
			}
			status, health := sh.status, sh.health
			if bad[status] {
				ok = false
				break
			}
			if status != "running" {
				continue
			}
			if health == "starting" {
				pending = true
			} else if health == "unhealthy" {
				if require == "all-healthy" {
					ok = false
					break
				}
			}
		}
		if !ok {
			return false
		}
		if !pending || !time.Now().Before(deadline) {
			return ok
		}
		time.Sleep(2 * time.Second)
	}
}

// _save_snapshot
func _save_snapshot(path string, c repairSnapConf) string {
	os.MkdirAll(c.dir, 0o755)
	stack := repairStripExt(filepath.Base(path))
	snap := filepath.Join(c.dir, fmt.Sprintf("%s.good.%d", stack, time.Now().Unix()))
	repairCopy2(path, snap)
	repairPruneSnapshots(c.dir, stack, c.keep)
	return snap
}

// snapshot_after_up
func snapshot_after_up(path string) string {
	c := _snap_conf()
	if !c.onSuccess || !_validate(path) {
		return ""
	}
	settle := _snap_conf_int("SNAPSHOT_SETTLE_SECS", 15)
	if _deploy_health_ok(path, c.require, settle) {
		return _save_snapshot(path, c)
	}
	return ""
}

// _snap_conf_int
func _snap_conf_int(key string, def int) int {
	// YAML master first.
	if v, ok := loadNamed("global_inject")[key]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	// then the .conf scan
	data, err := os.ReadFile(repairConfPath())
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, key+"=") {
				v := strings.TrimSpace(strings.SplitN(line, "=", 2)[1])
				if n, err := strconv.Atoi(v); err == nil {
					return n
				}
			}
		}
	}
	return def
}

// repairStripExt removes .yml/.yaml.
func repairStripExt(name string) string {
	name = strings.ReplaceAll(name, ".yml", "")
	name = strings.ReplaceAll(name, ".yaml", "")
	return name
}

// repairCopy2 copies a file preserving mode (best-effort, like shutil.copy2).
func repairCopy2(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(src); err == nil {
		mode = fi.Mode()
	}
	if err := os.WriteFile(dst, data, mode); err != nil {
		return err
	}
	if fi, err := os.Stat(src); err == nil {
		os.Chtimes(dst, time.Now(), fi.ModTime())
	}
	return nil
}

// repairPruneSnapshots keeps the newest N "<stack>.good.*" files.
func repairPruneSnapshots(dir, stack string, keep int) {
	existing, _ := filepath.Glob(filepath.Join(dir, fmt.Sprintf("%s.good.*", stack)))
	sort.Strings(existing)
	if len(existing) <= keep {
		return
	}
	for _, old := range existing[:len(existing)-keep] {
		os.Remove(old)
	}
}

// repair_file — run all repair passes on a single compose file. Returns fixes.
func repair_file(path string, dryRun bool) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	content := string(data)
	original := content
	var fixes []string

	var f []string

	content, f = fix_corrupt_blkio(content)
	fixes = append(fixes, f...)

	content, f = fix_labels_in_networks(content)
	fixes = append(fixes, f...)

	content, f = fix_duplicate_labels(content)
	fixes = append(fixes, f...)

	content, f = fix_missing_closing_quotes(content)
	fixes = append(fixes, f...)

	content, f = fix_n_labels(content)
	fixes = append(fixes, f...)

	content, f = fix_name_field(content, path)
	fixes = append(fixes, f...)

	// ── Structural passes (dedup + phantom depends_on) ──
	content, f = fix_duplicate_service_keys(content)
	fixes = append(fixes, f...)

	content, f = fix_undefined_depends(content)
	fixes = append(fixes, f...)

	content, f = fix_dependency_cycles(content)
	fixes = append(fixes, f...)

	content, f = fix_network_form(content)
	fixes = append(fixes, f...)

	content, f = fix_undefined_networks(content)
	fixes = append(fixes, f...)

	// orphan network removal — gated on FIX_REMOVE_ORPHANS (default off)
	removeOrphans := false
	answered := false
	if ro, ok := configLoad()["FIX_REMOVE_ORPHANS"]; ok {
		v := strings.Trim(strings.TrimSpace(ro), "\"")
		removeOrphans = v == "1" || v == "on" || v == "true" || v == "True"
		answered = true // YAML answered; skip the .conf scan below
	}
	if !answered {
		confPath := filepath.Join(configDir(), "stacks.conf")
		if cdata, err := os.ReadFile(confPath); err == nil {
			for _, line := range strings.Split(string(cdata), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "FIX_REMOVE_ORPHANS=") {
					v := strings.Trim(strings.TrimSpace(strings.SplitN(strings.TrimSpace(line), "=", 2)[1]), "\"")
					removeOrphans = v == "1" || v == "on" || v == "true" || v == "True"
				}
			}
		}
	}
	if removeOrphans {
		content, f = fix_orphan_networks(content)
		fixes = append(fixes, f...)
	}

	if !dryRun && content != original {
		// back up the broken file before writing the repaired version
		bdir := filepath.Join(configDir(), "snapshots", "repair-backups")
		if os.MkdirAll(bdir, 0o755) == nil {
			stack := filepath.Base(path)
			repairCopy2(path, filepath.Join(bdir, fmt.Sprintf("%s.broken.%d", stack, time.Now().Unix())))
		}
		os.WriteFile(path, []byte(content), 0o644)
	}

	return fixes
}

// ── individual fixers ────────────────────────────────────────────────────────

var reBlkio = regexp.MustCompile(`device_read_bps:\s*\[[^\]]*(?:CMD|NONE|SHELL)[^\]]*\]`)

func fix_corrupt_blkio(content string) (string, []string) {
	var fixes []string
	if reBlkio.MatchString(content) {
		content = reBlkio.ReplaceAllString(content, "device_read_bps: [{path: /dev/nvme0n1, rate: 500mb}]")
		fixes = append(fixes, "corrupt_blkio: HC test leaked into blkio_config")
	}
	return content, fixes
}

var (
	reNetworksTop = regexp.MustCompile(`^networks:\s*$`)
	reTopLevel    = regexp.MustCompile(`^[a-zA-Z\[]`)
	reLabelInNet  = regexp.MustCompile(`^\s+- "(traefik\.|sablier\.)`)
)

func fix_labels_in_networks(content string) (string, []string) {
	var fixes []string
	lines := strings.Split(content, "\n")
	var result []string
	inNetworks := false

	for _, line := range lines {
		if reNetworksTop.MatchString(line) {
			inNetworks = true
		} else if reTopLevel.MatchString(line) && !strings.HasPrefix(line, " ") {
			inNetworks = false
		}

		if inNetworks && reLabelInNet.MatchString(line) {
			fixes = append(fixes, fmt.Sprintf(`labels_in_networks: removed "%s"`, strings.TrimSpace(line)))
			continue
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n"), fixes
}

var (
	reSvcKey2     = regexp.MustCompile(`^  [a-zA-Z0-9_-]+:\s*$`)
	reLabelsBlock = regexp.MustCompile(`^\s+labels:\s*$`)
)

func fix_duplicate_labels(content string) (string, []string) {
	var fixes []string
	lines := strings.Split(content, "\n")
	var result []string
	seenLabels := map[string]bool{}
	inLabels := false

	for _, line := range lines {
		// Reset on new service
		if reSvcKey2.MatchString(line) {
			inLabels = false
			seenLabels = map[string]bool{}
		}

		if reLabelsBlock.MatchString(line) {
			inLabels = true
			seenLabels = map[string]bool{}
			result = append(result, line)
			continue
		}

		if inLabels {
			if !strings.HasPrefix(strings.TrimSpace(line), "-") {
				inLabels = false
			} else {
				stripped := strings.TrimSpace(line)
				if strings.Contains(stripped, "traefik.enable=") ||
					strings.Contains(stripped, "sablier.enable=") ||
					strings.Contains(stripped, "sablier.group=") {
					if seenLabels[stripped] {
						fixes = append(fixes, "duplicate_label: removed duplicate "+stripped)
						continue
					}
					seenLabels[stripped] = true
				}
			}
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n"), fixes
}

var reSablierGroup = regexp.MustCompile(`sablier\.group=([a-zA-Z0-9_-]+)`)

func fix_missing_closing_quotes(content string) (string, []string) {
	var fixes []string
	lines := strings.Split(content, "\n")
	var result []string
	for _, line := range lines {
		if strings.Contains(line, "sablier.group=") {
			m := reSablierGroup.FindStringSubmatch(line)
			if m != nil {
				val := m[1]
				expected := fmt.Sprintf(`sablier.group=%s"`, val)
				if !strings.Contains(line, expected) {
					re := regexp.MustCompile(`sablier\.group=` + regexp.QuoteMeta(val) + `([^a-zA-Z0-9_"-]|$)`)
					line = re.ReplaceAllString(line, fmt.Sprintf(`sablier.group=%s"$1`, val))
					fixes = append(fixes, "missing_quote: fixed sablier.group="+val)
				}
			}
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n"), fixes
}

var reSingleLetterLabel = regexp.MustCompile(`^\s+- "[a-z]"\s*$`)

func fix_n_labels(content string) (string, []string) {
	var fixes []string
	lines := strings.Split(content, "\n")
	var result []string
	for _, line := range lines {
		if reSingleLetterLabel.MatchString(line) {
			fixes = append(fixes, "corrupt_label: removed "+strings.TrimSpace(line))
			continue
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n"), fixes
}

var reNameField = regexp.MustCompile(`^name:\s*`)

func fix_name_field(content, path string) (string, []string) {
	var fixes []string
	stackName := repairStripExt(filepath.Base(path))
	lines := strings.Split(content, "\n")

	// Remove any existing name: lines (check first 5 for the correct one)
	hasCorrect := false
	limit := len(lines)
	if limit > 5 {
		limit = 5
	}
	for _, l := range lines[:limit] {
		if l == "name: "+stackName {
			hasCorrect = true
			break
		}
	}
	if hasCorrect {
		return content, fixes
	}

	var kept []string
	for _, l := range lines {
		if reNameField.MatchString(l) {
			continue
		}
		kept = append(kept, l)
	}
	lines = kept

	// Insert after leading comments
	insertPos := 0
	for i, line := range lines {
		if strings.HasPrefix(line, "#") {
			insertPos = i + 1
		} else {
			break
		}
	}

	lines = insertAt(lines, insertPos, "name: "+stackName)
	fixes = append(fixes, "name_field: set to "+stackName)
	return strings.Join(lines, "\n"), fixes
}

var (
	reSvcNetworksBlock = regexp.MustCompile(`^    networks:\s*$`)
	reNetListItem      = regexp.MustCompile(`^      -\s+"?([a-zA-Z0-9_.-]+)"?\s*$`)
	reNetMapKey        = regexp.MustCompile(`^      ([a-zA-Z0-9_.-]+):\s*$`)
	reNetChild8        = regexp.MustCompile(`^        \S`)
	reSixIndent        = regexp.MustCompile(`^      `)
)

func fix_network_form(content string) (string, []string) {
	var fixes []string
	lines := strings.Split(content, "\n")
	var out []string
	i := 0
	n := len(lines)
	for i < n {
		l := lines[i]
		if reSvcNetworksBlock.MatchString(l) {
			out = append(out, l)
			i++
			var nets []string // preserve order, dedupe
			seen := map[string]bool{}
			mixed := false
			sawList := false
			sawMap := false
			for i < n {
				lm := reNetListItem.FindStringSubmatch(lines[i])
				mm := reNetMapKey.FindStringSubmatch(lines[i])
				if lm != nil {
					sawList = true
					net := lm[1]
					if !seen[net] {
						seen[net] = true
						nets = append(nets, net)
					}
					i++
				} else if mm != nil {
					sawMap = true
					net := mm[1]
					if !seen[net] {
						seen[net] = true
						nets = append(nets, net)
					}
					i++
					// skip its child lines (priority etc, 8-space)
					for i < n && reNetChild8.MatchString(lines[i]) {
						i++
					}
				} else if reSixIndent.MatchString(lines[i]) {
					i++ // stray indented line, skip
				} else {
					break
				}
			}
			if sawList && sawMap {
				mixed = true
			}
			// rebuild in mapping form
			for _, net := range nets {
				pri := repairDefaultNetPriority
				if net == "traefik_net" {
					pri = 1000
				}
				out = append(out, fmt.Sprintf("      %s:", net))
				out = append(out, fmt.Sprintf("        priority: %d", pri))
			}
			if mixed {
				fixes = append(fixes, "network_form: normalized mixed list/mapping networks block to mapping form")
			} else if sawList {
				fixes = append(fixes, "network_form: converted list-form networks to mapping form")
			}
			continue
		}
		out = append(out, l)
		i++
	}
	return strings.Join(out, "\n"), fixes
}

var (
	reTopLevelAlpha = regexp.MustCompile(`^[a-zA-Z]`)
	reTopNetKey     = regexp.MustCompile(`^  ([a-zA-Z0-9_.-]+):`)
	reUsedMapKey    = regexp.MustCompile(`(?m)^      ([a-zA-Z0-9_.-]+):`)
	reUsedListItem  = regexp.MustCompile(`(?m)^\s+-\s+"?([a-zA-Z0-9_.-]+)"?\s*$`)
)

func fix_orphan_networks(content string) (string, []string) {
	var fixes []string
	lines := strings.Split(content, "\n")
	// collect top-level declared nets (under 'networks:')
	declared := map[string]int{}
	inNet := false
	for idx, line := range lines {
		if reNetworksTop.MatchString(line) {
			inNet = true
			continue
		}
		if reTopLevelAlpha.MatchString(line) && !strings.HasPrefix(line, " ") {
			inNet = false
		}
		if inNet {
			m := reTopNetKey.FindStringSubmatch(line)
			if m != nil {
				declared[m[1]] = idx
			}
		}
	}
	// collect nets referenced anywhere in a service networks block (6-space) or list form
	used := map[string]bool{}
	for _, m := range reUsedMapKey.FindAllStringSubmatch(content, -1) {
		used[m[1]] = true
	}
	for _, m := range reUsedListItem.FindAllStringSubmatch(content, -1) {
		used[m[1]] = true
	}
	// never remove traefik_net (universal) even if it looks unused
	protected := map[string]bool{"traefik_net": true}
	orphans := map[string]bool{}
	for net := range declared {
		if !used[net] && !protected[net] && strings.HasSuffix(net, "_net") {
			orphans[net] = true
		}
	}
	if len(orphans) == 0 {
		return content, fixes
	}
	// remove each orphan's declaration line (handles one-line {..} form)
	var out []string
	for _, line := range lines {
		m := reTopNetKey.FindStringSubmatch(line)
		if m != nil && orphans[m[1]] {
			fixes = append(fixes, fmt.Sprintf("orphan_network: removed unused '%s'", m[1]))
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n"), fixes
}

var (
	reNetKey6     = regexp.MustCompile(`^      ([a-zA-Z0-9_.-]+):\s*$`)
	reNetKey6Inl  = regexp.MustCompile(`^      ([a-zA-Z0-9_.-]+):\s*\{`)
	reFourNonWS   = regexp.MustCompile(`^    \S`)
	reTwoNonWS    = regexp.MustCompile(`^  \S`)
	reZeroNonWS   = regexp.MustCompile(`^\S`)
	reSvcNetEntry = regexp.MustCompile(`^(    )networks:\s*$`)
)

func fix_undefined_networks(content string) (string, []string) {
	var fixes []string
	lines := strings.Split(content, "\n")
	// defined top-level networks
	defined := map[string]bool{}
	inNet := false
	for _, line := range lines {
		if reNetworksTop.MatchString(line) {
			inNet = true
			continue
		}
		if reTopLevelAlpha.MatchString(line) && !strings.HasPrefix(line, " ") {
			inNet = false
		}
		if inNet {
			m := reTopNetKey.FindStringSubmatch(line)
			if m != nil {
				defined[m[1]] = true
			}
		}
	}
	if !defined["traefik_net"] {
		return content, fixes // safety
	}

	var out []string
	i := 0
	n := len(lines)
	for i < n {
		line := lines[i]
		if !reSvcNetEntry.MatchString(line) {
			out = append(out, line)
			i++
			continue
		}
		// entering a service-level networks: block (4-space)
		out = append(out, line)
		i++
		keptAny := false
		for i < n {
			nl := reNetKey6.FindStringSubmatch(lines[i])
			nlInline := reNetKey6Inl.FindStringSubmatch(lines[i])
			if nl != nil || nlInline != nil {
				var net string
				if nl != nil {
					net = nl[1]
				} else {
					net = nlInline[1]
				}
				if defined[net] {
					out = append(out, lines[i])
					i++
					keptAny = true
					// keep its child lines (8-space) if block form
					if nl != nil {
						for i < n && reNetChild8.MatchString(lines[i]) {
							out = append(out, lines[i])
							i++
						}
					}
				} else {
					fixes = append(fixes, fmt.Sprintf("undefined_network: removed '%s' from service", net))
					i++
					// skip its child lines (8-space)
					for i < n && reNetChild8.MatchString(lines[i]) {
						i++
					}
				}
			} else if reFourNonWS.MatchString(lines[i]) || reTwoNonWS.MatchString(lines[i]) || reZeroNonWS.MatchString(lines[i]) {
				break // left the networks block
			} else {
				out = append(out, lines[i])
				i++
			}
		}
		if !keptAny {
			out = append(out, "      traefik_net:")
			out = append(out, "        priority: 1000")
			fixes = append(fixes, "undefined_network: service left networkless -> added traefik_net")
		}
	}
	return strings.Join(out, "\n"), fixes
}

var (
	reSvcKeyFull = regexp.MustCompile(`^  ([a-zA-Z0-9_.+-]+):\s*$`)
	reDependsOn4 = regexp.MustCompile(`^    depends_on:\s*$`)
	reDepEntry   = regexp.MustCompile(`^      -\s+(["']?)([a-zA-Z0-9_.+-]+)["']?\s*$`)
)

func fix_dependency_cycles(content string) (string, []string) {
	var fixes []string
	lines := strings.Split(content, "\n")
	// map each service -> set of deps, and remember line index of each dep entry
	graph := map[string]map[string]bool{}
	depLines := map[[2]string]int{} // (svc, dep) -> line index
	// preserve insertion order to match Python's `list(graph)` (dict insertion order)
	var svcOrder []string
	depOrder := map[string][]string{} // svc -> deps in insertion order
	cur := ""
	inDep := false
	for i, line := range lines {
		m := reSvcKeyFull.FindStringSubmatch(line)
		if m != nil {
			cur = m[1]
			if graph[cur] == nil {
				graph[cur] = map[string]bool{}
				svcOrder = append(svcOrder, cur)
			}
			inDep = false
			continue
		}
		if cur == "" {
			continue
		}
		if reDependsOn4.MatchString(line) {
			inDep = true
			continue
		}
		if inDep {
			dm := reDepEntry.FindStringSubmatch(line)
			if dm != nil {
				if !graph[cur][dm[2]] {
					depOrder[cur] = append(depOrder[cur], dm[2])
				}
				graph[cur][dm[2]] = true
				depLines[[2]string{cur, dm[2]}] = i
			} else {
				inDep = false
			}
		}
	}
	// find 2-node cycles (iterate services in file order, like Python's list(graph))
	remove := map[int]bool{} // line indices to drop
	for _, a := range svcOrder {
		for _, b := range depOrder[a] {
			if !graph[a][b] {
				continue // edge already removed
			}
			if graph[b] != nil && graph[b][a] {
				// cycle a<->b. Drop the edge from the service with MORE deps.
				var victim, other string
				if len(graph[a]) >= len(graph[b]) {
					victim, other = a, b
				} else {
					victim, other = b, a
				}
				key := [2]string{victim, other}
				if idx, ok := depLines[key]; ok {
					remove[idx] = true
					delete(graph[victim], other)
					fixes = append(fixes, fmt.Sprintf("dependency_cycle: removed '%s' from %s.depends_on", other, victim))
				}
			}
		}
	}
	if len(remove) == 0 {
		return content, fixes
	}
	var newLines []string
	for i, l := range lines {
		if !remove[i] {
			newLines = append(newLines, l)
		}
	}
	return strings.Join(newLines, "\n"), fixes
}

var (
	reServicesTop  = regexp.MustCompile(`^services:\s*$`)
	reDependsAny   = regexp.MustCompile(`^(\s+)depends_on:\s*$`)
	reDepEntryAny  = regexp.MustCompile(`^\s+-\s+(["']?)([a-zA-Z0-9_.+-]+)["']?\s*$`)
	reDependsInline = regexp.MustCompile(`^(\s+)depends_on:\s*\[(.*)\]\s*$`)
)

func fix_undefined_depends(content string) (string, []string) {
	var fixes []string
	lines := strings.Split(content, "\n")
	// collect all defined service names (2-space indent, under services:)
	defined := map[string]bool{}
	inServices := false
	for _, line := range lines {
		if reServicesTop.MatchString(line) {
			inServices = true
			continue
		}
		if reTopLevelAlpha.MatchString(line) && !strings.HasPrefix(line, " ") {
			inServices = false
		}
		if inServices {
			m := reSvcKeyFull.FindStringSubmatch(line)
			if m != nil {
				defined[m[1]] = true
			}
		}
	}
	if len(defined) == 0 {
		return content, fixes
	}
	var out []string
	i := 0
	n := len(lines)
	for i < n {
		line := lines[i]
		// detect a depends_on: block (list form) at indent
		m := reDependsAny.FindStringSubmatch(line)
		if m != nil {
			indent := m[1]
			j := i + 1
			var kept []string
			var removed []string
			for j < n {
				dm := reDepEntryAny.FindStringSubmatch(lines[j])
				if dm != nil && (len(lines[j])-len(strings.TrimLeft(lines[j], " \t"))) > len(indent) {
					dep := dm[2]
					if defined[dep] {
						kept = append(kept, lines[j])
					} else {
						removed = append(removed, dep)
					}
					j++
				} else {
					break
				}
			}
			if len(removed) > 0 {
				for _, d := range removed {
					fixes = append(fixes, fmt.Sprintf("undefined_depends: removed '%s'", d))
				}
				if len(kept) > 0 {
					out = append(out, line)
					out = append(out, kept...)
				}
				// if nothing kept, drop the depends_on: line entirely
				i = j
				continue
			}
			out = append(out, line)
			i++
			continue
		}
		// also handle inline form: depends_on: [a, b]
		mi := reDependsInline.FindStringSubmatch(line)
		if mi != nil {
			indent, body := mi[1], mi[2]
			var deps []string
			for _, d := range strings.Split(body, ",") {
				d = strings.TrimSpace(d)
				if d == "" {
					continue
				}
				d = strings.Trim(d, "\"'")
				deps = append(deps, d)
			}
			var keep, drop []string
			for _, d := range deps {
				if defined[d] {
					keep = append(keep, d)
				} else {
					drop = append(drop, d)
				}
			}
			if len(drop) > 0 {
				for _, d := range drop {
					fixes = append(fixes, fmt.Sprintf("undefined_depends: removed '%s'", d))
				}
				if len(keep) > 0 {
					out = append(out, fmt.Sprintf("%sdepends_on: [%s]", indent, strings.Join(keep, ", ")))
				}
				i++
				continue
			}
		}
		out = append(out, line)
		i++
	}
	return strings.Join(out, "\n"), fixes
}

type repairBlock struct {
	name  string
	start int
	end   int
}

func fix_duplicate_service_keys(content string) (string, []string) {
	var fixes []string
	lines := strings.Split(content, "\n")
	// find service block boundaries: lines matching ^  <name>:  (2-space indent)
	var blocks []repairBlock
	var cur *repairBlock
	for i, line := range lines {
		m := reSvcKeyFull.FindStringSubmatch(line)
		if m != nil {
			if cur != nil {
				blocks = append(blocks, repairBlock{cur.name, cur.start, i})
			}
			cur = &repairBlock{name: m[1], start: i}
		} else if reTopLevelAlpha.MatchString(line) && cur != nil {
			// left the services section
			blocks = append(blocks, repairBlock{cur.name, cur.start, i})
			cur = nil
		}
	}
	if cur != nil {
		blocks = append(blocks, repairBlock{cur.name, cur.start, len(lines)})
	}
	// group by name (preserve first-seen order)
	byName := map[string][]repairBlock{}
	var nameOrder []string
	for _, b := range blocks {
		if _, ok := byName[b.name]; !ok {
			nameOrder = append(nameOrder, b.name)
		}
		byName[b.name] = append(byName[b.name], b)
	}
	var dropRanges []repairBlock
	score := func(b repairBlock) int {
		c := 0
		for _, l := range lines[b.start:b.end] {
			if strings.TrimSpace(l) != "" {
				c++
			}
		}
		return c
	}
	for _, name := range nameOrder {
		bl := byName[name]
		if len(bl) < 2 {
			continue
		}
		// score each by number of non-blank lines (completeness); keep the max.
		// Python's sorted is stable; emulate with a stable sort by descending score.
		scored := make([]repairBlock, len(bl))
		copy(scored, bl)
		sort.SliceStable(scored, func(a, b int) bool {
			return score(scored[a]) > score(scored[b])
		})
		for _, b := range scored[1:] {
			dropRanges = append(dropRanges, b)
			fixes = append(fixes, fmt.Sprintf("duplicate_service: removed second '%s' block (lines %d-%d)", name, b.start+1, b.end))
		}
	}
	if len(dropRanges) == 0 {
		return content, fixes
	}
	drop := map[int]bool{}
	for _, b := range dropRanges {
		for k := b.start; k < b.end; k++ {
			drop[k] = true
		}
	}
	var newLines []string
	for i, l := range lines {
		if !drop[i] {
			newLines = append(newLines, l)
		}
	}
	return strings.Join(newLines, "\n"), fixes
}

var reLineNum = regexp.MustCompile(`line (\d+)`)

// _compose_error — run compose config, return (ok, line_no, message). lineNo 0 if none.
func _compose_error(path string) (bool, int, string) {
	r := cli("compose", "-f", path, "config")
	if r.exitCode == 0 {
		return true, 0, ""
	}
	err := strings.TrimSpace(r.stderr)
	// filter the harmless unset-variable warnings
	var lines []string
	for _, l := range strings.Split(err, "\n") {
		if strings.Contains(l, "variable is not set") || strings.Contains(l, "AK_OUTPOST") {
			continue
		}
		lines = append(lines, l)
	}
	msg := err
	if len(lines) > 0 {
		msg = lines[len(lines)-1]
	}
	// try to extract a line number
	lno := 0
	matches := reLineNum.FindAllStringSubmatch(msg, -1)
	if len(matches) > 0 {
		if n, e := strconv.Atoi(matches[len(matches)-1][1]); e == nil {
			lno = n // the deepest/last line number compose reports
		}
	}
	return false, lno, msg
}

// repair_loop — error-driven surgical repair.
func repair_loop(path string, maxPasses int, logf string) []string {
	var actions []string
	logFn := func(m string) {
		actions = append(actions, m)
		if logf != "" {
			f, err := os.OpenFile(logf, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err == nil {
				f.WriteString(m + "\n")
				f.Close()
			}
		}
	}

	lastErr := ""
	haveLast := false
	for pass := 0; pass < maxPasses; pass++ {
		ok, lno, msg := _compose_error(path)
		if ok {
			logFn(fmt.Sprintf("repair_loop: VALID after %d pass(es)", pass))
			return actions
		}
		if haveLast && msg == lastErr {
			// no progress on the same error -> stop to avoid infinite loop
			logFn("repair_loop: STUCK on: " + msg)
			break
		}
		lastErr = msg
		haveLast = true
		data, err := os.ReadFile(path)
		if err != nil {
			break
		}
		content := string(data)
		before := content
		// classify + dispatch to the right in-place fixer
		fixedBy := ""
		ml := strings.ToLower(msg)
		var f []string
		if strings.Contains(ml, "did not find expected") || strings.Contains(ml, "mapping values") ||
			strings.Contains(ml, "block collection") || strings.Contains(ml, "found character") {
			content, f = fix_network_form(content) // mixed list/mapping nets
			if len(f) > 0 {
				fixedBy = "network_form"
			}
			if len(f) == 0 {
				content2, f2 := _fix_indent_at(content, lno) // generic indent repair
				if f2 {
					content = content2
					fixedBy = "indent"
				}
			}
		} else if strings.Contains(ml, "already defined") || strings.Contains(ml, "are equal") || strings.Contains(ml, "duplicate") {
			content, f = fix_duplicate_service_keys(content)
			if len(f) > 0 {
				fixedBy = "dup_service"
			}
			if len(f) == 0 {
				content, f = fix_network_form(content) // dedupes net lists too
				if len(f) > 0 {
					fixedBy = "dup_network"
				}
			}
		} else if strings.Contains(ml, "depends on undefined service") {
			content, f = fix_undefined_depends(content)
			if len(f) > 0 {
				fixedBy = "undefined_depends"
			}
		} else if strings.Contains(ml, "undefined network") {
			content, f = fix_undefined_networks(content)
			if len(f) > 0 {
				fixedBy = "undefined_network"
			}
		} else if strings.Contains(ml, "cycle") {
			content, f = fix_dependency_cycles(content)
			if len(f) > 0 {
				fixedBy = "dependency_cycle"
			}
		}
		if fixedBy != "" && content != before {
			repairCopy2(path, path+".prerepair")
			os.WriteFile(path, []byte(content), 0o644)
			logFn(fmt.Sprintf("repair_loop pass %d: %s -> fixed (%s)", pass, repairTrunc(msg, 80), fixedBy))
		} else {
			logFn(fmt.Sprintf("repair_loop pass %d: NO FIXER for: %s", pass, repairTrunc(msg, 120)))
			break
		}
	}
	return actions
}

// repairTrunc emulates Python slicing msg[:N].
func repairTrunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// _fix_indent_at — generic indentation repair near a reported line.
func _fix_indent_at(content string, lno int) (string, bool) {
	if lno == 0 {
		return content, false
	}
	lines := strings.Split(content, "\n")
	i := lno - 1
	if i < 0 || i >= len(lines) {
		return content, false
	}
	fixed := false
	start := i - 2
	if start < 0 {
		start = 0
	}
	end := i + 2
	if end > len(lines) {
		end = len(lines)
	}
	for j := start; j < end; j++ {
		l := lines[j]
		st := strings.TrimLeft(l, " ")
		ind := len(l) - len(st)
		// service-level keys should be 4 spaces; list items 6; net children 8
		if st != "" && !strings.HasPrefix(st, "-") && strings.HasSuffix(st, ":") && (ind == 3 || ind == 5) {
			newInd := ind
			if ind == 3 || ind == 5 {
				newInd = ind + 1
			}
			lines[j] = strings.Repeat(" ", newInd) + st
			fixed = true
		}
	}
	if fixed {
		return strings.Join(lines, "\n"), true
	}
	return content, false
}

// scan_all — scan all yml files and repair them.
func scan_all(stacksDirPath string, dryRun bool) {
	totalFixes := 0
	entries, err := os.ReadDir(stacksDirPath)
	if err != nil {
		fmt.Printf("\nTotal fixes: %d\n", totalFixes)
		return
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, fname := range names {
		if !strings.HasSuffix(fname, ".yml") {
			continue
		}
		path := filepath.Join(stacksDirPath, fname)
		fixes := repair_file(path, dryRun)
		if len(fixes) > 0 {
			prefix := ""
			if dryRun {
				prefix = "[dry-run] "
			}
			fmt.Printf("%sFixed %s:\n", prefix, fname)
			for _, f := range fixes {
				fmt.Printf("  - %s\n", f)
			}
			totalFixes += len(fixes)
		}
	}
	fmt.Printf("\nTotal fixes: %d\n", totalFixes)
}
