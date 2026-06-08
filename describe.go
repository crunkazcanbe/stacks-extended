package main

// describe.go — faithful Go port of stacks_describe.py.
//
// stacks_describe — inject service descriptions from conf files.
// Conf dir: ~/.config/stacks/descriptions/
// Usage: describeMain("all") | describeMain(stackname)

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// describeConfDir is the descriptions conf dir (universal: configDir()/descriptions).
func describeConfDir() string {
	return filepath.Join(configDir(), "descriptions")
}

// describeLookup mirrors the Python LOOKUP dict (insertion order preserved via
// describeLookupOrder so the "contains" matching scans in the same sequence).
var describeLookup = map[string]string{
	"ollama":         "Local LLM inference server with AMD ROCm GPU acceleration",
	"comfyui":        "Node-based Stable Diffusion image generation GUI",
	"openwebui":      "Elegant chat frontend for local LLMs and RAG",
	"playwright":     "Headless browser for AI web scraping and automation",
	"searxng":        "Privacy-respecting metasearch engine for AI web search",
	"n8n":            "Visual drag-and-drop workflow automation platform",
	"letta":          "Autonomous agents with persistent long-term memory",
	"litellm":        "Multi-provider AI gateway standardizing LLM APIs",
	"langflow":       "Visual LLM pipeline and agent framework builder",
	"langfuse":       "LLM observability and prompt management platform",
	"traefik":        "Cloud-native reverse proxy and load balancer",
	"sablier":        "Container wake-on-demand autoscaling service",
	"authelia":       "Single sign-on authentication and authorization server",
	"crowdsec":       "Collaborative intrusion detection and prevention system",
	"portainer":      "Web UI for managing Docker containers and stacks",
	"rancher":        "Enterprise Kubernetes and container management platform",
	"gitea":          "Lightweight self-hosted Git repository service",
	"nextcloud":      "Self-hosted cloud storage and collaboration suite",
	"immich":         "High-performance self-hosted photo and video backup",
	"grafana":        "Analytics and monitoring dashboard platform",
	"prometheus":     "Time-series metrics collection and alerting system",
	"loki":           "Log aggregation system designed for Grafana",
	"pihole":         "Network-wide DNS ad blocking server",
	"adguard":        "DNS-based ad and tracker blocking server",
	"technitium":     "Advanced self-hosted DNS server with web UI",
	"wazuh":          "Open source SIEM security monitoring platform",
	"vaultwarden":    "Lightweight self-hosted Bitwarden password manager",
	"jellyfin":       "Free self-hosted media streaming server",
	"postgres":       "Robust open source relational database server",
	"mariadb":        "High-performance MySQL-compatible database server",
	"redis":          "In-memory data structure store and cache",
	"mongodb":        "NoSQL document-oriented database server",
	"neo4j":          "Native graph database for connected data",
	"qdrant":         "High-performance vector similarity search engine",
	"surrealdb":      "Multi-model cloud-native database engine",
	"minio":          "S3-compatible high-performance object storage server",
	"netbird":        "WireGuard-based zero-config mesh VPN platform",
	"tailscale":      "Zero-config WireGuard mesh VPN client",
	"cloudflared":    "Cloudflare Tunnel daemon for secure external access",
	"pangolin":       "Secure tunnel relay for private network access",
	"wazuhindexer":   "Wazuh SIEM data indexing and storage engine",
	"wazuhmanager":   "Wazuh security event collection and analysis hub",
	"wazuhdashboard": "Wazuh SIEM web dashboard and visualization UI",
	"generator":      "Wazuh configuration and certificate generator",
	"pangolinclient": "Secure Pangolin tunnel client for remote access",
	"gerbil":         "Pangolin tunnel relay service component",
	"errorpages":     "Custom styled HTTP error pages for Traefik",
	"authentik":      "Open source identity provider and SSO platform",
	"jellyseerr":     "Media request management for Jellyfin",
	"zoraxy":         "Simple self-hosted reverse proxy manager",
	"openresty":      "Nginx-based web platform with Lua scripting",
	"defectdojo":     "DevSecOps vulnerability management platform",
	"voidauth":       "Lightweight authentication proxy service",
	"dockhand":       "Docker webhook and automation handler",
	"speaches":       "OpenAI-compatible speech-to-text API server",
	"whisper":        "Fast Whisper speech recognition backend",
	"terminalagent":  "Open Interpreter AI code execution agent",
	"opennotebook":   "AI-powered Jupyter-style notebook interface",
	"browserless":    "Headless Chrome browser as a service",
	"gooseagent":     "AI coding agent with tool use capabilities",
	"tabby":          "Self-hosted AI coding assistant server",
	"hermes":         "Custom AI agent hub and workspace platform",
	"zep":            "Long-term memory store for AI assistants",
	"memos":          "Lightweight self-hosted memo and note service",
	"supabase":       "Open source Firebase alternative platform",
	"librechat":      "Enhanced multi-provider AI chat interface",
	"exo":            "Distributed AI inference cluster framework",
	"dockmate":       "Docker container management and monitoring UI",
	"glance":         "Self-hosted dashboard for server overview",
	"coolify":        "Self-hosted Heroku and Netlify alternative PaaS",
	"dokploy":        "Free self-hosted app deployment platform",
	"pterodactyl":    "Open source game server management panel",
	"penpot":         "Open source design and prototyping platform",
	"appsmith":       "Low-code platform for building internal tools",
	"tooljet":        "Open source low-code application builder",
	"syncthing":      "Continuous peer-to-peer file synchronization",
	"invidious":      "Privacy-respecting YouTube frontend",
	"mealie":         "Self-hosted recipe manager and meal planner",
	"tandoor":        "Recipe management platform with meal planning",
	"homeassistant":  "Open source home automation platform",
	"nodered":        "Flow-based visual IoT programming tool",
	"mosquitto":      "Lightweight MQTT message broker",
	"dify":           "Open source LLM app development platform",
	"windmill":       "Open source developer platform for scripts",
	"netdata":        "Real-time infrastructure monitoring and alerting",
	"komodo":         "Container and server management platform",
	"beszel":         "Lightweight server resource monitoring hub",
	"dozzle":         "Real-time Docker container log viewer",
	"ntopng":         "High-speed network traffic analysis tool",
	"headscale":      "Self-hosted Tailscale control server",
	"headplane":      "Web UI management panel for Headscale",
	"clamav":         "Open source antivirus engine and scanner",
	"odoo":           "Open source ERP and business application suite",
	"dolibarr":       "Open source ERP and CRM platform",
	"gamevault":      "Self-hosted game library and launcher",
	"romm":           "Self-hosted retro game ROM manager",
	"webtop":         "Full Linux desktop environment in the browser",
	"scrutiny":       "Hard drive SMART monitoring dashboard",
	"duplicati":      "Encrypted cloud backup solution",
	"borgmatic":      "Automated BorgBackup wrapper utility",
	"provisioner":    "NetBird management server provisioner",
	"trivy":          "Container and filesystem vulnerability scanner",
	"redroid":        "Android container for x86 hosts via KVM",
	"dokku":          "Docker-powered mini-Heroku PaaS platform",
}

