package main

// netguardian.go — faithful Go port of stacks_network_guardian.py.
//
// The original guardian rewrote stack & core files with blind string-replace,
// had a broken subnet-scan regex that made it think every subnet was free, and
// force-injected traefik_net into services. That corrupted core files and
// clobbered working configs.
//
// This replacement does the SAME job (define missing networks/volumes into the
// smallest creator file, with correct non-colliding subnets) by reusing the
// audited logic from stacks_fix.py. It does NOT touch service files, does NOT
// inject traefik_net anywhere, and only ever INSERTS into creator files (never
// deletes a line). Every write is backed up with a .bak-<timestamp>.
//
// In the Python project this module shells the fixer (stacks_fix.py) in via
// importlib and calls its audited helpers (load_conf, on, discover_creator_files,
// collect_service_refs, smallest_file_overall, all_used_subnets, add_to_creator).
// Those helpers are not yet ported as a shared Go module, so the small subset the
// guardian relies on is ported here as module-unique ng* helpers, behaving
// identically. If/when stacks_fix.py is ported, these can be swapped for the
// shared versions.

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

// ── creator-file model (mirrors the Python dict value) ──────────────────────
type ngCreator struct {
	nets map[string]bool
	vols map[string]bool
	size int64
}

// ngBackupDir mirrors stacks_fix.BACKUP_DIR but with universal paths.
func ngBackupDir() string {
	return filepath.Join(configDir(), "fix-backups")
}

// ngOn is a faithful port of stacks_fix.on():
//
//	def on(v): return str(v).strip() not in ("0", "", "false", "False", "no")
func ngOn(v string) bool {
	s := strings.TrimSpace(v)
	switch s {
	case "0", "", "false", "False", "no":
		return false
	}
	return true
}

// ngConfGet mirrors cfg.get(key, default) over the loaded config map.
func ngConfGet(cfg map[string]string, key, def string) string {
	if v, ok := cfg[key]; ok {
		return v
	}
	return def
}

// ngBackup is a faithful port of stacks_fix._backup(): honour FIX_BACKUP, copy
// the file into the backup dir as <name>.bak-<unix-ts>. All errors swallowed.
func ngBackup(p string) {
	defer func() { recover() }()
	cfg := configLoad()
	if !ngOn(ngConfGet(cfg, "FIX_BACKUP", "1")) {
		return
	}
	bdir := ngBackupDir()
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	dst := filepath.Join(bdir, filepath.Base(p)+fmt.Sprintf(".bak-%d", time.Now().Unix()))
	_ = os.WriteFile(dst, data, 0o644)
}

// ── regexes (compiled once), mirroring stacks_fix ───────────────────────────
var (
	ngReTopBlockName    = regexp.MustCompile(`^  ([a-zA-Z0-9][a-zA-Z0-9_.\-]*):`)
	ngReTopBlockNameSub = regexp.MustCompile(`^  ([a-zA-Z0-9][a-zA-Z0-9_.\-]*):(.*)$`)
	ngReTopLevelStart   = regexp.MustCompile(`^[a-zA-Z]`)
	ngReServices        = regexp.MustCompile(`^services:\s*$`)
	ngReProvisioner     = regexp.MustCompile(`^  (provisioner[a-zA-Z0-9_.\-]*):\s*$`)
	ngReProvBlockLine   = regexp.MustCompile(`^  [a-zA-Z0-9]`)
	ngReServiceNetList  = regexp.MustCompile(`(?m)^\s{4,6}-\s+"?([a-zA-Z0-9][a-zA-Z0-9_.\-]*_net)"?\s*$`)
	ngReServiceNetMap   = regexp.MustCompile(`(?m)^\s{6}([a-zA-Z0-9][a-zA-Z0-9_.\-]*_net):\s*$`)
	ngReServiceVol      = regexp.MustCompile(`(?m)^\s{4,8}-\s+"?([a-zA-Z0-9][a-zA-Z0-9_.\-]*):(/[^"\s]+)"?\s*$`)
	ngReProvContainer   = regexp.MustCompile(`container_name:\s*provisioner`)
	ngReNetworksHeader  = regexp.MustCompile(`(?m)^networks:\s*$`)
	ngReProvListEntry   = regexp.MustCompile(`^      -\s+"?([^"\s]+)"?`)
	ngReProvKeyChild    = regexp.MustCompile(`^    [a-zA-Z]`)
)

