package lib

// Edge — the VPS edge watchdog.
//
// Named for the hundred-eyed all-seeing watchman of myth. It runs ON the VPS
// (51.81.85.20) and probes every public site from the *real public edge*
// (DNS → VPS Traefik → Pangolin wildcard → home tunnel → home Traefik). The
// home watchdog can see a site is broken but cannot fix an EDGE problem; Edge
// is the half that can. It both detects AND repairs VPS-side breakage.
//
// Failure classification:
//   • MANY sites down at once  → an EDGE incident (Pangolin / tunnel / Traefik /
//     Docker daemon). Edge runs its repair toolbox below.
//   • ONE site down            → almost always a home-app problem, which the
//     HOME watchdog owns. Edge logs it (and can optionally poke the home
//     watchdog over SSH) but does not thrash the edge over a single app.
//
// Repair toolbox (tiered, only-as-needed, with an anti-loop grace window):
//   1. Pangolin wildcard sso flipped to 1  → UPDATE sso=0 + restart pangolin
//      (THE recurring incident — a migration flips it and gates every domain,
//      which 404s non-browser apps like the Vaultwarden phone app).
//   2. Pangolin wildcard enabled=0         → re-enable it.
//   3. Pangolin container down/unhealthy   → restart, then recreate if needed.
//   4. Gerbil (WireGuard tunnel server)    → restart, then recreate.
//   5. Traefik (edge proxy)                → restart, then recreate.
//   6. Docker daemon itself unreachable    → systemctl restart docker.
//   7. After any repair, re-probe; still down → escalate restart → recreate.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ── config ───────────────────────────────────────────────────────────────────

type edgeConfig struct {
	sites      []string
	interval   int
	grace      int
	okCodes    string
	timeout    string
	edgeThresh int // >= this many sites down at once ⇒ treat as an edge incident

	fixSSO          bool
	restartPangolin bool
	restartGerbil   bool
	restartTraefik  bool
	recreateOnFail  bool

	pangolinName string
	gerbilName   string
	traefikName  string
	pangolinDB   string
	composeFile  string // /opt/pangolin/docker-compose.yml (for --force-recreate)

	triggerHome string // optional shell cmd to poke the home watchdog (SSH) on single-site fail
}

func loadEdgeConfig() edgeConfig {
	cfg := configLoad()
	// Back-compat: this feature was briefly called "Argus". Honor any legacy
	// ARGUS_* keys by mapping them onto EDGE_* when the new key isn't set, so
	// old configs keep working with no edits.
	for k, v := range cfg {
		if strings.HasPrefix(k, "ARGUS_") {
			nk := "EDGE_" + strings.TrimPrefix(k, "ARGUS_")
			if cfg[nk] == "" {
				cfg[nk] = v
			}
		}
	}
	c := edgeConfig{
		interval:        cfgInt(cfg, "EDGE_INTERVAL", 60),
		grace:           cfgInt(cfg, "EDGE_GRACE", 120),
		okCodes:         cfgStrKey(cfg, "EDGE_OK_CODES", "200,204,301,302,307,308,401,403,405"),
		timeout:         cfgStrKey(cfg, "EDGE_TIMEOUT", "12"),
		edgeThresh:      cfgInt(cfg, "EDGE_THRESHOLD", 2),
		fixSSO:          cfgBoolKey(cfg, "EDGE_FIX_SSO", true),
		restartPangolin: cfgBoolKey(cfg, "EDGE_RESTART_PANGOLIN", true),
		restartGerbil:   cfgBoolKey(cfg, "EDGE_RESTART_GERBIL", true),
		restartTraefik:  cfgBoolKey(cfg, "EDGE_RESTART_TRAEFIK", true),
		recreateOnFail:  cfgBoolKey(cfg, "EDGE_RECREATE_ON_FAIL", true),
		pangolinName:    cfgStrKey(cfg, "EDGE_PANGOLIN_CONTAINER", "pangolin"),
		gerbilName:      cfgStrKey(cfg, "EDGE_GERBIL_CONTAINER", "gerbil"),
		traefikName:     cfgStrKey(cfg, "EDGE_TRAEFIK_CONTAINER", "traefik"),
		pangolinDB:      cfgStrKey(cfg, "EDGE_PANGOLIN_DB", "/opt/pangolin/config/db/db.sqlite"),
		composeFile:     cfgStrKey(cfg, "EDGE_COMPOSE_FILE", "/opt/pangolin/docker-compose.yml"),
		triggerHome:     cfgStrKey(cfg, "EDGE_TRIGGER_HOME", ""),
	}
	c.sites = edgeSites(cfg)
	return c
}

