package main

// gendynamic.go — faithful Go port of stacks_gen_dynamic.py.
// Auto-generates Traefik dynamic config files. Scans compose stacks and
// generates routers, services, middlewares, TCP routes. Config-driven: reads
// from stacks.conf / stacks.yaml for domains, URLs, feature flags.
// Universal paths: stacksDir()/configDir()/home() — nothing machine-hardcoded.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ── Config defaults ──────────────────────────────────────────────────────────
var genDynDefaults = map[string]string{
	"PRIMARY_DOMAIN":   "loveiznothin.com",
	"SECONDARY_DOMAIN": "bellzserver.cloud",
	"AUTHENTIK_URL":    "http://authentik_server:9000",
	"CROWDSEC_URL":     "http://crowdsec_bouncer:8080",
	"SABLIER_URL":      "http://sablier:10000",
	"SABLIER_THEME":    "ghost",
	"SABLIER_DURATION": "1h",
	"GEN_ROUTERS":      "1",
	"GEN_SERVICES":     "1",
	"GEN_MIDDLEWARES":  "1",
	"GEN_SABLIER":      "1",
	"GEN_TCP":          "1",
	"GEN_AUTH":         "1", // include authentik middleware (Authentik = the main gate)
	"GEN_CROWDSEC":     "1", // include crowdsec middleware
	"GEN_DOMAIN":       "primary", // primary|secondary|both
	// ── Security hardening (config-toggleable; safe defaults) ─────────────────
	"GEN_PERMISSIONS_POLICY": "1", // add a Permissions-Policy response header (safe, pure addition)
	"PERMISSIONS_POLICY":     "camera=(), microphone=(), geolocation=(), payment=(), usb=(), interest-cohort=()",
	"GEN_CSP":                "0", // add Content-Security-Policy (OFF by default — CSP breaks many apps)
	"CSP_POLICY":             "default-src 'self'; img-src 'self' data: https:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; connect-src 'self' https: wss:; font-src 'self' data:; frame-ancestors 'self'",
	"GEN_CF_IPALLOW":         "0", // restrict origin to Cloudflare + LAN IPs (OFF by default — can lock out access)
	"CF_TRUSTED_IPS":         "",  // extra comma-separated CIDRs to always allow (e.g. your VPN subnet)
}

// Cloudflare published edge ranges (IPv4 + IPv6) + private LAN, used by the
// optional cloudflare-ipallow middleware. Update from https://www.cloudflare.com/ips/
var genDynCloudflareIPs = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
}

var genDynPrivateLANIPs = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.1/32"}

// TCP database port map. genDynTCPOrder preserves the Python dict iteration order
// (matters for is_tcp_service / get_tcp_port first-match semantics).
var genDynTCPOrder = []string{
	"postgres", "postgresql", "mysql", "mariadb", "mongo", "mongodb",
	"redis", "mssql", "neo4j",
}
var genDynTCPPorts = map[string]int{
	"postgres": 5432, "postgresql": 5432,
	"mysql": 3306, "mariadb": 3306,
	"mongo": 27017, "mongodb": 27017,
	"redis": 6379,
	"mssql": 1433,
	"neo4j": 7687,
}

// genDynPortMapOrder preserves Python dict order for image -> port defaults.
var genDynPortMapOrder = []string{
	"nginx", "apache", "caddy", "grafana", "prometheus", "gitea",
	"nextcloud", "vaultwarden", "portainer",
}
var genDynPortMap = map[string]int{
	"nginx": 80, "apache": 80, "caddy": 80,
	"grafana": 3000, "prometheus": 9090,
	"gitea": 3000, "nextcloud": 80,
	"vaultwarden": 80, "portainer": 9000,
}

var genDynLBPortRe = regexp.MustCompile(`loadbalancer\.server\.port=(\d+)`)
var genDynHostRuleRe = regexp.MustCompile("rule=Host\\(`([^.]+)\\.")