// ngBlockHeaderRe builds ^<block>:\s*$ for a given top-level block name.
func ngBlockHeaderRe(block string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(block) + `:\s*$`)
}

// ngTopLevelBlockNames — faithful port of stacks_fix.top_level_block_names().
// Returns names defined under a top-level `networks:` or `volumes:` block.
func ngTopLevelBlockNames(content, block string) []string {
	var names []string
	inBlock := false
	headerRe := ngBlockHeaderRe(block)
	for _, line := range strings.Split(content, "\n") {
		if headerRe.MatchString(line) {
			inBlock = true
			continue
		}
		if inBlock && ngReTopLevelStart.MatchString(line) && !strings.HasPrefix(line, " ") {
			inBlock = false
		}
		if !inBlock {
			continue
		}
		if m := ngReTopBlockName.FindStringSubmatch(line); m != nil {
			names = append(names, m[1])
		}
	}
	return names
}

// ngDiscoverCreatorFiles — faithful port of stacks_fix.discover_creator_files().
// A "creator file" defines named entries under top-level networks:/volumes:.
func ngDiscoverCreatorFiles(stacksDirPath string, skipFiles map[string]bool) map[string]*ngCreator {
	creators := map[string]*ngCreator{}
	if skipFiles == nil {
		skipFiles = map[string]bool{}
	}
	for _, f := range ngSortedYmls(stacksDirPath) {
		if skipFiles[f] {
			continue
		}
		path := filepath.Join(stacksDirPath, f)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		content := string(data)
		nets := ngSet(ngTopLevelBlockNames(content, "networks"))
		vols := ngSet(ngTopLevelBlockNames(content, "volumes"))
		if len(nets) > 0 || len(vols) > 0 {
			var sz int64
			if fi, e := os.Stat(path); e == nil {
				sz = fi.Size()
			}
			creators[path] = &ngCreator{nets: nets, vols: vols, size: sz}
		}
	}
	return creators
}

// ngSmallestFileOverall — faithful port of stacks_fix.smallest_file_overall().
// Smallest yml with a provisioner container, else smallest with a networks:
// section. Never picks files without a provisioner or networks: block.
func ngSmallestFileOverall(stacksDirPath string) string {
	var best string
	var bestSize int64 = -1
	var fallback string
	var fallbackSize int64 = -1
	for _, f := range ngSortedYmls(stacksDirPath) {
		path := filepath.Join(stacksDirPath, f)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		content := string(data)
		fi, e := os.Stat(path)
		if e != nil {
			continue
		}
		sz := fi.Size()
		if ngReProvContainer.MatchString(content) {
			if bestSize < 0 || sz < bestSize {
				bestSize = sz
				best = path
			}
		} else if ngReNetworksHeader.MatchString(content) {
			if fallbackSize < 0 || sz < fallbackSize {
				fallbackSize = sz
				fallback = path
			}
		}
	}
	if best != "" {
		return best
	}
	return fallback
}

// ngAllUsedSubnets — faithful port of stacks_fix.all_used_subnets().
// Scan every creator file for used 3rd octets in <base>.<N>.0/24.
func ngAllUsedSubnets(creators map[string]*ngCreator, subnetBase string) map[int]bool {
	used := map[int]bool{}
	pat := regexp.MustCompile(regexp.QuoteMeta(subnetBase) + `\.(\d{1,3})\.0/24`)
	for path := range creators {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, m := range pat.FindAllStringSubmatch(string(data), -1) {
			if n, e := strconv.Atoi(m[1]); e == nil {
				used[n] = true
			}
		}
	}
	return used
}

// ngNextSubnetOctet — faithful port of stacks_fix.next_subnet_octet().
// Gap-fill from 1..254; if full, climb above the highest.
func ngNextSubnetOctet(used map[int]bool) int {
	for n := 1; n < 255; n++ {
		if !used[n] {
			return n
		}
	}
	if len(used) > 0 {
		max := 0
		for n := range used {
			if n > max {
				max = n
			}
		}
		return max + 1
	}
	return 1
}