// edgeSites reads the watch list. Priority: the edge.sites file (one host per
// line) → the EDGE_SITES conf key → a sensible default seed of her always-on
// public sites. Hosts may be bare ("vault.loveiznothin.com").
func edgeSites(cfg map[string]string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(h string) {
		h = strings.TrimSpace(h)
		h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
		h = strings.TrimSuffix(h, "/")
		if h == "" || strings.HasPrefix(h, "#") || seen[h] {
			return
		}
		seen[h] = true
		out = append(out, h)
	}
	// file first (edge.sites, or the legacy argus.sites as a fallback)
	sitesFile := filepath.Join(configDir(), "edge.sites")
	if !fileExists(sitesFile) {
		if legacy := filepath.Join(configDir(), "argus.sites"); fileExists(legacy) {
			sitesFile = legacy
		}
	}
	if b, err := os.ReadFile(sitesFile); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			add(ln)
		}
	}
	// conf key (space/comma separated)
	for _, h := range strings.FieldsFunc(cfg["EDGE_SITES"], func(r rune) bool {
		return r == ' ' || r == ',' || r == '\n' || r == '\t'
	}) {
		add(h)
	}
	if len(out) == 0 {
		for _, h := range edgeDefaultSites {
			add(h)
		}
	}
	return out
}

// Seed list — her critical always-on public sites. She edits edge.sites to add
// more; these are just so Edge is useful the moment it's installed.
var edgeDefaultSites = []string{
	"vault.loveiznothin.com",
	"links.loveiznothin.com",
	"search.loveiznothin.com",
	"homepage.loveiznothin.com",
	"glances.loveiznothin.com",
	"pangolin.loveiznothin.dev",
}

// ── dispatch ─────────────────────────────────────────────────────────────────

func cmdEdge(args []string) {
	action := ""
	if len(args) > 0 {
		action = strings.TrimPrefix(strings.TrimPrefix(args[0], "--"), "-")
	}
	switch action {
	case "install", "setup":
		edgeInstall()
	case "uninstall", "remove":
		edgeUninstall()
	case "status":
		edgeStatus()
	case "check", "once":
		edgeRun(true)
	case "repair", "heal":
		// force a full edge-repair pass regardless of probe results
		cfg := loadEdgeConfig()
		fmt.Println("\x1b[1;36m🔧 Edge forced edge repair\x1b[0m")
		edgeEdgeRepair(cfg, cfg.sites)
	case "", "run", "watch":
		edgeRun(false)
	default:
		fmt.Println("usage: stacks edge [run|once|status|repair|install|uninstall]")
	}
}

// ── the loop ─────────────────────────────────────────────────────────────────

func edgeRun(once bool) {
	c := loadEdgeConfig()
	fmt.Printf("\x1b[1;35m👁  Edge watchdog\x1b[0m  every %ds, grace=%ds, %d sites\n",
		c.interval, c.grace, len(c.sites))
	fmt.Printf("   watching: %s\n", strings.Join(c.sites, " "))
	var lastEdgeHeal time.Time
	first := true
	for {
		c = loadEdgeConfig() // reload so edits to edge.sites/conf take effect live
		down := edgeSweep(c)
		switch {
		case len(down) == 0:
			if first || once {
				fmt.Printf("\x1b[32m✔ all %d sites serving from the edge\x1b[0m\n", len(c.sites))
			}
		case len(down) >= c.edgeThresh:
			fmt.Printf("\x1b[31m✘ EDGE INCIDENT — %d/%d sites down: %s\x1b[0m\n",
				len(down), len(c.sites), strings.Join(down, " "))
			grace := time.Duration(c.grace) * time.Second
			if !lastEdgeHeal.IsZero() && time.Since(lastEdgeHeal) < grace {
				fmt.Printf("   …in grace window (%s left) — letting the last repair settle\n",
					(grace - time.Since(lastEdgeHeal)).Round(time.Second))
			} else {
				edgeEdgeRepair(c, down)
				lastEdgeHeal = time.Now()
			}
		default: // single site (or below threshold) — home-app problem, not the edge
			fmt.Printf("\x1b[33m⚠ %s down — looks like a home-app issue (home watchdog's job), not the edge\x1b[0m\n",
				strings.Join(down, " "))
			if c.triggerHome != "" {
				fmt.Println("   poking the home watchdog:", c.triggerHome)
				runShell(c.triggerHome)
			}
		}
		if once {
			return
		}
		first = false
		time.Sleep(time.Duration(c.interval) * time.Second)
	}
}

func edgeSweep(c edgeConfig) []string {
	cfg := map[string]string{"WATCH_SITE_OK_CODES": c.okCodes, "WATCH_SITE_TIMEOUT": c.timeout}
	var down []string
	for _, h := range c.sites {
		if !siteOK(h, cfg) {
			down = append(down, h)
		}
	}
	return down
}

// ── the repair toolbox ───────────────────────────────────────────────────────

