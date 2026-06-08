package main

// families.go — faithful Go port of stacks_families.py (the Container Family
// Detector). 3 detection methods: common name root, direct prefix, shared
// private network + name match. Universal: uses stacksDir()/configDir() instead
// of the Python's hardcoded paths, but the detection logic is line-for-line.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// set helpers (Python sets -> map[string]bool)
func strset(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, s := range items {
		m[s] = true
	}
	return m
}

var (
	famGlobalNets = strset("traefik_net", "apartment_net", "bridge", "host", "none",
		"ingress", "docker_gwbridge")
	famSkipContainers = strset("provisioner", "adminer", "surrealist", "cloudbeaver")
	famInfraSkip      = strset("traefik", "sablier", "crowdsec-bouncer", "error-pages")
	famDBWords        = strset("db", "redis", "cache", "postgres", "mysql", "mongo", "mariadb",
		"worker", "celery", "cron", "realtime", "beat", "scheduler",
		"daemon", "rabbitmq", "memcached", "valkey", "indexer")
	famNonFamilyRoots = strset("open", "agent", "cloudflared", "minecraft", "pritunl",
		"tailscale", "provisioner")
	famDBSubstrings = strset("postgres", "mysql", "mongo", "redis", "rabbitmq", "memcached")
)

// isSupport mirrors is_support(): a support/sidecar container (db, redis, worker…).
func isSupport(name string) bool {
	parts := strings.Split(strings.ReplaceAll(name, "_", "-"), "-")
	if famDBWords[parts[len(parts)-1]] {
		return true
	}
	for w := range famDBSubstrings {
		if strings.Contains(name, w) {
			return true
		}
	}
	return false
}

// famRoot mirrors root(): first meaningful segment (authentik-server -> authentik).
func famRoot(name string) string {
	s := strings.ReplaceAll(name, "_", "-")
	s = strings.ReplaceAll(s, ".", "-")
	return strings.Split(s, "-")[0]
}

// related mirrors related(): true if a and b are likely the same family.
func related(a, b string) bool {
	ra, rb := famRoot(a), famRoot(b)
	if famNonFamilyRoots[ra] || famNonFamilyRoots[rb] {
		return false
	}
	if ra == rb && len(ra) >= 3 {
		return true
	}
	s, lg := a, b
	if len(b) < len(a) {
		s, lg = b, a
	}
	return strings.HasPrefix(lg, s+"-") || strings.HasPrefix(lg, s+"_")
}

// famInfo mirrors the per-container dict from load_all().
type famInfo struct {
	file string
	nets map[string]bool
	ip   string
}

var (
	reContainerName = regexp.MustCompile(`container_name:\s*(\S+)`)
	reNextService   = regexp.MustCompile(`\n  [a-zA-Z][a-zA-Z0-9]`)
	reNet           = regexp.MustCompile(`(\w+_net)\s*:`)
	rePortIP        = regexp.MustCompile(`(192\.168\.1\.\d+):(\d+):\d+`)
)

// famStacksDir is overridable via get_families(stacks_dir); default = stacksDir().
var famStacksDir = ""

func familiesStacksDir() string {
	if famStacksDir != "" {
		return famStacksDir
	}
	return stacksDir()
}

// loadAll mirrors load_all(): scan every *.yml; returns ordered names + info map.
func loadAll() ([]string, map[string]famInfo) {
	order := []string{}
	containers := map[string]famInfo{}
	files, _ := filepath.Glob(filepath.Join(familiesStacksDir(), "*.yml"))
	sort.Strings(files)
	for _, fpath := range files {
		raw, err := os.ReadFile(fpath)
		if err != nil {
			continue
		}
		data := string(raw)
		fname := filepath.Base(fpath)
		for _, m := range reContainerName.FindAllStringSubmatch(data, -1) {
			cname := strings.Trim(strings.TrimSpace(m[1]), `"'`)
			if cname == "" {
				continue
			}
			idx := strings.Index(data, "container_name: "+cname)
			if idx < 0 {
				continue
			}
			end := idx + 3000
			if end > len(data) {
				end = len(data)
			}
			block := data[idx:end]
			if len(block) > 10 {
				if nx := reNextService.FindStringIndex(block[10:]); nx != nil {
					block = block[:nx[0]+10]
				}
			}
			nets := map[string]bool{}
			for _, nm := range reNet.FindAllStringSubmatch(block, -1) {
				if !famGlobalNets[nm[1]] {
					nets[nm[1]] = true
				}
			}
			ip := ""
			if pm := rePortIP.FindStringSubmatch(block); pm != nil {
				ip = pm[1]
			}
			if _, seen := containers[cname]; !seen {
				order = append(order, cname)
			}
			containers[cname] = famInfo{file: fname, nets: nets, ip: ip}
		}
	}
	return order, containers
}

