// collision.go — faithful Go port of stacks_collision.py.
//
// IP and port collision detection + assignment.
// PUBLIC FUNCTIONS:
//   collisionLoadConf()                  — load stacks.conf settings (with defaults)
//   scanAllIPs()                         — {ip: [(stack,container)]}
//   scanAllPorts()                       — {ip:port: [(stack,container)]}
//   getCollisions()                      — (ipCollisions, portCollisions)
//   getNextAvailableIP()                 — next free IP in range
//   getNextAvailablePort(ip, preferred)  — next free port for a given IP
//   getImageDefaultPort(image)           — inspect image for ExposedPorts
//   isLockedContainer(name)              — check if container is locked
//   isNetworkModeHost(fpath, svc)        — check if service uses network_mode: host
//   collisionValidateIP(ip)              — check IP against range/blacklist/whitelist
//   collisionValidatePort(port)          — check port against range/blacklist
//   addIPBlacklist(ip)                   — add IP to blacklist in stacks.conf
//   addIPWhitelist(ip)                   — add IP to whitelist in stacks.conf
//   addPortBlacklist(port)               — add port to blacklist in stacks.conf
//   addLockedContainer(name)             — add container to locked list
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

// ── module-unique helpers/state ───────────────────────────────────────────────

// collisionConfFile mirrors CONF_FILE = ~/.config/stacks/stacks.conf.
func collisionConfFile() string { return filepath.Join(configDir(), "stacks.conf") }

// collisionStacksDir mirrors STACKS_DIR (hardcoded in Python → use helper).
func collisionStacksDir() string { return stacksDir() }

// collisionLedgerFile mirrors LEDGER_FILE = ~/.config/stacks/ip_assignments.conf.
func collisionLedgerFile() string { return filepath.Join(configDir(), "ip_assignments.conf") }

// collisionOwner mirrors the Python (stack, container) tuple.
type collisionOwner struct {
	Stack     string
	Container string
}

// collisionIPRec mirrors an IP-collision dict.
type collisionIPRec struct {
	IP     string
	Owners []collisionOwner
	Type   string
}

// collisionPortRec mirrors a port-collision dict.
type collisionPortRec struct {
	IP      string
	Port    string
	Owners  []collisionOwner
	Type    string
	Running []string
	Active  bool
}