// describeLookupOrder preserves the Python dict's insertion order so that the
// "contains" fallback scans keys in the identical sequence.
var describeLookupOrder = []string{
	"ollama", "comfyui", "openwebui", "playwright", "searxng", "n8n", "letta",
	"litellm", "langflow", "langfuse", "traefik", "sablier", "authelia",
	"crowdsec", "portainer", "rancher", "gitea", "nextcloud", "immich",
	"grafana", "prometheus", "loki", "pihole", "adguard", "technitium",
	"wazuh", "vaultwarden", "jellyfin", "postgres", "mariadb", "redis",
	"mongodb", "neo4j", "qdrant", "surrealdb", "minio", "netbird", "tailscale",
	"cloudflared", "pangolin", "wazuhindexer", "wazuhmanager", "wazuhdashboard",
	"generator", "pangolinclient", "gerbil", "errorpages", "authentik",
	"jellyseerr", "zoraxy", "openresty", "defectdojo", "voidauth", "dockhand",
	"speaches", "whisper", "terminalagent", "opennotebook", "browserless",
	"gooseagent", "tabby", "hermes", "zep", "memos", "supabase", "librechat",
	"exo", "dockmate", "glance", "coolify", "dokploy", "pterodactyl", "penpot",
	"appsmith", "tooljet", "syncthing", "invidious", "mealie", "tandoor",
	"homeassistant", "nodered", "mosquitto", "dify", "windmill", "netdata",
	"komodo", "beszel", "dozzle", "ntopng", "headscale", "headplane", "clamav",
	"odoo", "dolibarr", "gamevault", "romm", "webtop", "scrutiny", "duplicati",
	"borgmatic", "provisioner", "trivy", "redroid", "dokku",
}