// loadConf mirrors load_conf(): start with DEFAULTS, overlay stacks.conf
// (KEY=VALUE), then overlay the YAML master (stacks.yaml wins) via configLoad().
func genDynLoadConf() map[string]string {
	cfg := map[string]string{}
	for k, v := range genDynDefaults {
		cfg[k] = v
	}
	confPath := filepath.Join(configDir(), "stacks.conf")
	if data, err := os.ReadFile(confPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.Contains(line, "=") && !strings.HasPrefix(line, "#") {
				k, v, _ := strings.Cut(line, "=")
				cfg[strings.TrimSpace(k)] = strings.Trim(strings.Trim(strings.TrimSpace(v), `"`), `'`)
			}
		}
	}
	// YAML master overlay (stacks.yaml wins)
	for k, v := range configLoad() {
		cfg[k] = v
	}
	return cfg
}

// genDynLabelStrings normalizes a service's labels into a []string of "k=v"
// (or the raw list entries), mirroring the Python isinstance(labels, dict) branch.
func genDynLabelStrings(svc map[string]interface{}) []string {
	raw, ok := svc["labels"]
	if !ok || raw == nil {
		return nil
	}
	switch t := raw.(type) {
	case map[string]interface{}:
		var out []string
		for k, v := range t {
			out = append(out, fmt.Sprintf("%s=%s", k, genDynScalar(v)))
		}
		return out
	case []interface{}:
		var out []string
		for _, x := range t {
			out = append(out, genDynScalar(x))
		}
		return out
	default:
		return nil
	}
}

// genDynScalar renders a YAML scalar the way Python's str()/f-string would for
// labels (no "1"/"0" coercion — that's only for the config loader).
func genDynScalar(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case bool:
		if t {
			return "True"
		}
		return "False"
	default:
		return fmt.Sprint(t)
	}
}

func genDynStr(svc map[string]interface{}, key string) string {
	if v, ok := svc[key]; ok && v != nil {
		return genDynScalar(v)
	}
	return ""
}

// getServicePort: extract port from traefik label or common defaults.
func genDynGetServicePort(svc map[string]interface{}) int {
	for _, l := range genDynLabelStrings(svc) {
		if m := genDynLBPortRe.FindStringSubmatch(l); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return n
			}
		}
	}
	image := strings.ToLower(genDynStr(svc, "image"))
	for _, k := range genDynPortMapOrder {
		if strings.Contains(image, k) {
			return genDynPortMap[k]
		}
	}
	return 80
}

// isTCPService: check if service is a TCP database.
func genDynIsTCPService(name string, svc map[string]interface{}) bool {
	nameLower := strings.ToLower(name)
	for _, db := range genDynTCPOrder {
		if strings.Contains(nameLower, db) {
			return true
		}
	}
	image := strings.ToLower(genDynStr(svc, "image"))
	for _, db := range genDynTCPOrder {
		if strings.Contains(image, db) {
			return true
		}
	}
	return false
}

// getTCPPort: returns (port, ok). ok=false mirrors Python returning None.
func genDynGetTCPPort(name string, svc map[string]interface{}) (int, bool) {
	nameLower := strings.ToLower(name)
	for _, db := range genDynTCPOrder {
		if strings.Contains(nameLower, db) {
			return genDynTCPPorts[db], true
		}
	}
	image := strings.ToLower(genDynStr(svc, "image"))
	for _, db := range genDynTCPOrder {
		if strings.Contains(image, db) {
			return genDynTCPPorts[db], true
		}
	}
	return 0, false
}

// serviceHasTraefik.
func genDynServiceHasTraefik(svc map[string]interface{}) bool {
	raw, ok := svc["labels"]
	if ok && raw != nil {
		if m, isMap := raw.(map[string]interface{}); isMap {
			v := "false"
			if x, ok := m["traefik.enable"]; ok {
				v = genDynScalar(x)
			}
			return strings.ToLower(v) == "true"
		}
		if lst, isList := raw.([]interface{}); isList {
			for _, l := range lst {
				if strings.Contains(strings.ToLower(genDynScalar(l)), "traefik.enable=true") {
					return true
				}
			}
		}
	}
	return false
}