// collisionSplitCSV mirrors [x.strip() for x in s.split(",") if x.strip()].
func collisionSplitCSV(s string) []string {
	out := []string{}
	for _, x := range strings.Split(s, ",") {
		x = strings.TrimSpace(x)
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

// collisionSet mirrors a set built from the CSV.
func collisionSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, x := range collisionSplitCSV(s) {
		m[x] = true
	}
	return m
}

// collisionStripQuotes mirrors .strip('"').strip("'") chains.
func collisionStripQuotes(s string) string {
	return strings.Trim(s, "\"'")
}

// ── Host port scanner ────────────────────────────────────────────────────────

var collisionSSRe = regexp.MustCompile(`([\d.]+):(\d+)\s+0\.0\.0\.0:\*`)

// scanHostPorts mirrors scan_host_ports(): {ip: set(ports)} via `ss -tlnp`.
func scanHostPorts() map[string]map[string]bool {
	hostPorts := map[string]map[string]bool{}
	cmd := exec.Command("ss", "-tlnp")
	out, err := cmd.Output()
	if err != nil {
		return hostPorts
	}
	for _, line := range strings.Split(string(out), "\n") {
		m := collisionSSRe.FindStringSubmatch(line)
		if m != nil {
			ip, port := m[1], m[2]
			if hostPorts[ip] == nil {
				hostPorts[ip] = map[string]bool{}
			}
			hostPorts[ip][port] = true
		}
	}
	return hostPorts
}

// isPortFreeOnHost mirrors is_port_free_on_host(ip, port).
func isPortFreeOnHost(ip string, port string) bool {
	hostPorts := scanHostPorts()
	if ps, ok := hostPorts[ip]; ok && ps[port] {
		return false
	}
	return true
}

// ── Related container grouping ────────────────────────────────────────────────

var (
	collisionCnameRe   = regexp.MustCompile(`container_name:\s*(\S+)`)
	collisionNetRe     = regexp.MustCompile(`(\w+_net)\s*:`)
	collisionDependRe  = regexp.MustCompile(`-\s+[A-Za-z][\w-]+`)
	collisionURLRefRe  = regexp.MustCompile(`(?:https?|redis|postgres|mysql|mongo)://[^@\s]*@?([a-zA-Z][a-zA-Z0-9_-]+):\d+`)
)

// getRelatedContainers mirrors get_related_containers(fpath).
// Returns list of groups (each a list of related container names), size>1.
func getRelatedContainers(fpath string) [][]string {
	// groups are represented as ordered slices acting like sets.
	groups := [][]string{}

	dataBytes, err := os.ReadFile(fpath)
	if err != nil {
		return [][]string{}
	}
	data := string(dataBytes)

	// cnames = [strip quotes]
	cnames := []string{}
	cnameSet := map[string]bool{}
	for _, m := range collisionCnameRe.FindAllStringSubmatch(data, -1) {
		c := collisionStripQuotes(m[1])
		cnames = append(cnames, c)
		cnameSet[c] = true
	}
	if len(cnames) == 0 {
		return [][]string{}
	}

	// Build info blocks per container.
	info := map[string]string{}
	infoOrder := []string{}
	for _, cname := range cnames {
		idx := strings.Index(data, "container_name: "+cname)
		if idx < 0 {
			continue
		}
		end := idx + 3000
		if end > len(data) {
			end = len(data)
		}
		if _, seen := info[cname]; !seen {
			infoOrder = append(infoOrder, cname)
		}
		info[cname] = data[idx:end]
	}

	findG := func(name string) int {
		for i, g := range groups {
			if inList(g, name) {
				return i
			}
		}
		return -1
	}
	groupAdd := func(gi int, name string) {
		if !inList(groups[gi], name) {
			groups[gi] = append(groups[gi], name)
		}
	}
	merge := func(a, b string) {
		if a == b || !cnameSet[a] || !cnameSet[b] {
			return
		}
		ga, gb := findG(a), findG(b)
		if ga < 0 && gb < 0 {
			groups = append(groups, []string{a, b})
		} else if ga < 0 {
			groupAdd(gb, a)
		} else if gb < 0 {
			groupAdd(ga, b)
		} else if ga != gb {
			// ga.update(gb); groups.remove(gb)
			for _, x := range groups[gb] {
				groupAdd(ga, x)
			}
			groups = append(groups[:gb], groups[gb+1:]...)
		}
	}

	globalNets := map[string]bool{
		"traefik_net": true, "apartment_net": true, "bridge": true, "host": true,
		"none": true, "ingress": true, "docker_gwbridge": true,
	}

	// ── PRIMARY 1: Shared private network ──────────────────────────────────────
	netMembers := map[string][]string{}
	netOrder := []string{}
	for _, cname := range infoOrder {
		block := info[cname]
		for _, m := range collisionNetRe.FindAllStringSubmatch(block, -1) {
			net := m[1]
			if globalNets[net] {
				continue
			}
			if _, ok := netMembers[net]; !ok {
				netOrder = append(netOrder, net)
			}
			if !inList(netMembers[net], cname) {
				netMembers[net] = append(netMembers[net], cname)
			}
		}
	}
	for _, net := range netOrder {
		members := netMembers[net]
		if len(members) > 1 {
			for _, m := range members[1:] {
				merge(members[0], m)
			}
		}
	}

	// ── PRIMARY 2: Name prefix ─────────────────────────────────────────────────
	for i, c1 := range cnames {
		for _, c2 := range cnames[i+1:] {
			short, long := c1, c2
			if len(c1) > len(c2) {
				short, long = c2, c1
			}
			if strings.HasPrefix(long, short+"-") || strings.HasPrefix(long, short+"_") {
				merge(c1, c2)
			}
		}
	}

	// ── SECONDARY 3: depends_on ────────────────────────────────────────────────
	for _, cname := range infoOrder {
		block := info[cname]
		if strings.Contains(block, "depends_on") {
			for _, m := range collisionDependRe.FindAllString(block, -1) {
				d := strings.TrimSpace(m)
				d = strings.TrimLeft(d, "- ")
				d = strings.Trim(d, "\"'")
				if cnameSet[d] && d != cname {
					merge(cname, d)
				}
			}
		}
	}

	// ── SECONDARY 4: Env var references ────────────────────────────────────────
	envKeys := []string{
		"DB_HOST", "DATABASE_HOST", "POSTGRES_HOST", "MYSQL_HOST",
		"MONGO_HOST", "REDIS_HOST", "REDIS_URL", "DATABASE_URL",
		"MONGO_URL", "ELASTICSEARCH_HOST", "OPENSEARCH_HOST",
		"CELERY_BROKER_URL", "AMQP_URL", "RABBITMQ_HOST",
	}
	for _, cname := range infoOrder {
		block := info[cname]
		for _, key := range envKeys {
			re := regexp.MustCompile(key + `=(?:https?://|redis://|amqp://|postgresql://|mysql://)?(?:[^@\s]*@)?([a-zA-Z][a-zA-Z0-9_-]+)`)
			for _, mm := range re.FindAllStringSubmatch(block, -1) {
				val := collisionStripQuotes(mm[1])
				if cnameSet[val] && val != cname {
					merge(cname, val)
				}
			}
		}
	}

	// ── SECONDARY 5: URL references ────────────────────────────────────────────
	for _, cname := range infoOrder {
		block := info[cname]
		for _, mm := range collisionURLRefRe.FindAllStringSubmatch(block, -1) {
			ref := collisionStripQuotes(mm[1])
			if cnameSet[ref] && ref != cname {
				merge(cname, ref)
			}
		}
	}

	out := [][]string{}
	for _, g := range groups {
		if len(g) > 1 {
			out = append(out, g)
		}
	}
	return out
}

// ── Config ────────────────────────────────────────────────────────────────────

// collisionLoadConf mirrors load_conf(): defaults + stacks.conf + YAML overlay.
func collisionLoadConf() map[string]string {
	cfg := map[string]string{
		"IP_RANGE_START":            "192.168.1.153",
		"IP_RANGE_END":              "192.168.1.253",
		"PORT_RANGE_START":          "8080",
		"PORT_RANGE_END":            "8999",
		"IP_BLACKLIST":              "192.168.1.1,192.168.1.114,192.168.1.151",
		"IP_WHITELIST":              "",
		"PORT_BLACKLIST":            "22,80,443,3306,5432,6379,27017,2375,2376",
		"LOCKED_IPS":                "",
		"IP_PORT_LOCKED_CONTAINERS": "cloudflared,cloudflared_tunnel_core,cloudflared-doh,traefik,sablier",
		"NETWORK_MODE_SKIP":         "1",
		"IP_COLLISION_AUTOFIX":      "0",
	}
	// stacks.conf overlay
	if data, err := os.ReadFile(collisionConfFile()); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			l := strings.TrimSpace(line)
			if strings.Contains(l, "=") && !strings.HasPrefix(l, "#") {
				parts := strings.SplitN(l, "=", 2)
				k := strings.TrimSpace(parts[0])
				v := strings.Trim(strings.TrimSpace(parts[1]), "\"'")
				cfg[k] = v
			}
		}
	}
	// YAML master overlay (stacks.yaml wins) — configLoad() reads the YAML/conf.
	for k, v := range configLoad() {
		cfg[k] = v
	}
	return cfg
}