// describeStripChars removes the chars the Python strips for name matching.
func describeStripChars(s string, chars ...string) string {
	for _, c := range chars {
		s = strings.ReplaceAll(s, c, "")
	}
	return s
}

// getFallback is a faithful port of get_fallback(name, image="").
func describeGetFallback(name, image string) string {
	n := describeStripChars(strings.ToLower(name), "-", "_", ".")
	// image.lower().split("/")[-1].split(":")[0].replace("-","").replace("_","")
	imgLower := strings.ToLower(image)
	slashParts := strings.Split(imgLower, "/")
	img := slashParts[len(slashParts)-1]
	img = strings.Split(img, ":")[0]
	img = describeStripChars(img, "-", "_")

	for _, key := range describeLookupOrder {
		k := describeStripChars(key, "-", "_")
		if k == n || k == img {
			return describeLookup[key]
		}
	}
	for _, key := range describeLookupOrder {
		k := describeStripChars(key, "-", "_")
		if strings.Contains(n, k) || strings.Contains(img, k) ||
			strings.Contains(k, n) || strings.Contains(k, img) {
			return describeLookup[key]
		}
	}
	return fmt.Sprintf("Self-hosted %s service container", name)
}

// describeLoadConf loads description conf file, returns map of {service: [lines]}
// plus the keys in Python dict insertion order (so the "find by service in any
// format" fallback in inject scans keys in the identical sequence as Python's
// `for k in conf_descs`). Faithful port of load_conf(stack_name).
func describeLoadConf(stackName string) (map[string][]string, []string) {
	confPath := filepath.Join(describeConfDir(), stackName+".conf")
	data, err := os.ReadFile(confPath)
	if err != nil {
		return map[string][]string{}, nil
	}
	descs := map[string][]string{}
	var order []string
	// setDesc mimics Python dict assignment: a new key is appended to the order;
	// reassigning an existing key keeps its original position.
	setDesc := func(k string, v []string) {
		if _, exists := descs[k]; !exists {
			order = append(order, k)
		}
		descs[k] = v
	}
	currentSvc := ""
	hasSvc := false
	var currentLines []string

	for _, line := range describeReadLines(string(data)) {
		s := strings.TrimRight(line, " \t\r\n\v\f")
		// Skip blank lines between services
		if s == "" {
			if hasSvc && len(currentLines) > 0 {
				setDesc(currentSvc, currentLines)
				currentSvc = ""
				hasSvc = false
				currentLines = nil
			}
			continue
		}
		// Comment lines — collect as description content
		if strings.HasPrefix(s, "#") {
			if hasSvc {
				currentLines = append(currentLines, s)
			}
			continue
		}
		// Non-comment, non-blank — this is a service name
		if hasSvc && len(currentLines) > 0 {
			setDesc(currentSvc, currentLines)
		}
		currentSvc = strings.TrimSpace(s)
		hasSvc = true
		currentLines = nil
	}
	if hasSvc && len(currentLines) > 0 {
		setDesc(currentSvc, currentLines)
	}
	return descs, order
}