// ngCollectServiceRefs — faithful port of stacks_fix.collect_service_refs().
// Gather network & volume names that services actually reference, verbatim.
func ngCollectServiceRefs(stacksDirPath string, creators map[string]*ngCreator, skipFiles map[string]bool) (map[string]bool, map[string]bool) {
	neededNets := map[string]bool{}
	neededVols := map[string]bool{}
	if skipFiles == nil {
		skipFiles = map[string]bool{}
	}
	for _, f := range ngSortedYmls(stacksDirPath) {
		if skipFiles[f] {
			continue
		}
		path := filepath.Join(stacksDirPath, f)
		// (creator files still have their service refs read; their own
		// top-level defs are handled separately by the caller.)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		content := string(data)

		for _, m := range ngReServiceNetList.FindAllStringSubmatch(content, -1) {
			neededNets[m[1]] = true
		}
		for _, m := range ngReServiceNetMap.FindAllStringSubmatch(content, -1) {
			neededNets[m[1]] = true
		}

		for _, m := range ngReServiceVol.FindAllStringSubmatch(content, -1) {
			vol := m[1]
			pathPart := m[2]
			if strings.HasPrefix(vol, ".") || strings.HasPrefix(vol, "/") || strings.HasPrefix(vol, "~") {
				continue
			}
			switch vol {
			case "http", "https", "ftp", "ws", "wss", "tcp", "udp":
				continue
			}
			if strings.Contains(pathPart, "//") {
				continue
			}
			neededVols[vol] = true
		}
	}
	return neededNets, neededVols
}

// ngNetDefinition — faithful port of stacks_fix.net_definition().
func ngNetDefinition(name string, octet int, subnetBase string) string {
	base := name
	if strings.HasSuffix(name, "_net") {
		base = name[:len(name)-4]
	}
	return fmt.Sprintf(
		"  %s: {name: %s, driver: bridge, attachable: true, "+
			"external: false, internal: false, enable_ipv6: false, "+
			"labels: [\"com.bellzserver.network=%s\", "+
			"\"com.bellzserver.env=production\"], "+
			"ipam: {driver: default, config: [{subnet: %s.%d.0/24, "+
			"gateway: %s.%d.1}]}}\n",
		name, name, base, subnetBase, octet, subnetBase, octet,
	)
}

// ngVolDefinition — faithful port of stacks_fix.vol_definition().
func ngVolDefinition(name string, external bool) string {
	if external {
		return fmt.Sprintf("  %s: {name: %s, external: true}\n", name, name)
	}
	return fmt.Sprintf("  %s: {name: %s, external: false}\n", name, name)
}

// ngFindProvisionerBlock — faithful port of stacks_fix.find_provisioner_block().
// Returns (start, end, ok) of the first provisioner_* service block.
func ngFindProvisionerBlock(lines []string) (int, int, bool) {
	inServices := false
	for i, line := range lines {
		if ngReServices.MatchString(strings.TrimRight(line, " \t\r\n")) {
			inServices = true
			continue
		}
		if inServices {
			if ngReProvisioner.MatchString(strings.TrimRight(line, " \t\r\n")) {
				for j := i + 1; j < len(lines); j++ {
					if ngReProvBlockLine.MatchString(lines[j]) ||
						(ngReTopLevelStart.MatchString(lines[j]) && !strings.HasPrefix(lines[j], " ")) {
						return i, j, true
					}
				}
				return i, len(lines), true
			}
		}
	}
	return 0, 0, false
}

// ngInsertAfterBlockHeader — faithful port of the nested insert_after_block_header.
// Returns (newLines, true) on success, (nil, false) if header not found.
func ngInsertAfterBlockHeader(lines []string, header string, payload []string) ([]string, bool) {
	headerRe := ngBlockHeaderRe(header)
	for i, l := range lines {
		if headerRe.MatchString(strings.TrimRight(l, " \t\r\n")) {
			out := make([]string, 0, len(lines)+len(payload))
			out = append(out, lines[:i+1]...)
			out = append(out, payload...)
			out = append(out, lines[i+1:]...)
			return out, true
		}
	}
	return nil, false
}