// collisionUpdateConf mirrors _update_conf(key, value): write into stacks.conf.
func collisionUpdateConf(key, value string) bool {
	data, err := os.ReadFile(collisionConfFile())
	var lines []string
	if err == nil {
		// readlines() keeps trailing newlines; we operate on logical lines.
		raw := string(data)
		if raw != "" {
			lines = strings.Split(raw, "\n")
			// Strip a trailing empty element introduced by a final newline so we
			// don't reintroduce blank lines on rewrite.
			if len(lines) > 0 && lines[len(lines)-1] == "" {
				lines = lines[:len(lines)-1]
			}
		}
	}
	keyRe := regexp.MustCompile(`^` + regexp.QuoteMeta(key) + `\s*=`)
	found := false
	for i, l := range lines {
		if keyRe.MatchString(strings.TrimSpace(l)) {
			lines[i] = fmt.Sprintf(`%s="%s"`, key, value)
			found = true
			break
		}
	}
	if !found {
		lines = append(lines, fmt.Sprintf(`%s="%s"`, key, value))
	}
	out := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(collisionConfFile(), []byte(out), 0644); err != nil {
		return false
	}
	return true
}

// ── Blacklist/Whitelist management ────────────────────────────────────────────

// addIPBlacklist mirrors add_ip_blacklist(ip).
func addIPBlacklist(ip string) []string {
	cfg := collisionLoadConf()
	bl := collisionSplitCSV(cfg["IP_BLACKLIST"])
	if !inList(bl, ip) {
		bl = append(bl, ip)
		collisionUpdateConf("IP_BLACKLIST", strings.Join(bl, ","))
	}
	return bl
}

// addIPWhitelist mirrors add_ip_whitelist(ip).
func addIPWhitelist(ip string) []string {
	cfg := collisionLoadConf()
	wl := collisionSplitCSV(cfg["IP_WHITELIST"])
	if !inList(wl, ip) {
		wl = append(wl, ip)
		collisionUpdateConf("IP_WHITELIST", strings.Join(wl, ","))
	}
	return wl
}

// addPortBlacklist mirrors add_port_blacklist(port).
func addPortBlacklist(port string) []string {
	cfg := collisionLoadConf()
	bl := collisionSplitCSV(cfg["PORT_BLACKLIST"])
	if !inList(bl, port) {
		bl = append(bl, port)
		collisionUpdateConf("PORT_BLACKLIST", strings.Join(bl, ","))
	}
	return bl
}

// addLockedContainer mirrors add_locked_container(name).
func addLockedContainer(name string) []string {
	cfg := collisionLoadConf()
	locked := collisionSplitCSV(cfg["IP_PORT_LOCKED_CONTAINERS"])
	if !inList(locked, name) {
		locked = append(locked, name)
		collisionUpdateConf("IP_PORT_LOCKED_CONTAINERS", strings.Join(locked, ","))
	}
	return locked
}

// ── Validation ────────────────────────────────────────────────────────────────

// collisionValidateIP mirrors validate_ip(ip): (valid, reason).
func collisionValidateIP(ip string) (bool, string) {
	cfg := collisionLoadConf()
	blacklist := collisionSet(cfg["IP_BLACKLIST"])
	locked := collisionSet(cfg["LOCKED_IPS"])
	whitelist := collisionSplitCSV(cfg["IP_WHITELIST"])

	if blacklist[ip] {
		return false, "blacklisted"
	}
	if locked[ip] {
		return false, "locked"
	}

	if len(whitelist) > 0 {
		if inList(whitelist, ip) {
			return true, "whitelisted"
		}
		return false, "not in whitelist"
	}

	// Check range
	startParts := strings.Split(cfg["IP_RANGE_START"], ".")
	endParts := strings.Split(cfg["IP_RANGE_END"], ".")
	ipParts := strings.Split(ip, ".")
	if len(startParts) < 4 || len(endParts) < 4 || len(ipParts) < 4 {
		return false, "invalid format"
	}
	start, err1 := strconv.Atoi(startParts[len(startParts)-1])
	end, err2 := strconv.Atoi(endParts[len(endParts)-1])
	last, err3 := strconv.Atoi(ipParts[len(ipParts)-1])
	if err1 != nil || err2 != nil || err3 != nil {
		return false, "invalid format"
	}
	prefix := strings.Join(startParts[:3], ".")
	ipPrefix := strings.Join(ipParts[:3], ".")
	if ipPrefix != prefix {
		return false, fmt.Sprintf("wrong subnet (expected %s.x)", prefix)
	}
	if start <= last && last <= end {
		return true, "in range"
	}
	return false, fmt.Sprintf("out of range (%s-%s)", cfg["IP_RANGE_START"], cfg["IP_RANGE_END"])
}