// describeService mirrors the Python service dict {'name','image','container_name'}.
type describeService struct {
	name          string
	image         string
	containerName string
}

var describeReSvcLine = regexp.MustCompile(`^  ([a-zA-Z0-9_.\-]+):\s*$`)
var describeReServices = regexp.MustCompile(`^services:`)
var describeReTopKey = regexp.MustCompile(`^[a-zA-Z]`)
var describeReImage = regexp.MustCompile(`^\s+image:\s+(.+)`)
var describeReAnchor = regexp.MustCompile(`^\s+(<<:|image:|container_name:)`)
var describeReDashes = regexp.MustCompile(`^\s+#\s*-{3,}`)

// describeParseServices is a faithful port of parse_services(path).
func describeParseServices(path string) []describeService {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var services []describeService
	inServices := false
	hasCurrent := false
	var current describeService

	for _, line := range describeReadLines(string(data)) {
		s := strings.TrimRight(line, " \t\r\n\v\f")
		if describeReServices.MatchString(s) {
			inServices = true
			continue
		}
		if describeReTopKey.MatchString(s) && !strings.HasPrefix(s, " ") && inServices {
			if hasCurrent {
				services = append(services, current)
			}
			inServices = false
			continue
		}
		if !inServices {
			continue
		}
		if m := describeReSvcLine.FindStringSubmatch(s); m != nil {
			if hasCurrent {
				services = append(services, current)
			}
			current = describeService{name: m[1], image: "", containerName: m[1]}
			hasCurrent = true
			continue
		}
		if hasCurrent {
			if im := describeReImage.FindStringSubmatch(s); im != nil {
				current.image = strings.TrimSpace(im[1])
			}
		}
	}
	if hasCurrent {
		services = append(services, current)
	}
	return services
}

// describeReadLines splits content like Python's iteration over file lines /
// readlines(), preserving lines. Python's open() iteration yields each line
// including its trailing newline; here we only need the text content, and since
// callers strip the right side, splitting on "\n" is sufficient for parsing.
// For the readlines() use in inject (which preserves the line text), we keep the
// original newline characters via describeReadLinesKeep.
func describeReadLines(content string) []string {
	if content == "" {
		return nil
	}
	// Split keeping behavior equivalent to iterating file lines.
	lines := strings.Split(content, "\n")
	// If the content ends with a newline, the final empty element is spurious
	// for line-iteration semantics (Python would not yield a trailing empty
	// "line" after a final newline).
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// describeReadLinesKeep returns the file's lines WITH their trailing newline
// characters preserved, faithful to Python's readlines().
func describeReadLinesKeep(content string) []string {
	if content == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			out = append(out, content[start:i+1])
			start = i + 1
		}
	}
	if start < len(content) {
		out = append(out, content[start:])
	}
	return out
}