// edgeEdgeRepair runs the tiered VPS-side repairs, re-probing between tiers and
// stopping as soon as the edge is healthy again.
func edgeEdgeRepair(c edgeConfig, down []string) {
	probe := func() bool { return len(edgeSweep(c)) == 0 }

	// 1. Pangolin DB: wildcard sso flipped to 1, or wildcard disabled.
	if c.fixSSO {
		if n := edgeSQLScalar(c, "SELECT count(*) FROM resources WHERE wildcard=1 AND sso=1;"); n > 0 {
			fmt.Printf("   \x1b[33m↳ %d wildcard(s) auth-gated (sso=1) — resetting to pass-through (sso=0)\x1b[0m\n", n)
			edgeSQLExec(c, "UPDATE resources SET sso=0 WHERE wildcard=1;")
			edgeRestart(c.pangolinName)
			edgeSettle(c, 8)
			if probe() {
				fmt.Println("   \x1b[32m✔ edge restored (sso reset)\x1b[0m")
				return
			}
		}
		if n := edgeSQLScalar(c, "SELECT count(*) FROM resources WHERE wildcard=1 AND enabled=0;"); n > 0 {
			fmt.Printf("   \x1b[33m↳ %d wildcard(s) disabled — re-enabling\x1b[0m\n", n)
			edgeSQLExec(c, "UPDATE resources SET enabled=1 WHERE wildcard=1;")
			edgeRestart(c.pangolinName)
			edgeSettle(c, 8)
			if probe() {
				fmt.Println("   \x1b[32m✔ edge restored (wildcards re-enabled)\x1b[0m")
				return
			}
		}
	}

	// 2. Docker daemon itself — if it's unreachable nothing else can be fixed.
	if !edgeDockerUp() {
		fmt.Println("   \x1b[33m↳ Docker daemon unreachable — restarting docker.service\x1b[0m")
		runShell("systemctl restart docker")
		edgeSettle(c, 10)
	}

	// 3-5. Edge containers: pangolin → gerbil → traefik. Restart any that aren't
	// up/healthy, re-probing after each so we stop at the first thing that fixes it.
	type svc struct {
		name string
		on   bool
	}
	for _, s := range []svc{
		{c.pangolinName, c.restartPangolin},
		{c.gerbilName, c.restartGerbil},
		{c.traefikName, c.restartTraefik},
	} {
		if !s.on {
			continue
		}
		if edgeHealthy(s.name) {
			continue // this one's fine, leave it
		}
		fmt.Printf("   \x1b[33m↳ %s not healthy — restarting\x1b[0m\n", s.name)
		edgeRestart(s.name)
		edgeSettle(c, 8)
		if probe() {
			fmt.Printf("   \x1b[32m✔ edge restored (%s restart)\x1b[0m\n", s.name)
			return
		}
	}

	// 6. Escalation: a plain restart didn't do it — force-recreate the edge stack
	// from its compose file (picks up image/config drift a restart can't).
	if c.recreateOnFail && fileExists(c.composeFile) {
		fmt.Println("   \x1b[33m↳ restarts didn't restore the edge — force-recreating the Pangolin stack\x1b[0m")
		runShell(fmt.Sprintf("docker compose -f %s up -d --force-recreate", shq(c.composeFile)))
		edgeSettle(c, 12)
		if probe() {
			fmt.Println("   \x1b[32m✔ edge restored (stack recreate)\x1b[0m")
			return
		}
	}

	// Final verdict.
	if probe() {
		fmt.Println("   \x1b[32m✔ edge healthy again\x1b[0m")
	} else {
		still := edgeSweep(c)
		fmt.Printf("   \x1b[31m✘ edge still degraded after all repairs: %s\x1b[0m\n", strings.Join(still, " "))
		fmt.Println("     (exhausted the toolbox — may be DNS, the home tunnel down, or home Traefik. Check home.)")
	}
}

// edgeSettle re-probes a couple of times over a few seconds so a just-restarted
// container has a moment to bind before the next tier judges it.
func edgeSettle(c edgeConfig, secs int) {
	time.Sleep(time.Duration(secs) * time.Second)
}

// ── low-level helpers (docker + sqlite + shell), root service so no sudo) ─────

func edgeDockerUp() bool {
	return exec.Command("docker", "info", "--format", "{{.ServerVersion}}").Run() == nil
}

// edgeHealthy: container is running and, if it has a healthcheck, not unhealthy.
func edgeHealthy(name string) bool {
	out, err := exec.Command("docker", "inspect", "-f",
		"{{.State.Running}}|{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}", name).Output()
	if err != nil {
		return false
	}
	f := strings.SplitN(strings.TrimSpace(string(out)), "|", 2)
	if len(f) != 2 || f[0] != "true" {
		return false
	}
	return f[1] != "unhealthy" && f[1] != "starting"
}

func edgeRestart(name string) { _ = exec.Command("docker", "restart", name).Run() }