// serviceSablierEnabled: False if explicitly sablier.enable=false (always-on,
// never sleeps). Always-on services must NOT get a Sablier middleware.
func genDynServiceSablierEnabled(svc map[string]interface{}) bool {
	raw, ok := svc["labels"]
	if ok && raw != nil {
		if m, isMap := raw.(map[string]interface{}); isMap {
			v := "true"
			if x, ok := m["sablier.enable"]; ok {
				v = genDynScalar(x)
			}
			return strings.ToLower(v) != "false"
		}
		if lst, isList := raw.([]interface{}); isList {
			for _, l := range lst {
				if strings.Contains(strings.ToLower(genDynScalar(l)), "sablier.enable=false") {
					return false
				}
			}
		}
	}
	return true
}

// getSubdomain: from traefik label or derive from name.
func genDynGetSubdomain(name string, svc map[string]interface{}) string {
	for _, l := range genDynLabelStrings(svc) {
		if m := genDynHostRuleRe.FindStringSubmatch(l); m != nil {
			return m[1]
		}
	}
	cname := name
	if c := genDynStr(svc, "container_name"); c != "" {
		cname = c
	}
	return strings.ToLower(strings.ReplaceAll(cname, "_", "-"))
}

func genDynGenRouter(name, subdomain, domain, svcName, sablierMW string, cfg map[string]string) string {
	var mws []string
	if cfg["GEN_CF_IPALLOW"] == "1" {
		mws = append(mws, "cloudflare-ipallow")
	}
	if sablierMW != "" {
		mws = append(mws, sablierMW)
	}
	mws = append(mws, "https-header")
	if cfg["GEN_CROWDSEC"] == "1" {
		mws = append(mws, "crowdsec_bouncer")
	}
	if cfg["GEN_AUTH"] == "1" {
		mws = append(mws, "authentik-auth")
	}
	mws = append(mws, "global-retry", "compress", "inflight", "buffering", "rate-limit")
	mwStr := strings.Join(mws, ", ")
	return fmt.Sprintf(
		"    %s-router:\n"+
			"      rule: \"Host(`%s.%s`)\"\n"+
			"      service: %s\n"+
			"      entryPoints: [web]\n"+
			"      middlewares: [%s]\n",
		name, subdomain, domain, svcName, mwStr)
}

func genDynGenService(name, container string, port int) string {
	return fmt.Sprintf(
		"    %s-svc:\n"+
			"      loadBalancer:\n"+
			"        servers: [{ url: \"http://%s:%d\" }]\n",
		name, container, port)
}

func genDynGenSablierMW(name, container string, cfg map[string]string) string {
	return fmt.Sprintf(
		"    sablier-%s:\n"+
			"      plugin:\n"+
			"        sablier:\n"+
			"          sablierUrl: \"%s\"\n"+
			"          sessionDuration: \"%s\"\n"+
			"          names: \"%s\"\n"+
			"          dynamic:\n"+
			"            displayName: \"%s\"\n"+
			"            provider: \"docker\"\n"+
			"            stopTimeout: \"30s\"\n"+
			"            refreshFrequency: \"5s\"\n"+
			"            theme: \"%s\"\n"+
			"            timeout: \"10m\"\n"+
			"            warmupPeriod: \"10s\"\n"+
			"            healthCheckPath: \"/\"\n"+
			"            healthCheckInterval: \"2s\"\n"+
			"            scaling:\n"+
			"              replicas: 1\n"+
			"              minReplicas: 0\n"+
			"              maxReplicas: 1\n",
		name, cfg["SABLIER_URL"], cfg["SABLIER_DURATION"], container, container, cfg["SABLIER_THEME"])
}