// ngEnsureInList — faithful port of the nested ensure_in_list inside add_to_creator.
func ngEnsureInList(block []string, key string, items map[string]bool) []string {
	keyRe := regexp.MustCompile(`^    ` + regexp.QuoteMeta(key) + `:\s*$`)
	for bi, bl := range block {
		if keyRe.MatchString(strings.TrimRight(bl, " \t\r\n")) {
			existing := map[string]bool{}
			insertAt := bi + 1
			for k := bi + 1; k < len(block); k++ {
				if mm := ngReProvListEntry.FindStringSubmatch(block[k]); mm != nil {
					existing[strings.Split(mm[1], ":")[0]] = true
					insertAt = k + 1
				} else if ngReProvKeyChild.MatchString(block[k]) {
					break
				}
			}
			var adds []string
			for _, it := range ngSortedKeys(items) {
				if !existing[it] {
					if key == "volumes" {
						adds = append(adds, fmt.Sprintf("      - \"%s:/provision/%s\"", it, it))
					} else {
						adds = append(adds, fmt.Sprintf("      - \"%s\"", it))
					}
				}
			}
			if len(adds) > 0 {
				out := make([]string, 0, len(block)+len(adds))
				out = append(out, block[:insertAt]...)
				out = append(out, adds...)
				out = append(out, block[insertAt:]...)
				block = out
			}
			return block
		}
	}
	return block
}

// ngAddToCreator — faithful port of stacks_fix.add_to_creator().
// Appends network/volume defs to a creator file's top-level blocks and syncs
// them into that file's provisioner_* service lists. Inserts only.
func ngAddToCreator(path string, newNets, newVols map[string]bool, subnetBase string, usedSubnets map[int]bool, dryRun bool) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	content := string(data)
	lines := strings.Split(content, "\n")
	var notes []string

	existingNets := ngSet(ngTopLevelBlockNames(content, "networks"))
	existingVols := ngSet(ngTopLevelBlockNames(content, "volumes"))

	// ---- Networks ----
	for _, net := range ngSortedKeys(newNets) {
		if existingNets[net] {
			continue
		}
		octet := ngNextSubnetOctet(usedSubnets)
		usedSubnets[octet] = true
		payload := []string{strings.TrimRight(ngNetDefinition(net, octet, subnetBase), "\n")}
		if res, ok := ngInsertAfterBlockHeader(lines, "networks", payload); ok {
			lines = res
		} else {
			inserted := false
			for i, l := range lines {
				if ngReServices.MatchString(strings.TrimRight(l, " \t\r\n")) {
					out := make([]string, 0, len(lines)+len(payload)+2)
					out = append(out, lines[:i]...)
					out = append(out, "networks:")
					out = append(out, payload...)
					out = append(out, "")
					out = append(out, lines[i:]...)
					lines = out
					inserted = true
					break
				}
			}
			if !inserted {
				out := make([]string, 0, len(lines)+len(payload)+2)
				out = append(out, "networks:")
				out = append(out, payload...)
				out = append(out, "")
				out = append(out, lines...)
				lines = out
			}
		}
		existingNets[net] = true
		notes = append(notes, fmt.Sprintf("net %s -> %s.%d.0/24", net, subnetBase, octet))
	}

	// ---- Volumes ----
	for _, vol := range ngSortedKeys(newVols) {
		if existingVols[vol] {
			continue
		}
		payload := []string{strings.TrimRight(ngVolDefinition(vol, true), "\n")}
		if res, ok := ngInsertAfterBlockHeader(lines, "volumes", payload); ok {
			lines = res
		} else {
			inserted := false
			for i, l := range lines {
				if ngReServices.MatchString(strings.TrimRight(l, " \t\r\n")) {
					out := make([]string, 0, len(lines)+len(payload)+2)
					out = append(out, lines[:i]...)
					out = append(out, "volumes:")
					out = append(out, payload...)
					out = append(out, "")
					out = append(out, lines[i:]...)
					lines = out
					inserted = true
					break
				}
			}
			if !inserted {
				out := make([]string, 0, len(lines)+len(payload)+2)
				out = append(out, "volumes:")
				out = append(out, payload...)
				out = append(out, "")
				out = append(out, lines...)
				lines = out
			}
		}
		existingVols[vol] = true
		notes = append(notes, fmt.Sprintf("vol %s", vol))
	}

	// ---- Provisioner sync ----
	if pstart, pend, ok := ngFindProvisionerBlock(lines); ok && (len(newNets) > 0 || len(newVols) > 0) {
		block := append([]string{}, lines[pstart:pend]...)
		block = ngEnsureInList(block, "networks", newNets)
		block = ngEnsureInList(block, "volumes", newVols)
		out := make([]string, 0, len(lines))
		out = append(out, lines[:pstart]...)
		out = append(out, block...)
		out = append(out, lines[pend:]...)
		lines = out
	}

	newContent := strings.Join(lines, "\n")
	if newContent != content && len(notes) > 0 {
		if dryRun {
			fmt.Printf("  [dry-run] would add to %s: %s\n", filepath.Base(path), strings.Join(notes, "; "))
		} else {
			ngBackup(path)
			_ = os.WriteFile(path, []byte(newContent), 0o644)
			fmt.Printf("  ✔ %s: added %s\n", filepath.Base(path), strings.Join(notes, "; "))
		}
		return len(notes)
	}
	return 0
}