func edgeSQLScalar(c edgeConfig, q string) int {
	out, err := exec.Command("sqlite3", c.pangolinDB, q).Output()
	if err != nil {
		return 0
	}
	n := 0
	fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n)
	return n
}

func edgeSQLExec(c edgeConfig, q string) {
	// back up first — never edit the Pangolin DB without a copy
	runShell(fmt.Sprintf("cp -f %s %s.edge-bak 2>/dev/null", shq(c.pangolinDB), shq(c.pangolinDB)))
	_ = exec.Command("sqlite3", c.pangolinDB, q).Run()
}

func runShell(cmd string) { _ = exec.Command("sh", "-c", cmd).Run() }

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ── status ───────────────────────────────────────────────────────────────────

func edgeStatus() {
	c := loadEdgeConfig()
	fmt.Println("\x1b[1;35m👁  Edge watchdog status\x1b[0m")
	fmt.Printf("   interval=%ds grace=%ds edge_threshold=%d\n", c.interval, c.grace, c.edgeThresh)
	fmt.Printf("   repairs: sso=%v pangolin=%v gerbil=%v traefik=%v recreate=%v\n",
		c.fixSSO, c.restartPangolin, c.restartGerbil, c.restartTraefik, c.recreateOnFail)
	fmt.Printf("   pangolin db=%s\n", c.pangolinDB)
	out, _ := exec.Command("systemctl", "is-enabled", edgeUnit).Output()
	st := strings.TrimSpace(string(out))
	if st == "" {
		st = "not-installed"
	}
	fmt.Printf("   %s: %s\n", edgeUnit, st)
	if c.fixSSO {
		bad := edgeSQLScalar(c, "SELECT count(*) FROM resources WHERE wildcard=1 AND sso=1;")
		mark := "\x1b[32m✔ all wildcards pass-through (sso=0)\x1b[0m"
		if bad > 0 {
			mark = fmt.Sprintf("\x1b[31m✘ %d wildcard(s) auth-gated (sso=1) — run: stacks edge repair\x1b[0m", bad)
		}
		fmt.Println("   pangolin:", mark)
	}
	fmt.Println("   live edge probe:")
	for _, h := range c.sites {
		mark := "\x1b[31m✘\x1b[0m"
		if siteOK(h, map[string]string{"WATCH_SITE_OK_CODES": c.okCodes, "WATCH_SITE_TIMEOUT": c.timeout}) {
			mark = "\x1b[32m✔\x1b[0m"
		}
		fmt.Printf("     %s %s\n", mark, h)
	}
}

// ── install / uninstall (systemd, runs as root so docker+sqlite need no sudo) ─

const edgeUnit = "stacks-edge.service"

func edgeInstall() {
	self := selfBin()
	confDir := configDir()
	// seed edge.sites if absent so she has something to edit
	sitesPath := filepath.Join(confDir, "edge.sites")
	if !fileExists(sitesPath) {
		_ = os.MkdirAll(confDir, 0755)
		body := "# Edge watch list — one public host per line. Lines starting with # are ignored.\n" +
			strings.Join(edgeDefaultSites, "\n") + "\n"
		_ = os.WriteFile(sitesPath, []byte(body), 0644)
		fmt.Println("   seeded", sitesPath)
	}
	unit := fmt.Sprintf(`[Unit]
Description=Stacks edge watchdog (probe public sites + repair the edge)
After=docker.service network-online.target
Wants=docker.service network-online.target

[Service]
Type=simple
Environment=STACKS_CONFIG_DIR=%s
ExecStart=%s edge run
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
`, confDir, self)
	if err := writeUnit(edgeUnit, unit); err != nil {
		fmt.Println("✘ write edge unit:", err)
		fmt.Println("  (need root — try: sudo", self, "edge install)")
		return
	}
	run := func(a ...string) { c := exec.Command("systemctl", a...); c.Stdout, c.Stderr = os.Stdout, os.Stderr; _ = c.Run() }
	run("daemon-reload")
	run("enable", "--now", edgeUnit)
	fmt.Println("\x1b[1;32m✔ installed + started\x1b[0m", edgeUnit)
	fmt.Println("  edit the watch list:", sitesPath)
	fmt.Println("  follow it:           journalctl -u", edgeUnit, "-f")
}

func edgeUninstall() {
	run := func(a ...string) { c := exec.Command("systemctl", a...); c.Stdout, c.Stderr = os.Stdout, os.Stderr; _ = c.Run() }
	run("disable", "--now", edgeUnit)
	p := filepath.Join("/etc/systemd/system", edgeUnit)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		fmt.Println("✘ remove", p, err, "(need root?)")
	}
	run("daemon-reload")
	fmt.Println("\x1b[1;32m✔ removed\x1b[0m", edgeUnit)
}