func genDynGenTCPRouter(name, subdomain, domain string, port int) string {
	return fmt.Sprintf(
		"    %s-tcp:\n"+
			"      rule: \"HostSNI(`%s.%s`)\"\n"+
			"      entryPoints: [websecure]\n"+
			"      service: %s-tcp-svc\n"+
			"      tls:\n"+
			"        passthrough: true\n",
		name, subdomain, domain, name)
}

func genDynGenTCPService(name, container string, port int) string {
	return fmt.Sprintf(
		"    %s-tcp-svc:\n"+
			"      loadBalancer:\n"+
			"        servers:\n"+
			"          - address: \"%s:%d\"\n",
		name, container, port)
}

func genDynGenCloudflareIPAllow(cfg map[string]string) string {
	var extra []string
	for _, c := range strings.Split(cfg["CF_TRUSTED_IPS"], ",") {
		if s := strings.TrimSpace(c); s != "" {
			extra = append(extra, s)
		}
	}
	var ranges []string
	ranges = append(ranges, genDynCloudflareIPs...)
	ranges = append(ranges, genDynPrivateLANIPs...)
	ranges = append(ranges, extra...)
	var lineParts []string
	for _, r := range ranges {
		lineParts = append(lineParts, fmt.Sprintf("          - \"%s\"", r))
	}
	lines := strings.Join(lineParts, "\n")
	return "    cloudflare-ipallow:\n" +
		"      ipAllowList:\n" +
		"        sourceRange:\n" +
		lines + "\n" +
		"        ipStrategy:\n" +
		"          depth: 1\n"
}

func genDynGenStandardMiddlewares(cfg map[string]string) string {
	authURL := cfg["AUTHENTIK_URL"]
	crowdsecURL := cfg["CROWDSEC_URL"]
	extraHdrs := ""
	if cfg["GEN_PERMISSIONS_POLICY"] == "1" {
		extraHdrs += fmt.Sprintf("          Permissions-Policy: \"%s\"\n", cfg["PERMISSIONS_POLICY"])
	}
	if cfg["GEN_CSP"] == "1" {
		extraHdrs += fmt.Sprintf("          Content-Security-Policy: \"%s\"\n", cfg["CSP_POLICY"])
	}
	out := "\n" +
		"    https-header:\n" +
		"      headers:\n" +
		"        customRequestHeaders:\n" +
		"          X-Forwarded-Proto: \"https\"\n" +
		"        customResponseHeaders:\n" +
		"          X-Frame-Options: \"SAMEORIGIN\"\n" +
		"          X-Content-Type-Options: \"nosniff\"\n" +
		"          X-XSS-Protection: \"1; mode=block\"\n" +
		"          Referrer-Policy: \"strict-origin-when-cross-origin\"\n" +
		"          Strict-Transport-Security: \"max-age=31536000; includeSubDomains; preload\"\n" +
		"          Server: \"\"\n" +
		"          X-Robots-Tag: \"noindex, nofollow\"\n" +
		extraHdrs
	if cfg["GEN_CF_IPALLOW"] == "1" {
		out += "\n" + genDynGenCloudflareIPAllow(cfg) + "\n"
	}
	out += "\n" +
		"\n" +
		"    global-retry:\n" +
		"      retry:\n" +
		"        attempts: 3\n" +
		"        initialInterval: 100ms\n" +
		"\n" +
		"    compress:\n" +
		"      compress:\n" +
		"        minResponseBodyBytes: 1024\n" +
		"        encodings: [zstd, br, gzip]\n" +
		"\n" +
		"    inflight:\n" +
		"      inFlightReq:\n" +
		"        amount: 100\n" +
		"        sourceCriterion:\n" +
		"          ipStrategy: { depth: 1 }\n" +
		"\n" +
		"    buffering:\n" +
		"      buffering:\n" +
		"        maxRequestBodyBytes: 10485760\n" +
		"        memRequestBodyBytes: 2097152\n" +
		"        maxResponseBodyBytes: 10485760\n" +
		"        memResponseBodyBytes: 2097152\n" +
		"        retryExpression: \"IsNetworkError() && Attempts() < 3\"\n" +
		"\n" +
		"    rate-limit:\n" +
		"      rateLimit:\n" +
		"        average: 100\n" +
		"        burst: 50\n" +
		"        period: 1s\n" +
		"        sourceCriterion:\n" +
		"          ipStrategy: { depth: 1 }\n" +
		"\n" +
		fmt.Sprintf("    authentik-auth:\n"+
			"      forwardAuth:\n"+
			"        address: \"%s/outpost.goauthentik.io/auth/traefik\"\n"+
			"        trustForwardHeader: true\n"+
			"        authResponseHeaders:\n"+
			"          - X-authentik-username\n"+
			"          - X-authentik-groups\n"+
			"          - X-authentik-email\n"+
			"          - X-authentik-name\n"+
			"          - X-authentik-uid\n"+
			"          - X-authentik-jwt\n"+
			"\n"+
			"    crowdsec_bouncer:\n"+
			"      forwardAuth:\n"+
			"        address: \"%s/api/v1/forwardAuth\"\n"+
			"        trustForwardHeader: true\n",
			authURL, crowdsecURL)
	return out
}