// collisionValidatePort mirrors validate_port(port): (valid, reason).
func collisionValidatePort(port string) (bool, string) {
	cfg := collisionLoadConf()
	blacklist := collisionSet(cfg["PORT_BLACKLIST"])
	if blacklist[port] {
		return false, "blacklisted"
	}
	p, errP := strconv.Atoi(port)
	start, err1 := strconv.Atoi(cfg["PORT_RANGE_START"])
	end, err2 := strconv.Atoi(cfg["PORT_RANGE_END"])
	if errP != nil || err1 != nil || err2 != nil {
		return false, "invalid format"
	}
	if start <= p && p <= end {
		return true, "in range"
	}
	// Allow outside range if not blacklisted
	return true, "outside range but not blacklisted"
}

// ── Container checks ──────────────────────────────────────────────────────────

// isLockedContainer mirrors is_locked_container(name).
func isLockedContainer(name string) bool {
	cfg := collisionLoadConf()
	locked := collisionSplitCSV(cfg["IP_PORT_LOCKED_CONTAINERS"])
	if inList(locked, name) {
		return true
	}
	for _, l := range locked {
		if strings.Contains(name, l) || strings.Contains(l, name) {
			return true
		}
	}
	return false
}

// isNetworkModeHost mirrors is_network_mode_host(fpath, svc_name).
func isNetworkModeHost(fpath, svcName string) bool {
	data, err := os.ReadFile(fpath)
	if err != nil {
		return false
	}
	content := string(data)
	// (?s) DOTALL; match container_name block up to next 2-space key or EOF.
	re := regexp.MustCompile(`(?s)container_name:\s*` + regexp.QuoteMeta(svcName) + `(.*?)(?:\n  [a-zA-Z]|\z)`)
	m := re.FindStringSubmatch(content)
	if m != nil && strings.Contains(m[1], "network_mode") && strings.Contains(m[1], "host") {
		return true
	}
	return false
}

// ── Image inspection ──────────────────────────────────────────────────────────

// getImageDefaultPort mirrors get_image_default_port(image).
func getImageDefaultPort(image string) []string {
	m := imageInspect(image)
	cfgRaw, _ := m["Config"].(map[string]interface{})
	if cfgRaw == nil {
		return []string{}
	}
	dataRaw, _ := cfgRaw["ExposedPorts"].(map[string]interface{})
	if len(dataRaw) == 0 {
		return []string{}
	}
	ports := []string{}
	for key := range dataRaw {
		port := strings.SplitN(key, "/", 2)[0]
		if collisionIsDigit(port) {
			ports = append(ports, port)
		}
	}
	sort.Slice(ports, func(i, j int) bool {
		a, _ := strconv.Atoi(ports[i])
		b, _ := strconv.Atoi(ports[j])
		return a < b
	})
	return ports
}

// collisionIsDigit mirrors str.isdigit() for the relevant (ASCII) inputs.
func collisionIsDigit(s string) bool {
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

// ── Scanning (YAML-aware ports parsing) ────────────────────────────────────────

// collisionPortEntry mirrors a parsed (host_ip, host_port) result.
type collisionPortEntry struct {
	IP   string // "" means None
	Port string // "" means None
	OK   bool
}

// parsePortEntry mirrors _parse_port_entry(entry).
// entry may be a string or a dict (map[string]interface{}).
func parsePortEntry(entry interface{}) collisionPortEntry {
	if dict, ok := entry.(map[string]interface{}); ok {
		pub, has := dict["published"]
		if !has || pub == nil {
			return collisionPortEntry{OK: false}
		}
		ip := collisionScalar(dict["host_ip"])
		port := strings.SplitN(collisionScalar(pub), "-", 2)[0]
		return collisionPortEntry{IP: ip, Port: port, OK: true}
	}
	s := collisionScalar(entry)
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"'")
	s = strings.SplitN(s, "/", 2)[0] // drop /tcp,/udp
	parts := strings.Split(s, ":")
	if len(parts) == 3 { // ip:host:container
		return collisionPortEntry{IP: parts[0], Port: parts[1], OK: true}
	}
	if len(parts) == 2 { // host:container (no specific ip)
		return collisionPortEntry{IP: "", Port: parts[0], OK: true}
	}
	return collisionPortEntry{OK: false} // bare container port → not host-published
}

// collisionScalar stringifies a YAML scalar the way Python str() would for the
// values we care about (ints, floats, bools, strings).
func collisionScalar(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "True"
		}
		return "False"
	default:
		return fmt.Sprintf("%v", t)
	}
}

// collisionService mirrors a yielded (container_name, ports_list) pair.
type collisionService struct {
	Name  string
	Ports []interface{}
}