// buildFamilies mirrors build_families(): union-find over the 3 methods.
func buildFamilies(order []string, containers map[string]famInfo) map[string]map[string]bool {
	parent := map[string]string{}
	for _, c := range order {
		parent[c] = c
	}
	var find func(string) string
	find = func(x string) string {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b string) {
		pa, pb := find(a), find(b)
		if pa == pb {
			return
		}
		if len(pa) <= len(pb) {
			parent[pb] = pa
		} else {
			parent[pa] = pb
		}
	}

	// Method 1: name-root / prefix match (primary)
	for i, c1 := range order {
		for _, c2 := range order[i+1:] {
			if related(c1, c2) {
				union(c1, c2)
			}
		}
	}

	// Method 2: shared private network + name confirmation
	netOrder := []string{}
	netMembers := map[string][]string{}
	for _, cname := range order {
		// nets iterated in sorted order for determinism (Python set order is arbitrary
		// but membership is what matters; union is commutative under name confirmation)
		nets := make([]string, 0, len(containers[cname].nets))
		for n := range containers[cname].nets {
			nets = append(nets, n)
		}
		sort.Strings(nets)
		for _, net := range nets {
			if _, ok := netMembers[net]; !ok {
				netOrder = append(netOrder, net)
			}
			netMembers[net] = append(netMembers[net], cname)
		}
	}
	for _, net := range netOrder {
		members := netMembers[net]
		if len(members) < 2 {
			continue
		}
		for i, c1 := range members {
			for _, c2 := range members[i+1:] {
				if related(c1, c2) {
					union(c1, c2)
				}
			}
		}
	}

	// Build groups
	groups := map[string]map[string]bool{}
	for _, c := range order {
		h := find(c)
		if groups[h] == nil {
			groups[h] = map[string]bool{}
		}
		groups[h][c] = true
	}

	// Filter + elect proper head
	result := map[string]map[string]bool{}
	for head, members := range groups {
		if len(members) < 2 {
			continue
		}
		skip := false
		for s := range famSkipContainers {
			if strings.Contains(head, s) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		var apps, supports []string
		for m := range members {
			if isSupport(m) {
				supports = append(supports, m)
			} else {
				apps = append(apps, m)
			}
		}
		if len(apps) == 0 {
			continue
		}
		if len(supports) == 0 {
			roots := map[string]bool{}
			for m := range members {
				roots[famRoot(m)] = true
			}
			if len(roots) > 1 {
				continue
			}
		}
		newHead := electHead(apps)
		if famInfraSkip[newHead] {
			continue
		}
		result[newHead] = members
	}
	return result
}

// electHead mirrors min(apps, key=head_score): smallest (len+penalty, name).
func electHead(apps []string) string {
	penalty := map[string]int{"indexer": 3, "dashboard": 2, "generator": 4,
		"certs": 4, "cert": 4, "worker": 3, "web": 1}
	score := func(n string) (int, string) {
		s := strings.ReplaceAll(n, ".", "-")
		parts := strings.Split(s, "-")
		last := parts[len(parts)-1]
		return len(n) + penalty[last], n
	}
	best := apps[0]
	bl, bn := score(best)
	for _, a := range apps[1:] {
		l, n := score(a)
		if l < bl || (l == bl && n < bn) {
			best, bl, bn = a, l, n
		}
	}
	return best
}

// loadFamilyWhitelist mirrors _load_family_whitelist(): families.yaml/.conf -> {member: head}.
func loadFamilyWhitelist() map[string]string {
	wl := map[string]string{}
	yp := filepath.Join(configDir(), "families.yaml")
	if y := loadNamed("families"); len(y) > 0 {
		_ = yp
		for m, h := range y {
			if strings.HasSuffix(h, "_net") {
				h = h[:len(h)-4]
			}
			if m != "" && h != "" {
				wl[m] = h
			}
		}
		if len(wl) > 0 {
			return wl
		}
	}
	cp := filepath.Join(configDir(), "families.conf")
	raw, err := os.ReadFile(cp)
	if err != nil {
		return wl
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		m, h, _ := strings.Cut(line, "=")
		m, h = strings.TrimSpace(m), strings.TrimSpace(h)
		if strings.HasSuffix(h, "_net") {
			h = h[:len(h)-4]
		}
		if m != "" && h != "" {
			wl[m] = h
		}
	}
	return wl
}

// getFamilies mirrors get_families(): {head: members}, with whitelist gap-fill.
func getFamilies(stacksDirArg string) map[string]map[string]bool {
	if stacksDirArg != "" {
		famStacksDir = stacksDirArg
	}
	order, containers := loadAll()
	fams := buildFamilies(order, containers)
	wl := loadFamilyWhitelist()
	if len(wl) > 0 {
		inReal := map[string]bool{}
		for _, mem := range fams {
			if len(mem) >= 2 {
				for m := range mem {
					inReal[m] = true
				}
			}
		}
		for member, head := range wl {
			if inReal[member] {
				continue
			}
			for h := range fams {
				if fams[h][member] && len(fams[h]) == 1 {
					delete(fams, h)
				}
			}
			if fams[head] == nil {
				fams[head] = map[string]bool{}
			}
			fams[head][head] = true
			fams[head][member] = true
		}
	}
	return fams
}

// getFamilyOf mirrors get_family_of().
func getFamilyOf(cname, stacksDirArg string) (string, map[string]bool) {
	for head, members := range getFamilies(stacksDirArg) {
		if members[cname] {
			return head, members
		}
	}
	return "", nil
}

// getFamilyHead mirrors get_family_head().
func getFamilyHead(cname, stacksDirArg string) string {
	h, _ := getFamilyOf(cname, stacksDirArg)
	return h
}

// familiesReport mirrors stacks_families.main(): the CONTAINER FAMILY REPORT.
func familiesReport() {
	order, containers := loadAll()
	families := buildFamilies(order, containers)
	allIn := map[string]bool{}
	for _, m := range families {
		for c := range m {
			allIn[c] = true
		}
	}
	type fam struct {
		head    string
		members map[string]bool
	}
	list := make([]fam, 0, len(families))
	for h, m := range families {
		list = append(list, fam{h, m})
	}
	// sort by (-len, head)
	sort.Slice(list, func(i, j int) bool {
		if len(list[i].members) != len(list[j].members) {
			return len(list[i].members) > len(list[j].members)
		}
		return list[i].head < list[j].head
	})
	line := strings.Repeat("=", 65)
	fmt.Println()
	fmt.Println(line)
	fmt.Println("  CONTAINER FAMILY REPORT")
	fmt.Println(line)
	fmt.Printf("  Total containers:        %d\n", len(containers))
	fmt.Printf("  Total families:          %d\n", len(list))
	fmt.Printf("  Containers in families:  %d\n", len(allIn))
	fmt.Printf("  Standalone containers:   %d\n", len(containers)-len(allIn))
	fmt.Println(line)
	for _, f := range list {
		other := make([]string, 0, len(f.members))
		for m := range f.members {
			if m != f.head {
				other = append(other, m)
			}
		}
		sort.Strings(other)
		fmt.Printf("\n  %s (%d containers)\n", f.head, len(f.members))
		for _, m := range other {
			fmt.Printf("    └─ %s\n", m)
		}
	}
}