// generateDynamic: generate a dynamic config from a compose file.
func genDynGenerateDynamic(stackPath, outPath string, cfg map[string]string) bool {
	content, err := os.ReadFile(stackPath)
	if err != nil {
		fmt.Printf("  Parse error %s: %v\n", filepath.Base(stackPath), err)
		return false
	}
	var data map[string]interface{}
	if err := yaml.Unmarshal(content, &data); err != nil {
		fmt.Printf("  Parse error %s: %v\n", filepath.Base(stackPath), err)
		return false
	}

	servicesRaw, _ := data["services"].(map[string]interface{})
	if len(servicesRaw) == 0 {
		return false
	}

	domain := cfg["PRIMARY_DOMAIN"]

	routersOut := ""
	servicesOut := ""
	middlewaresOut := ""
	tcpRoutersOut := ""
	tcpServicesOut := ""

	// Preserve compose file service order. yaml.v3 into map[string]interface{}
	// loses ordering, so re-read service keys in document order.
	for _, svcName := range genDynServiceKeysInOrder(content) {
		svcAny, ok := servicesRaw[svcName]
		if !ok {
			continue
		}
		svc, ok := svcAny.(map[string]interface{})
		if !ok {
			continue
		}
		container := svcName
		if c := genDynStr(svc, "container_name"); c != "" {
			container = c
		}

		if genDynIsTCPService(svcName, svc) && cfg["GEN_TCP"] == "1" {
			port, ok := genDynGetTCPPort(svcName, svc)
			subdomain := strings.ToLower(strings.ReplaceAll(container, "_", "-"))
			if ok {
				tcpRoutersOut += genDynGenTCPRouter(svcName, subdomain, domain, port)
				tcpServicesOut += genDynGenTCPService(svcName, container, port)
			}
			continue
		}

		if !genDynServiceHasTraefik(svc) {
			continue
		}

		port := genDynGetServicePort(svc)
		subdomain := genDynGetSubdomain(svcName, svc)
		sablierMW := ""
		if cfg["GEN_SABLIER"] == "1" && genDynServiceSablierEnabled(svc) {
			sablierMW = "sablier-" + svcName
		}

		if cfg["GEN_ROUTERS"] == "1" {
			routersOut += genDynGenRouter(svcName, subdomain, domain, svcName+"-svc", sablierMW, cfg)
		}
		if cfg["GEN_SERVICES"] == "1" {
			servicesOut += genDynGenService(svcName, container, port)
		}
		if cfg["GEN_SABLIER"] == "1" && genDynServiceSablierEnabled(svc) {
			middlewaresOut += genDynGenSablierMW(svcName, container, cfg)
		}
	}

	if routersOut == "" && servicesOut == "" {
		return false
	}

	out := "http:\n"
	out += "  serversTransports:\n"
	out += "    insecureTransport:\n"
	out += "      insecureSkipVerify: true\n\n"

	if routersOut != "" {
		out += "  routers:\n\n" + routersOut + "\n"
	}
	if servicesOut != "" {
		out += "  services:\n\n" + servicesOut + "\n"
	}
	if middlewaresOut != "" || cfg["GEN_MIDDLEWARES"] == "1" {
		out += "  middlewares:\n"
		if cfg["GEN_MIDDLEWARES"] == "1" {
			out += genDynGenStandardMiddlewares(cfg)
		}
		if middlewaresOut != "" {
			out += middlewaresOut
		}
	}

	if tcpRoutersOut != "" {
		out += "\ntcp:\n  routers:\n\n" + tcpRoutersOut
		out += "\n  services:\n\n" + tcpServicesOut
	}

	if err := os.WriteFile(outPath, []byte(out), 0644); err != nil {
		return false
	}
	return true
}