var (
	collisionServicesRe = regexp.MustCompile(`^services:\s*$`)
	collisionTopKeyRe   = regexp.MustCompile(`^\S`)
	collisionSvcKeyRe   = regexp.MustCompile(`^  ([A-Za-z0-9._-]+):\s*$`)
	collisionCnameLnRe  = regexp.MustCompile(`^\s+container_name:\s*([^\s#]+)`)
	collisionPortsKeyRe = regexp.MustCompile(`^    ports:\s*$`)
	collisionPortItemRe = regexp.MustCompile(`^\s*-\s*(.+)$`)
	collision4SpaceKey  = regexp.MustCompile(`^    \S`)
)

// iterServices mirrors _iter_services(fpath): YAML first, regex fallback.
func iterServices(fpath string) []collisionService {
	out := []collisionService{}

	data, err := os.ReadFile(fpath)
	if err != nil {
		return out
	}

	// YAML first.
	var doc map[string]interface{}
	if yaml.Unmarshal(data, &doc) == nil {
		if svcsRaw, ok := doc["services"].(map[string]interface{}); ok {
			// Preserve service order via a yaml.Node pass (Go maps are unordered).
			order := collisionServiceOrder(data)
			seen := map[string]bool{}
			emit := func(svc string) {
				bodyRaw := svcsRaw[svc]
				body, _ := bodyRaw.(map[string]interface{})
				var ports []interface{}
				if body != nil {
					if pr, ok := body["ports"]; ok && pr != nil {
						if lst, ok := pr.([]interface{}); ok {
							ports = lst
						} else {
							ports = []interface{}{pr}
						}
					}
				}
				name := svc
				if body != nil {
					if cn, ok := body["container_name"]; ok && cn != nil {
						if s := collisionScalar(cn); s != "" {
							name = s // Python: container_name or svc (falsy → svc)
						}
					}
				}
				out = append(out, collisionService{Name: name, Ports: ports})
			}
			for _, svc := range order {
				if _, ok := svcsRaw[svc]; ok {
					seen[svc] = true
					emit(svc)
				}
			}
			// any services not captured by the order scan
			for svc := range svcsRaw {
				if !seen[svc] {
					emit(svc)
				}
			}
			return out
		}
	}

	// Fallback: indentation-aware regex.
	var cur, cname string
	var ports []interface{}
	curSet := false
	inPorts := false
	inServices := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\n")
		if collisionServicesRe.MatchString(line) {
			inServices = true
			continue
		}
		if inServices && collisionTopKeyRe.MatchString(line) {
			inServices = false
		}
		if !inServices {
			continue
		}
		if m := collisionSvcKeyRe.FindStringSubmatch(line); m != nil {
			if curSet {
				name := cname
				if name == "" {
					name = cur
				}
				out = append(out, collisionService{Name: name, Ports: ports})
			}
			cur, cname, ports, inPorts = m[1], "", []interface{}{}, false
			curSet = true
			continue
		}
		if !curSet {
			continue
		}
		if mc := collisionCnameLnRe.FindStringSubmatch(line); mc != nil {
			cname = strings.Trim(mc[1], "\"'")
			continue
		}
		if collisionPortsKeyRe.MatchString(line) {
			inPorts = true
			continue
		}
		if inPorts {
			if mp := collisionPortItemRe.FindStringSubmatch(line); mp != nil {
				ports = append(ports, strings.TrimSpace(mp[1]))
			} else if collision4SpaceKey.MatchString(line) {
				inPorts = false
			}
		}
	}
	if curSet {
		name := cname
		if name == "" {
			name = cur
		}
		out = append(out, collisionService{Name: name, Ports: ports})
	}
	return out
}

// collisionServiceOrder extracts service keys in document order from the raw
// compose text (PyYAML preserves insertion order; Go's map does not).
func collisionServiceOrder(data []byte) []string {
	var root yaml.Node
	if yaml.Unmarshal(data, &root) != nil {
		return nil
	}
	if len(root.Content) == 0 {
		return nil
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == "services" {
			svcs := top.Content[i+1]
			if svcs.Kind != yaml.MappingNode {
				return nil
			}
			order := []string{}
			for j := 0; j+1 < len(svcs.Content); j += 2 {
				order = append(order, svcs.Content[j].Value)
			}
			return order
		}
	}
	return nil
}

// collisionBinding mirrors a (cname, ip, port) host binding.
type collisionBinding struct {
	Container string
	IP        string
	Port      string
}

// hostBindings mirrors _host_bindings(fpath): LAN (192.168.x) host-published ports.
func hostBindings(fpath string) []collisionBinding {
	out := []collisionBinding{}
	for _, svc := range iterServices(fpath) {
		for _, entry := range svc.Ports {
			pp := parsePortEntry(entry)
			if !pp.OK {
				continue
			}
			ip, port := pp.IP, pp.Port
			if ip != "" && strings.HasPrefix(ip, "192.168.") && port != "" && collisionIsDigit(port) {
				out = append(out, collisionBinding{Container: svc.Name, IP: ip, Port: port})
			}
		}
	}
	return out
}

// collisionGlobYML mirrors sorted(glob.glob(f"{STACKS_DIR}/*.yml")).
func collisionGlobYML() []string {
	matches, _ := filepath.Glob(filepath.Join(collisionStacksDir(), "*.yml"))
	sort.Strings(matches)
	return matches
}

