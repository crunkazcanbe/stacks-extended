package lib

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	yaml "gopkg.in/yaml.v3"
)

// ===== from collision.go =====

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
	collisionCnameRe  = regexp.MustCompile(`container_name:\s*(\S+)`)
	collisionNetRe    = regexp.MustCompile(`(\w+_net)\s*:`)
	collisionDependRe = regexp.MustCompile(`-\s+[A-Za-z][\w-]+`)
	collisionURLRefRe = regexp.MustCompile(`(?:https?|redis|postgres|mysql|mongo)://[^@\s]*@?([a-zA-Z][a-zA-Z0-9_-]+):\d+`)
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

// ===== from netguardian.go =====

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

// ===== from netdedupe.go =====

// netdedupe.go — `stacks netdedupe` (report): find networks declared as a creator
// (external:false) in more than one stack — those throw the "network exists but was
// not created for project" warning. Owner = same category priority as dedupe; the
// rest should become external:true. *-ext (VPS) stacks are skipped.

var topNetRe = regexp.MustCompile(`^  ([A-Za-z0-9_.-]+):\s*\{(.*)\}\s*$`)

// scanNetCreators -> map[netname][]stack where the net is declared external:false.
func scanNetCreators() map[string][]string {
	out := map[string][]string{}
	dir := stacksDir()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".yml") || strings.HasSuffix(n, "-ext.yml") {
			continue
		}
		stack := strings.TrimSuffix(n, ".yml")
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		intop := false
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "networks:") {
				intop = true
				continue
			}
			if len(line) > 0 && line[0] != ' ' && line[0] != '\t' {
				intop = false
			}
			if intop {
				if m := topNetRe.FindStringSubmatch(line); m != nil && strings.Contains(m[2], "external: false") {
					out[m[1]] = append(out[m[1]], stack)
				}
			}
		}
	}
	return out
}

func netOwner(stacks []string) string {
	owner := stacks[0]
	for _, s := range stacks[1:] {
		if prefixPriority(s) < prefixPriority(owner) ||
			(prefixPriority(s) == prefixPriority(owner) && stackNum(s) < stackNum(owner)) {
			owner = s
		}
	}
	return owner
}

func cmdNetdedupe(args []string) {
	dupes := map[string][]string{}
	for name, stacks := range scanNetCreators() {
		if len(stacks) > 1 {
			dupes[name] = stacks
		}
	}
	if len(dupes) == 0 {
		fmt.Println("✓ No double-creator networks — every network is created by exactly one stack.")
		return
	}
	names := make([]string, 0, len(dupes))
	for n := range dupes {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Printf("⚠ %d network(s) declared external:false in more than one stack:\n\n", len(dupes))
	for _, name := range names {
		stacks := dupes[name]
		sort.Strings(stacks)
		owner := netOwner(stacks)
		for _, s := range stacks {
			tag := "  → set external:true"
			if s == owner {
				tag = "  ← OWNER (keeps external:false)"
			}
			fmt.Printf("  %-22s %s%s\n", name, s, tag)
		}
		fmt.Println()
	}
}

// ===== from dedupe.go =====

// dedupe.go — `stacks dedupe` (report): find container_names declared in more than
// one stack, and recommend which copy to keep. Keeper rule (matches the Python
// version): the stack running the live container wins; else category priority
// core>db>net>ai>data>srvs>dev (dev/scratch loses), tie-broken by lower number.
// *-ext (VPS) stacks are skipped.

var keeperPriority = []string{"core", "db", "net", "ai", "data", "srvs", "dev"}

var cnameRe = regexp.MustCompile(`^\s+container_name:\s*"?([A-Za-z0-9_.-]+)`)
var numRe = regexp.MustCompile(`(\d+)`)

// scanContainerNames -> map[container_name][]stack across local (non -ext) stacks.
func scanContainerNames() map[string][]string {
	out := map[string][]string{}
	dir := stacksDir()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".yml") || strings.HasSuffix(n, "-ext.yml") {
			continue
		}
		stack := strings.TrimSuffix(n, ".yml")
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if m := cnameRe.FindStringSubmatch(line); m != nil {
				out[m[1]] = append(out[m[1]], stack)
			}
		}
	}
	return out
}

func prefixPriority(stack string) int {
	pfx := stack
	if i := strings.IndexAny(stack, "_-"); i >= 0 {
		pfx = stack[:i]
	}
	for idx, p := range keeperPriority {
		if p == pfx {
			return idx
		}
	}
	return len(keeperPriority)
}

func stackNum(stack string) int {
	if m := numRe.FindString(stack); m != "" {
		if n, err := strconv.Atoi(m); err == nil {
			return n
		}
	}
	return 9999
}

func recommendKeeper(name string, stacks []string, info map[string]ctrInfo) (string, string) {
	if v, ok := info[name]; ok && v.Project != "" {
		for _, s := range stacks {
			if s == v.Project {
				return s, "owns the live container (" + v.State + ")"
			}
		}
	}
	best := stacks[0]
	for _, s := range stacks[1:] {
		if prefixPriority(s) < prefixPriority(best) ||
			(prefixPriority(s) == prefixPriority(best) && stackNum(s) < stackNum(best)) {
			best = s
		}
	}
	return best, "category match (keep " + strings.SplitN(best, "_", 2)[0] + ", dev/scratch loses)"
}

func cmdDedupe(args []string) {
	dupes := map[string][]string{}
	for name, stacks := range scanContainerNames() {
		if len(stacks) > 1 {
			dupes[name] = stacks
		}
	}
	if len(dupes) == 0 {
		fmt.Println("✓ No duplicate container_names across stacks.")
		return
	}
	info := containerInfo()
	names := make([]string, 0, len(dupes))
	for n := range dupes {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Printf("⚠ %d duplicate container_name(s) — only one of each can run:\n\n", len(dupes))
	for _, name := range names {
		stacks := dupes[name]
		sort.Strings(stacks)
		keep, why := recommendKeeper(name, stacks, info)
		for _, s := range stacks {
			tag := ""
			if s == keep {
				tag = "  ← keep (" + why + ")"
			}
			fmt.Printf("  %-22s %s%s\n", name, s, tag)
		}
		fmt.Println()
	}
}