var genDynServiceKeyRe = regexp.MustCompile(`^  ([a-zA-Z0-9_.-]+):`)

// genDynServiceKeysInOrder extracts service-block names in document order from a
// compose file, so output ordering matches Python's dict iteration order.
func genDynServiceKeysInOrder(content []byte) []string {
	var keys []string
	inServices := false
	for _, line := range strings.Split(string(content), "\n") {
		s := strings.TrimRight(line, "\r")
		if strings.HasPrefix(s, "services:") {
			inServices = true
			continue
		}
		if inServices && len(s) > 0 && s[0] != ' ' && s[0] != '\t' && s[0] != '#' {
			// new top-level key ends the services block
			inServices = false
			continue
		}
		if !inServices {
			continue
		}
		if m := genDynServiceKeyRe.FindStringSubmatch(s); m != nil {
			keys = append(keys, m[1])
		}
	}
	return keys
}

// genDynMain mirrors main(): generate dynamic config(s) for one or all stacks.
// target defaults to "all"; flags is the remaining argv (e.g. "--force").
func genDynMain(args []string) {
	cfg := genDynLoadConf()
	stacksDirP := stacksDir()
	if v := cfg["STACKS_DIR_OVERRIDE"]; v != "" {
		stacksDirP = v
	}
	dynDir := filepath.Join(home(), "MyDocker", "Configs", "Dynamics")
	if v := cfg["DYNAMICS_DIR_OVERRIDE"]; v != "" {
		dynDir = v
	}

	target := "all"
	if len(args) > 0 {
		target = args[0]
	}

	// Stacks that run on a REMOTE host (VPS) — skip any '*-ext' stack during 'all'.
	excludeSuffix := "-ext.yml"

	var files []string
	if target == "all" {
		entries, _ := os.ReadDir(stacksDirP)
		for _, e := range entries {
			n := e.Name()
			if strings.HasSuffix(n, ".yml") && !strings.HasSuffix(n, excludeSuffix) {
				files = append(files, n)
			}
		}
		sort.Strings(files)
	} else {
		if strings.HasSuffix(target, ".yml") {
			files = []string{target}
		} else {
			files = []string{target + ".yml"}
		}
	}

	force := false
	for _, a := range args {
		if a == "--force" {
			force = true
		}
	}

	generated := 0
	for _, fname := range files {
		stackPath := filepath.Join(stacksDirP, fname)
		if _, err := os.Stat(stackPath); err != nil {
			continue
		}
		outName := fname // same name in dynamics dir
		outPath := filepath.Join(dynDir, outName)
		// Don't overwrite existing unless --force
		if _, err := os.Stat(outPath); err == nil && !force {
			fmt.Printf("  skip (exists): %s\n", fname)
			continue
		}
		if genDynGenerateDynamic(stackPath, outPath, cfg) {
			fmt.Printf("  generated: %s\n", fname)
			generated++
		} else {
			fmt.Printf("  skip (no traefik services): %s\n", fname)
		}
	}

	fmt.Printf("\nGenerated %d dynamic config(s)\n", generated)
}