// scanAllIPs mirrors scan_all_ips(): {ip: [(stack, container)]}.
func scanAllIPs() map[string][]collisionOwner {
	ipMap := map[string][]collisionOwner{}
	for _, fpath := range collisionGlobYML() {
		stack := strings.Replace(filepath.Base(fpath), ".yml", "", 1)
		for _, b := range hostBindings(fpath) {
			e := collisionOwner{Stack: stack, Container: b.Container}
			if !collisionOwnerInList(ipMap[b.IP], e) {
				ipMap[b.IP] = append(ipMap[b.IP], e)
			}
		}
	}
	return ipMap
}

// scanAllPorts mirrors scan_all_ports(): {"ip:port": [(stack, container)]}.
func scanAllPorts() map[string][]collisionOwner {
	portMap := map[string][]collisionOwner{}
	for _, fpath := range collisionGlobYML() {
		stack := strings.Replace(filepath.Base(fpath), ".yml", "", 1)
		for _, b := range hostBindings(fpath) {
			key := b.IP + ":" + b.Port
			e := collisionOwner{Stack: stack, Container: b.Container}
			if !collisionOwnerInList(portMap[key], e) {
				portMap[key] = append(portMap[key], e)
			}
		}
	}
	return portMap
}

// collisionOwnerInList mirrors `if e not in list`.
func collisionOwnerInList(l []collisionOwner, e collisionOwner) bool {
	for _, x := range l {
		if x == e {
			return true
		}
	}
	return false
}

// collisionRunningNames mirrors _running_names() via the shared Docker layer.
func collisionRunningNames() map[string]bool {
	return runningNames()
}

// getCollisions mirrors get_collisions(): (ipCollisions, portCollisions).
func getCollisions() ([]collisionIPRec, []collisionPortRec) {
	cfg := collisionLoadConf()
	ipMap := scanAllIPs()
	portMap := scanAllPorts()
	blacklistIPs := collisionSet(cfg["IP_BLACKLIST"])
	blacklistPorts := collisionSet(cfg["PORT_BLACKLIST"])
	running := collisionRunningNames()

	ipCollisions := []collisionIPRec{}
	for ip, owners := range ipMap {
		// IP sharing is OK - only flag blacklisted IPs
		if blacklistIPs[ip] {
			ipCollisions = append(ipCollisions, collisionIPRec{IP: ip, Owners: owners, Type: "blacklisted"})
		}
	}

	portCollisions := []collisionPortRec{}
	for key, owners := range portMap {
		kp := strings.SplitN(key, ":", 2)
		ip, port := kp[0], kp[1]
		run := []string{}
		for _, o := range owners {
			if running[o.Container] {
				run = append(run, o.Container)
			}
		}
		if len(owners) > 1 {
			portCollisions = append(portCollisions, collisionPortRec{
				IP: ip, Port: port, Owners: owners, Type: "duplicate",
				Running: run, Active: len(run) >= 2,
			})
		} else if blacklistPorts[port] {
			portCollisions = append(portCollisions, collisionPortRec{
				IP: ip, Port: port, Owners: owners, Type: "blacklisted",
				Running: run, Active: false,
			})
		}
	}

	// Sort active (live) collisions first.
	sort.SliceStable(portCollisions, func(i, j int) bool {
		a, b := portCollisions[i], portCollisions[j]
		// key = (not active, ip, port)
		ai, bi := !a.Active, !b.Active
		if ai != bi {
			return !ai // false (active) sorts before true
		}
		if a.IP != b.IP {
			return a.IP < b.IP
		}
		return a.Port < b.Port
	})
	return ipCollisions, portCollisions
}

// ── Assignment ────────────────────────────────────────────────────────────────

// getNextAvailableIP mirrors get_next_available_ip(): IP string or "".
func getNextAvailableIP() string {
	cfg := collisionLoadConf()
	ipMap := scanAllIPs()
	used := map[string]bool{}
	for ip := range ipMap {
		used[ip] = true
	}
	blacklist := collisionSet(cfg["IP_BLACKLIST"])
	locked := collisionSet(cfg["LOCKED_IPS"])
	whitelist := collisionSplitCSV(cfg["IP_WHITELIST"])

	blocked := func(ip string) bool { return used[ip] || blacklist[ip] || locked[ip] }

	if len(whitelist) > 0 {
		for _, ip := range whitelist {
			if !blocked(ip) {
				return ip
			}
		}
		return ""
	}

	start, prefix, end, ok := collisionRangeBounds(cfg)
	if !ok {
		return ""
	}
	for i := start; i <= end; i++ {
		ip := fmt.Sprintf("%s.%d", prefix, i)
		if !blocked(ip) {
			return ip
		}
	}
	return ""
}

// collisionRangeBounds mirrors the (start, prefix, end) extraction.
func collisionRangeBounds(cfg map[string]string) (start int, prefix string, end int, ok bool) {
	sParts := strings.Split(cfg["IP_RANGE_START"], ".")
	eParts := strings.Split(cfg["IP_RANGE_END"], ".")
	if len(sParts) < 4 || len(eParts) < 4 {
		return 0, "", 0, false
	}
	s, err1 := strconv.Atoi(sParts[len(sParts)-1])
	e, err2 := strconv.Atoi(eParts[len(eParts)-1])
	if err1 != nil || err2 != nil {
		return 0, "", 0, false
	}
	return s, strings.Join(sParts[:3], "."), e, true
}