// ── small set helpers (module-unique) ───────────────────────────────────────

func ngSet(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}

func ngSortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ngSortedYmls returns sorted .yml/.yaml filenames in dir (mirrors
// sorted(os.listdir) + extension filter used throughout stacks_fix).
func ngSortedYmls(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasSuffix(n, ".yml") || strings.HasSuffix(n, ".yaml") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// ── union of creator nets/vols ──────────────────────────────────────────────

func ngUnionDefined(creators map[string]*ngCreator, pick func(*ngCreator) map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, c := range creators {
		for k := range pick(c) {
			out[k] = true
		}
	}
	return out
}

func ngDiff(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		if !b[k] {
			out[k] = true
		}
	}
	return out
}

// ── run_guardian — faithful port of the top-level entry point ───────────────
//
// In Python this loads stacks_fix.py via importlib and bails safely if it can't.
// The Go port reuses the ng* helpers above (the audited fixer logic) directly,
// so there is no dynamic-load failure path; behaviour is otherwise identical.
func runGuardian() {
	cfg := configLoad()

	sd := ngConfGet(cfg, "STACKS_DIR", "")
	if sd == "" {
		// Universal-path fallback (Python hardcoded /home/bellzserver/...).
		sd = stacksDir()
	}
	if fi, err := os.Stat(sd); err != nil || !fi.IsDir() {
		fmt.Println("SUCCESS: Guardian idle (stacks dir missing) — no changes made.")
		return
	}

	// Respect the same toggle the fixer uses. If the user turned
	// network/volume auto-define OFF, the guardian stays out of the way.
	if !ngOn(ngConfGet(cfg, "FIX_DEFINE_NETVOL", "1")) {
		fmt.Println("SUCCESS: Guardian disabled via FIX_DEFINE_NETVOL=0 — no changes.")
		return
	}

	// Discover creator files by CONTENT (not hard-coded names).
	creators := ngDiscoverCreatorFiles(sd, nil)
	neededNets, neededVols := ngCollectServiceRefs(sd, creators, nil)

	var definedNets, definedVols map[string]bool
	if len(creators) > 0 {
		definedNets = ngUnionDefined(creators, func(c *ngCreator) map[string]bool { return c.nets })
		definedVols = ngUnionDefined(creators, func(c *ngCreator) map[string]bool { return c.vols })
	} else {
		definedNets = map[string]bool{}
		definedVols = map[string]bool{}
	}

	missingNets := ngDiff(neededNets, definedNets)
	missingVols := ngDiff(neededVols, definedVols)

	if len(missingNets) == 0 && len(missingVols) == 0 {
		fmt.Println("SUCCESS: Infrastructure synchronized. No drift detected.")
		return
	}

	// Pick smallest creator; if none exist, bootstrap into smallest file overall.
	var targetPath string
	if len(creators) > 0 {
		// min(creators, key=size). Python ties resolve by dict iteration order
		// (insertion = sorted-listdir order); replicate via sorted paths.
		paths := make([]string, 0, len(creators))
		for p := range creators {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		var bestSize int64 = -1
		for _, p := range paths {
			if bestSize < 0 || creators[p].size < bestSize {
				bestSize = creators[p].size
				targetPath = p
			}
		}
	} else {
		targetPath = ngSmallestFileOverall(sd)
		if targetPath == "" {
			fmt.Println("SUCCESS: Guardian idle (no compose files) — no changes made.")
			return
		}
	}

	subnetBase := ngConfGet(cfg, "FIX_SUBNET_BASE", "10.50")
	used := ngAllUsedSubnets(creators, subnetBase)
	added := ngAddToCreator(targetPath, missingNets, missingVols, subnetBase, used, false)

	if added > 0 {
		fmt.Printf("SUCCESS: Guardian optimized infrastructure inside %s\n", filepath.Base(targetPath))
	} else {
		fmt.Println("SUCCESS: Infrastructure synchronized. No drift detected.")
	}
}