// describeInjectDescriptions is a faithful port of inject_descriptions(path).
func describeInjectDescriptions(path string) {
	stackName := strings.ReplaceAll(filepath.Base(path), ".yml", "")
	services := describeParseServices(path)
	if len(services) == 0 {
		fmt.Printf("  No services in %s\n", stackName)
		return
	}

	confDescs, confOrder := describeLoadConf(stackName)

	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("  No services in %s\n", stackName)
		return
	}
	lines := describeReadLinesKeep(string(data))
	var out []string
	svcNum := 0

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		s := strings.TrimRight(line, " \t\r\n\v\f")
		if m := describeReSvcLine.FindStringSubmatch(s); m != nil {
			svcName := m[1]
			// Check it's a real service
			isService := false
			end := i + 6
			if end > len(lines) {
				end = len(lines)
			}
			for j := i + 1; j < end; j++ {
				if describeReAnchor.MatchString(lines[j]) {
					isService = true
					break
				}
			}

			if isService {
				// Remove any existing desc block from end of out.
				// Python: while out and out[-1].strip().startswith('#  #') or
				//         (out and re.match(r'\s+#\s*-{3,}', out[-1])):
				// Operator precedence: (A and B) or (C and D).
				for {
					condA := len(out) > 0 && strings.HasPrefix(strings.TrimSpace(out[len(out)-1]), "#  #")
					condCD := len(out) > 0 && describeReDashes.MatchString(out[len(out)-1])
					if condA || condCD {
						out = out[:len(out)-1]
						continue
					}
					break
				}
				// Also remove the last block of # lines
				for len(out) > 0 && strings.HasPrefix(strings.TrimSpace(out[len(out)-1]), "#") {
					last := strings.TrimSpace(out[len(out)-1])
					if strings.Contains(last, "---") || strings.Contains(last, "Description:") ||
						strings.Contains(last, "🐳") || strings.Contains(last, "✅") {
						out = out[:len(out)-1]
					} else {
						break
					}
				}

				svcNum++
				// Get description
				var descLines []string
				if dl, ok := confDescs[svcName]; ok {
					descLines = dl
				} else {
					// Find by service in any format
					found := ""
					foundOk := false
					for _, k := range confOrder {
						if describeStripChars(strings.ToLower(k), "-", "_") ==
							describeStripChars(strings.ToLower(svcName), "-", "_") {
							found = k
							foundOk = true
							break
						}
					}
					if foundOk {
						descLines = confDescs[found]
					} else {
						// Fallback to lookup
						img := ""
						for _, sv := range services {
							if sv.name == svcName {
								img = sv.image
								break
							}
						}
						descLines = []string{"# " + describeGetFallback(svcName, img)}
					}
				}

				// Build description block
				display := strings.ReplaceAll(strings.ReplaceAll(strings.ToUpper(svcName), "-", " "), "_", " ")
				block := "  # ---------------------------------------------------------\n"
				block += fmt.Sprintf("  # %02d. %s 🐳\n", svcNum, display)
				for _, dl := range descLines {
					dl = strings.TrimSpace(dl)
					if !strings.HasPrefix(dl, "#") {
						dl = "# " + dl
					}
					block += "  " + dl + "\n"
				}
				block += "  # ---------------------------------------------------------\n"
				out = append(out, block)
			}
		}

		out = append(out, line)
	}

	if err := os.WriteFile(path, []byte(strings.Join(out, "")), 0644); err != nil {
		fmt.Printf("  No services in %s\n", stackName)
		return
	}
	fmt.Printf("  ✔ %s — %d services described\n", stackName, svcNum)
}

// describeMain is a faithful port of the __main__ block.
func describeMain(args []string) {
	target := "all"
	if len(args) > 0 {
		target = args[0]
	}

	var files []string
	if target == "all" || target == "--all" {
		entries, err := os.ReadDir(stacksDir())
		if err == nil {
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".yml") {
					files = append(files, filepath.Join(stacksDir(), e.Name()))
				}
			}
			sort.Strings(files)
		}
	} else if describeIsFile(target) {
		files = []string{target}
	} else if describeIsFile(filepath.Join(stacksDir(), target+".yml")) {
		files = []string{filepath.Join(stacksDir(), target+".yml")}
	} else {
		files = []string{filepath.Join(stacksDir(), target)}
	}

	fmt.Printf("\n\033[1;35m📝 Injecting service descriptions...\033[0m\n")
	for _, f := range files {
		if describeIsFile(f) {
			describeInjectDescriptions(f)
		}
	}
	fmt.Printf("\n\033[1;32m✔ Done\033[0m\n")
}

// describeIsFile mirrors os.path.isfile (true only for regular files).
func describeIsFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}