// getNextAvailablePort mirrors get_next_available_port(ip, preferred_port="").
func getNextAvailablePort(ip string, preferredPort string) string {
	cfg := collisionLoadConf()
	portMap := scanAllPorts()
	blacklist := collisionSet(cfg["PORT_BLACKLIST"])

	usedPorts := map[string]bool{}
	for key := range portMap {
		kp := strings.SplitN(key, ":", 2)
		if kp[0] == ip {
			usedPorts[kp[1]] = true
		}
	}

	if preferredPort != "" {
		p := preferredPort
		if !usedPorts[p] && !blacklist[p] {
			return p
		}
	}

	start, err1 := strconv.Atoi(cfg["PORT_RANGE_START"])
	end, err2 := strconv.Atoi(cfg["PORT_RANGE_END"])
	if err1 != nil || err2 != nil {
		return ""
	}
	for port := start; port <= end; port++ {
		p := strconv.Itoa(port)
		if !usedPorts[p] && !blacklist[p] {
			return p
		}
	}
	return ""
}

// findIPWithFreePort mirrors find_ip_with_free_port(port): (ip, port) or ("","").
func findIPWithFreePort(port string) (string, string) {
	cfg := collisionLoadConf()
	portMap := scanAllPorts()
	blacklistIPs := collisionSet(cfg["IP_BLACKLIST"])
	blacklistPorts := collisionSet(cfg["PORT_BLACKLIST"])
	locked := collisionSet(cfg["LOCKED_IPS"])
	whitelist := collisionSplitCSV(cfg["IP_WHITELIST"])

	if blacklistPorts[port] {
		return "", ""
	}

	used := map[string]bool{}
	for k := range portMap {
		used[k] = true
	}

	var ipsToTry []string
	if len(whitelist) > 0 {
		ipsToTry = whitelist
	} else {
		start, prefix, end, ok := collisionRangeBounds(cfg)
		if ok {
			for i := start; i <= end; i++ {
				ipsToTry = append(ipsToTry, fmt.Sprintf("%s.%d", prefix, i))
			}
		}
	}

	hostPorts := scanHostPorts()

	for _, ip := range ipsToTry {
		if blacklistIPs[ip] || locked[ip] {
			continue
		}
		if !used[ip+":"+port] {
			if ps, ok := hostPorts[ip]; ok && ps[port] {
				continue
			}
			return ip, port
		}
	}
	return "", ""
}

// ── Sticky assignment ledger ──────────────────────────────────────────────────

// loadLedger mirrors load_ledger(): container_name -> 'ip:port'.
func loadLedger() map[string]string {
	led := map[string]string{}
	data, err := os.ReadFile(collisionLedgerFile())
	if err != nil {
		return led
	}
	for _, line := range strings.Split(string(data), "\n") {
		l := strings.TrimSpace(line)
		if strings.Contains(l, "=") && !strings.HasPrefix(l, "#") {
			parts := strings.SplitN(l, "=", 2)
			led[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return led
}

// saveLedger mirrors save_ledger(led).
func saveLedger(led map[string]string) {
	if err := os.MkdirAll(filepath.Dir(collisionLedgerFile()), 0755); err != nil {
		return
	}
	keys := make([]string, 0, len(led))
	for k := range led {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# container_name=ip:port — remembered IP assignments (stops rotation)\n")
	for _, k := range keys {
		b.WriteString(fmt.Sprintf("%s=%s\n", k, led[k]))
	}
	os.WriteFile(collisionLedgerFile(), []byte(b.String()), 0644)
}

// stableAssign mirrors stable_assign(cname, port): (ip, port) or ("","").
func stableAssign(cname string, port string) (string, string) {
	cfg := collisionLoadConf()
	led := loadLedger()
	portMap := scanAllPorts()
	hostPorts := scanHostPorts()
	blacklistIPs := collisionSet(cfg["IP_BLACKLIST"])
	locked := collisionSet(cfg["LOCKED_IPS"])
	whitelist := collisionSplitCSV(cfg["IP_WHITELIST"])
	blacklistPorts := collisionSet(cfg["PORT_BLACKLIST"])

	free := func(ip, prt string) bool {
		if ip == "" || blacklistIPs[ip] || locked[ip] {
			return false
		}
		// another container already holding ip:prt blocks it (our own slot is OK)
		for _, o := range portMap[ip+":"+prt] {
			if o.Container != cname {
				return false
			}
		}
		if ps, ok := hostPorts[ip]; ok && ps[prt] {
			return false
		}
		return true
	}

	var ips []string
	if len(whitelist) > 0 {
		ips = whitelist
	} else {
		start, prefix, end, ok := collisionRangeBounds(cfg)
		if ok {
			for i := start; i <= end; i++ {
				ips = append(ips, fmt.Sprintf("%s.%d", prefix, i))
			}
		}
	}

	// 1) reuse remembered assignment
	rec := led[cname]
	if strings.HasSuffix(rec, ":"+port) {
		recIP := strings.SplitN(rec, ":", 2)[0]
		if free(recIP, port) {
			return recIP, port
		}
	}

	// 2) first-fit on the DEFAULT port
	for _, ip := range ips {
		if free(ip, port) {
			led[cname] = ip + ":" + port
			saveLedger(led)
			return ip, port
		}
	}

	// 3) LAST RESORT — change the port
	pStart, err1 := strconv.Atoi(cfg["PORT_RANGE_START"])
	pEnd, err2 := strconv.Atoi(cfg["PORT_RANGE_END"])
	if err1 != nil || err2 != nil {
		pStart, pEnd = 8080, 8999
	}
	for _, ip := range ips {
		if blacklistIPs[ip] || locked[ip] {
			continue
		}
		for np := pStart; np <= pEnd; np++ {
			nps := strconv.Itoa(np)
			if blacklistPorts[nps] {
				continue
			}
			if free(ip, nps) {
				led[cname] = ip + ":" + nps
				saveLedger(led)
				return ip, nps
			}
		}
	}
	return "", ""
}

// collisionImagePortRe mirrors the __main__ --find-port image lookup regex.
var collisionImagePortRe = func(svc string) *regexp.Regexp {
	return regexp.MustCompile(`(?s)container_name:\s*` + regexp.QuoteMeta(svc) + `.*?image:\s*(\S+)`)
}

// collisionFindPort mirrors the `--find-port PORT SVC FILE` CLI mode.
// Returns ("ip:port", true) on success, or ("", false) on failure (exit 1).
func collisionFindPort(port, svc, file string) (string, bool) {
	if isLockedContainer(svc) {
		return "", false
	}
	newIP, newPort := findIPWithFreePort(port)
	if newIP == "" {
		if data, err := os.ReadFile(file); err == nil {
			m := collisionImagePortRe(svc).FindStringSubmatch(string(data))
			if m != nil {
				for _, p := range getImageDefaultPort(strings.TrimSpace(m[1])) {
					newIP, newPort = findIPWithFreePort(p)
					if newIP != "" {
						break
					}
				}
			}
		}
	}
	if newIP != "" {
		return fmt.Sprintf("%s:%s", newIP, newPort), true
	}
	return "", false
}

// collisionMain mirrors the __main__ normal-mode diagnostic printout.
func collisionMain(args []string) {
	// --find-port PORT SVC FILE mode
	if len(args) >= 4 && args[0] == "--find-port" {
		res, ok := collisionFindPort(args[1], args[2], args[3])
		if ok {
			fmt.Println(res)
			os.Exit(0)
		}
		os.Exit(1)
	}
	cfg := collisionLoadConf()
	fmt.Printf("IP Range:  %s → %s\n", cfg["IP_RANGE_START"], cfg["IP_RANGE_END"])
	fmt.Printf("Port Range: %s → %s\n", cfg["PORT_RANGE_START"], cfg["PORT_RANGE_END"])
	fmt.Printf("Blacklist IPs:  %s\n", cfg["IP_BLACKLIST"])
	fmt.Printf("Blacklist Ports:%s\n", cfg["PORT_BLACKLIST"])
	fmt.Printf("Locked containers: %s\n", cfg["IP_PORT_LOCKED_CONTAINERS"])
	fmt.Println()
	ipCol, portCol := getCollisions()
	fmt.Printf("IP collisions:   %d\n", len(ipCol))
	for i, c := range ipCol {
		if i >= 5 {
			break
		}
		fmt.Printf("  %-12s %-18s %s\n", c.Type, c.IP, collisionOwnersStr(c.Owners))
	}
	fmt.Printf("Port collisions: %d\n", len(portCol))
	for i, c := range portCol {
		if i >= 5 {
			break
		}
		fmt.Printf("  %-12s %s:%-8s %s\n", c.Type, c.IP, c.Port, collisionOwnersStr(c.Owners))
	}
	fmt.Println()
	nextIP := getNextAvailableIP()
	fmt.Printf("Next available IP: %s\n", collisionNoneStr(nextIP))
	nextPort := ""
	if nextIP != "" {
		nextPort = getNextAvailablePort(nextIP, "")
	}
	fmt.Printf("Next available port on %s: %s\n", collisionNoneStr(nextIP), collisionNoneStr(nextPort))
	fmt.Println()
	if len(args) > 0 {
		img := args[0]
		ports := getImageDefaultPort(img)
		fmt.Printf("Default ports for %s: %s\n", img, collisionStrListRepr(ports))
	}
	r1ip, r1p := findIPWithFreePort("8080")
	fmt.Printf("IP with free port 8080: %s\n", collisionTupleRepr(r1ip, r1p))
	r2ip, r2p := findIPWithFreePort("8443")
	fmt.Printf("IP with free port 8443: %s\n", collisionTupleRepr(r2ip, r2p))
}

// collisionNoneStr renders an empty string as Python would print None.
func collisionNoneStr(s string) string {
	if s == "" {
		return "None"
	}
	return s
}

// collisionStrListRepr renders a []string like Python's repr of a list of strs:
// ['8080', '443'] (empty → []).
func collisionStrListRepr(items []string) string {
	parts := make([]string, 0, len(items))
	for _, s := range items {
		parts = append(parts, "'"+s+"'")
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// collisionTupleRepr renders an (ip, port) result like Python's tuple repr:
// ('192.168.1.153', '8080') or (None, None) when empty.
func collisionTupleRepr(ip, port string) string {
	a := "None"
	if ip != "" {
		a = "'" + ip + "'"
	}
	b := "None"
	if port != "" {
		b = "'" + port + "'"
	}
	return "(" + a + ", " + b + ")"
}

// collisionOwnersStr renders an owners slice like Python's list-of-tuples repr.
func collisionOwnersStr(owners []collisionOwner) string {
	parts := make([]string, 0, len(owners))
	for _, o := range owners {
		parts = append(parts, fmt.Sprintf("('%s', '%s')", o.Stack, o.Container))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
