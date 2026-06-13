package lib

// Controlled boot bring-up + 24/7 watchdog, built into the stacks program.
//
//   stacks up --boot      one controlled, parallel, strategy-driven bring-up
//   stacks watch          the 24/7 watchdog loop (keeps watched stacks alive)
//   stacks boot --install install + enable the early-boot & watchdog services
//   stacks boot --uninstall / --status
//
// All knobs live in the normal stacks config (stacks.yaml / Settings tab):
//   boot_delay, up_parallel, boot_stacks, start_strategy, boot_escalate,
//   boot_escalation, boot_force, boot_download_missing, boot_override_docker,
//   watch_enabled, watch_stacks, watch_interval, watch_strategy,
//   watch_escalate, watch_escalation, watch_force.
//
// The engine reuses the program's OWN tested up/repair/recreate/fix by
// re-invoking the stacks binary per stack (process isolation + parallelism),
// exactly like the old stackd daemon did — no logic is duplicated.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── config ──────────────────────────────────────────────────────────────────

type bootConfig struct {
	delay           int
	parallel        int
	stacks          []string
	strategy        string
	escalate        bool
	escalation      []string
	force           bool
	downloadMissing bool
	overrideDocker  bool
	// watchdog
	watchEnabled    bool
	watchStacks     []string
	watchInterval   int
	watchStrategy   string
	watchEscalate   bool
	watchEscalation []string
	watchForce      bool
}

func cfgInt(cfg map[string]string, key string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(cfg[key])); err == nil {
		return v
	}
	return def
}

func cfgBoolKey(cfg map[string]string, key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(cfg[key]))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func cfgStrKey(cfg map[string]string, key, def string) string {
	if v := strings.TrimSpace(cfg[key]); v != "" {
		return v
	}
	return def
}

func loadBootConfig() bootConfig {
	cfg := configLoad()
	bc := bootConfig{
		delay:           cfgInt(cfg, "BOOT_DELAY", 0),
		parallel:        cfgInt(cfg, "UP_PARALLEL", 3),
		stacks:          strings.Fields(cfg["BOOT_STACKS"]),
		strategy:        strings.ToLower(cfgStrKey(cfg, "START_STRATEGY", "repair")),
		escalate:        cfgBoolKey(cfg, "BOOT_ESCALATE", true),
		escalation:      strings.Fields(cfgStrKey(cfg, "BOOT_ESCALATION", "recreate fix")),
		force:           cfgBoolKey(cfg, "BOOT_FORCE", false),
		downloadMissing: cfgBoolKey(cfg, "BOOT_DOWNLOAD_MISSING", false),
		overrideDocker:  cfgBoolKey(cfg, "BOOT_OVERRIDE_DOCKER", false),
		watchEnabled:    cfgBoolKey(cfg, "WATCH_ENABLED", true),
		watchStacks:     strings.Fields(cfg["WATCH_STACKS"]),
		watchInterval:   cfgInt(cfg, "WATCH_INTERVAL", 30),
		watchStrategy:   strings.ToLower(cfgStrKey(cfg, "WATCH_STRATEGY", "repair")),
		watchEscalate:   cfgBoolKey(cfg, "WATCH_ESCALATE", true),
		watchEscalation: strings.Fields(cfgStrKey(cfg, "WATCH_ESCALATION", "recreate fix")),
		watchForce:      cfgBoolKey(cfg, "WATCH_FORCE", false),
	}
	if bc.parallel < 1 {
		bc.parallel = 1
	}
	if bc.watchInterval < 5 {
		bc.watchInterval = 5
	}
	// blank boot list => every stack; blank watch list => the boot list.
	if len(bc.stacks) == 0 {
		bc.stacks = dispStackList()
	}
	if len(bc.watchStacks) == 0 {
		bc.watchStacks = bc.stacks
	}
	return bc
}

// ── helpers ─────────────────────────────────────────────────────────────────

// selfBin returns the stacks binary to re-invoke for per-stack work.
func selfBin() string {
	if p, err := os.Executable(); err == nil && p != "" {
		return p
	}
	if _, err := os.Stat("/usr/local/bin/stacks"); err == nil {
		return "/usr/local/bin/stacks"
	}
	return os.Args[0]
}

// strategyArgs maps a strategy word to the `stacks up <stack> …` modifier.
func strategyArgs(stack, strategy string) []string {
	switch strings.ToLower(strategy) {
	case "repair":
		return []string{"up", stack, "repair"}
	case "fix":
		return []string{"up", stack, "fix"}
	case "recreate":
		return []string{"up", stack, "recreate"}
	default: // "up" / "start" / anything else => plain up
		return []string{"up", stack}
	}
}

// restartStack is the CHEAPEST heal: docker restart every container in the stack
// (running OR exited). It clears a wedged app that Docker still calls "running"
// but that has stopped serving its web page — the common case — without the cost
// of a repair/recreate/fix. It also starts a container that has exited.
func restartStack(stack string) {
	for name := range stackContainerStates(stack, true) {
		_ = exec.Command("docker", "restart", "-t", "10", name).Run()
	}
}

// runStrategy applies one strategy to one stack. "restart" is handled inline
// (a plain docker restart); every other strategy re-invokes the stacks binary so
// we reuse the program's own tested up/repair/recreate/fix.
func runStrategy(stack, strategy string) {
	if strings.ToLower(strategy) == "restart" {
		restartStack(stack)
		return
	}
	args := strategyArgs(stack, strategy)
	cmd := exec.Command(selfBin(), args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	_ = cmd.Run()
}

// stackContainerStates returns name->state for one compose project (all=incl. stopped).
func stackContainerStates(stack string, all bool) map[string]string {
	psArgs := []string{"ps", "--format", "{{.Names}}\t{{.State}}",
		"--filter", "label=com.docker.compose.project=" + stack}
	if all {
		psArgs = append([]string{"ps", "-a"}, psArgs[1:]...)
	}
	out, err := exec.Command("docker", psArgs...).Output()
	res := map[string]string{}
	if err != nil {
		return res
	}
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if ln == "" {
			continue
		}
		name, state, _ := strings.Cut(ln, "\t")
		res[strings.TrimSpace(name)] = strings.TrimSpace(state)
	}
	return res
}

// stackHealthy is deliberately Zero-Scale/Sablier-aware: a stack is only
// "unhealthy" (worth escalating/healing) if it has NO containers at all (never
// brought up) or something is actively THRASHING (restarting/dead). Containers
// that are cleanly "exited" are treated as asleep-on-purpose (wake-on-visit) and
// left alone — the watchdog must never fight Zero Scale by waking sleepers.
func stackHealthy(stack string) bool {
	all := stackContainerStates(stack, true)
	if len(all) == 0 {
		return false
	}
	for _, st := range all {
		if st == "restarting" || st == "dead" {
			return false
		}
	}
	return true
}

// healthOK is the escalation gate. A stack is OK only if its containers aren't
// thrashing AND — when site-checking is on — its web page actually serves. This
// is the whole point: a container can be "running" while its site is wedged
// (errors/502/won't connect). Escalation must keep climbing until the PAGE is
// back, not stop the moment Docker says "running". A short settle gives the app
// time to bind its port after a restart/recreate before we probe the site.
func healthOK(stack string, siteCheck bool, cfg map[string]string) bool {
	if !stackHealthy(stack) {
		return false
	}
	if !siteCheck {
		return true
	}
	time.Sleep(4 * time.Second) // let the app bind before probing the page
	return stackSiteOK(stack, cfg)
}

// applyTo runs the gentle strategy then (force OR not-yet-OK) escalates in order.
// When siteCheck is on, "OK" means the web page serves, so escalation continues
// restart→repair→recreate→fix until the site is actually back (or steps run out).
func applyTo(stack, strategy string, escalate, force bool, escalation []string, siteCheck bool, cfg map[string]string) {
	fmt.Printf("\x1b[1;36m▸ %s\x1b[0m  (%s)\n", stack, strategy)
	runStrategy(stack, strategy)
	if force {
		for _, step := range escalation {
			fmt.Printf("  \x1b[33m↑ forced %s → %s\x1b[0m\n", stack, step)
			runStrategy(stack, step)
		}
		return
	}
	if !escalate {
		return
	}
	for _, step := range escalation {
		if healthOK(stack, siteCheck, cfg) {
			return
		}
		why := "unhealthy"
		if siteCheck {
			why = "still not serving"
		}
		fmt.Printf("  \x1b[33m↑ %s %s → %s\x1b[0m\n", stack, why, step)
		runStrategy(stack, step)
	}
}

// parallelApply runs applyTo across stacks, `parallel` at a time.
func parallelApply(stacks []string, parallel int, strategy string, escalate, force bool, escalation []string, siteCheck bool, cfg map[string]string) {
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for _, s := range stacks {
		wg.Add(1)
		sem <- struct{}{}
		go func(stack string) {
			defer wg.Done()
			defer func() { <-sem }()
			applyTo(stack, strategy, escalate, force, escalation, siteCheck, cfg)
		}(s)
	}
	wg.Wait()
}

// ── boot ────────────────────────────────────────────────────────────────────

func runBoot() {
	banner()
	bc := loadBootConfig()
	fmt.Printf("\x1b[1;32m⏻ Stacks controlled boot\x1b[0m  "+
		"delay=%ds parallel=%d strategy=%s force=%v override-docker=%v\n",
		bc.delay, bc.parallel, bc.strategy, bc.force, bc.overrideDocker)
	fmt.Printf("  boot list (%d): %s\n", len(bc.stacks), strings.Join(bc.stacks, " "))

	if bc.delay > 0 {
		fmt.Printf("  waiting %ds before starting…\n", bc.delay)
		time.Sleep(time.Duration(bc.delay) * time.Second)
	}

	// Docker-startup control (toggle, enforced every boot):
	//   boot_override_docker=1 → set managed containers restart=no so Docker
	//       stops launching them; stacks becomes the sole startup authority
	//       (the watchdog handles crash-restart).
	//   boot_override_docker=0 → set them back to restart=unless-stopped, i.e.
	//       hand startup BACK to Docker (regular Docker auto-start). So turning
	//       the option off cleanly undoes a previous override.
	bootApplyDockerRestart(bc.stacks, bc.overrideDocker)

	bootCfg := configLoad()
	parallelApply(bc.stacks, bc.parallel, bc.strategy, bc.escalate, bc.force, bc.escalation, false, bootCfg)

	if bc.downloadMissing {
		bootDownloadMissing(bc.stacks)
	}

	// summary
	up, down := 0, 0
	for _, s := range bc.stacks {
		if stackHealthy(s) {
			up++
		} else {
			down++
		}
	}
	fmt.Printf("\x1b[1;32m✔ boot done\x1b[0m  %d up / %d not-yet-healthy\n", up, down)

	// Optional: after bring-up, verify the actual SITES serve (not just that
	// containers are running) and heal any whose route is broken (502/404).
	// Gated behind boot_verify_sites so it's off by default. A short settle
	// wait gives apps time to bind before the first probe, and each healed
	// stack is left alone (no second probe this pass) to avoid a boot loop.
	cfg := configLoad()
	if cfgBoolKey(cfg, "BOOT_VERIFY_SITES", false) {
		settle := cfgInt(cfg, "HEAL_GRACE", 120)
		if settle > 30 {
			settle = 30 // boot settle is capped short; full grace is the watchdog's job
		}
		fmt.Printf("\x1b[36m🔎 verifying sites (settle %ds)…\x1b[0m\n", settle)
		time.Sleep(time.Duration(settle) * time.Second)
		var broken []string
		for _, s := range bc.stacks {
			if stackHealthy(s) && !stackSiteOK(s, cfg) {
				fmt.Printf("\x1b[31m✘ %s: site not serving after boot — healing\x1b[0m\n", s)
				broken = append(broken, s)
			}
		}
		if len(broken) > 0 {
			parallelApply(broken, bc.parallel, bc.watchStrategy,
				bc.watchEscalate, bc.watchForce, bc.watchEscalation, true, cfg)
		} else {
			fmt.Println("\x1b[32m  all boot sites serving ✔\x1b[0m")
		}
	}
}

// bootApplyDockerRestart enforces the Docker-startup-control toggle:
//   override=true  → restart=no            (Docker won't auto-start; stacks does)
//   override=false → restart=unless-stopped (hand startup back to Docker)
// It only touches containers whose policy actually differs, so it's cheap and
// doesn't churn anything already in the desired state.
func bootApplyDockerRestart(stacks []string, override bool) {
	want := "unless-stopped"
	msg := "\x1b[35m⚙ Docker auto-start active (restart=unless-stopped) — set boot_override_docker=1 to take control\x1b[0m"
	if override {
		want = "no"
		msg = "\x1b[35m⚙ override-docker ON — Docker auto-start DISABLED (restart=no); stacks controls startup\x1b[0m"
	}
	fmt.Println("  " + msg)
	changed := 0
	for _, s := range stacks {
		for name := range stackContainerStates(s, true) {
			cur, _ := exec.Command("docker", "inspect", "-f", "{{.HostConfig.RestartPolicy.Name}}", name).Output()
			if strings.TrimSpace(string(cur)) == want {
				continue
			}
			if exec.Command("docker", "update", "--restart="+want, name).Run() == nil {
				changed++
			}
		}
	}
	if changed > 0 {
		fmt.Printf("    (updated %d container restart policies)\n", changed)
	}
}

// bootDownloadMissing pre-pulls images for every stack NOT in the boot list and
// leaves them stopped (so visiting/wake-on-demand is instant later).
func bootDownloadMissing(bootList []string) {
	inBoot := map[string]bool{}
	for _, s := range bootList {
		inBoot[s] = true
	}
	for _, s := range dispStackList() {
		if inBoot[s] {
			continue
		}
		fmt.Printf("  \x1b[34m⤓ pre-pulling %s\x1b[0m\n", s)
		cmd := exec.Command(selfBin(), "pull", s)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		_ = cmd.Run()
	}
}

// ── auto-pull missing images (watchdog) ─────────────────────────────────────

// bootImageRe matches `image: repo:tag` lines in a compose file.
var bootImageRe = regexp.MustCompile(`(?m)^[\t ]*image:[\t ]*["']?([^"'#\s]+)`)

// stackImageRefs returns the deduped image references a stack declares, read
// from its compose file (universal: works wherever the file lives).
func stackImageRefs(stack string) []string {
	f := stackFileFor(stack)
	if f == "" {
		return nil
	}
	data, err := os.ReadFile(f)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var imgs []string
	for _, m := range bootImageRe.FindAllStringSubmatch(string(data), -1) {
		img := strings.TrimSpace(m[1])
		if img == "" || strings.Contains(img, "${") || seen[img] { // skip unresolved ${VARS}
			continue
		}
		seen[img] = true
		imgs = append(imgs, img)
	}
	return imgs
}

// pullGuard ensures only ONE background pull sweep runs at a time, so a slow
// pull never overlaps itself across watchdog ticks.
var pullGuard sync.Mutex
var pullRunning bool

// maybePullMissing — when WATCH_PULL_MISSING is on, scan the watched stacks for
// any declared image that isn't present locally and pull them in the background,
// ONE AT A TIME (gentle), via the Docker API. Non-blocking: the watchdog keeps
// checking/healing sites while images download. A new sweep won't start while a
// previous one is still pulling.
func maybePullMissing(stacks []string, cfg map[string]string) {
	if !cfgBoolKey(cfg, "WATCH_PULL_MISSING", false) {
		return
	}
	pullGuard.Lock()
	if pullRunning {
		pullGuard.Unlock()
		return
	}
	// collect what's missing (cheap: local inspects) before committing to a run
	seen := map[string]bool{}
	var missing []string
	for _, s := range stacks {
		for _, img := range stackImageRefs(s) {
			if seen[img] {
				continue
			}
			seen[img] = true
			if !imageExistsLocal(img) {
				missing = append(missing, img)
			}
		}
	}
	if len(missing) == 0 {
		pullGuard.Unlock()
		return
	}
	pullRunning = true
	pullGuard.Unlock()

	go func() {
		defer func() { pullGuard.Lock(); pullRunning = false; pullGuard.Unlock() }()
		fmt.Printf("\x1b[36m⤓ watchdog: %d image(s) missing — pulling one at a time…\x1b[0m\n", len(missing))
		for _, img := range missing {
			if imageExistsLocal(img) { // may have arrived via another path
				continue
			}
			fmt.Printf("\x1b[36m  ⤓ pulling %s\x1b[0m\n", img)
			if err := apiPullImage(img); err != nil {
				fmt.Printf("\x1b[33m  ⚠ pull failed for %s: %v\x1b[0m\n", img, err)
			} else {
				fmt.Printf("\x1b[32m  ✔ pulled %s\x1b[0m\n", img)
			}
		}
	}()
}

// ── watchdog ────────────────────────────────────────────────────────────────

// extractHosts pulls every Host(`…`) hostname out of a blob of Traefik rule text.
func extractHosts(s string) []string {
	seen := map[string]bool{}
	var hosts []string
	for {
		i := strings.Index(s, "Host(`")
		if i < 0 {
			break
		}
		s = s[i+6:]
		j := strings.IndexByte(s, '`')
		if j < 0 {
			break
		}
		if h := s[:j]; !seen[h] && strings.Contains(h, ".") {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// stackHosts finds the public hostnames a stack serves — UNIVERSAL: it reads the
// Traefik router rules from the stack's container LABELS via the Docker API (the
// standard label-provider way that works on ANY Docker+Traefik host), and only
// falls back to a Traefik file-provider dynamics file (DYNAMICS_DIR/<stack>.yml)
// when there are no labels (file-provider setups like Bellz's).
func stackHosts(stack string, cfg map[string]string) []string {
	// 1) Docker labels on the stack's containers (universal)
	out, _ := exec.Command("docker", "ps", "--filter",
		"label=com.docker.compose.project="+stack, "--format", "{{.Names}}").Output()
	seen := map[string]bool{}
	var hosts []string
	for _, name := range strings.Fields(string(out)) {
		lbl, _ := exec.Command("docker", "inspect", "-f",
			"{{range .Config.Labels}}{{println .}}{{end}}", name).Output()
		for _, h := range extractHosts(string(lbl)) {
			if !seen[h] {
				seen[h] = true
				hosts = append(hosts, h)
			}
		}
	}
	if len(hosts) > 0 {
		return hosts
	}
	// 2) fallback: a Traefik file-provider dynamics file for this stack
	dir := cfg["DYNAMICS_DIR"]
	if dir == "" {
		dir = filepath.Join(stacksDir(), "..", "Configs", "Dynamics")
	}
	if data, err := os.ReadFile(filepath.Join(dir, stack+".yml")); err == nil {
		return extractHosts(string(data))
	}
	return nil
}

// siteOK does an HTTP check of a public site (following redirects) and returns
// whether the FINAL status is in the acceptable set. Following redirects is key:
// a working auth gate ends at a 200 login page, but a BROKEN gate ends at a 404 —
// exactly the Vaultwarden case (302→404).
func siteOK(host string, cfg map[string]string) bool {
	ok := cfg["WATCH_SITE_OK_CODES"]
	if ok == "" {
		ok = "200,204,301,302,307,308,401,403,405"
	}
	to := cfg["WATCH_SITE_TIMEOUT"]
	if to == "" {
		to = "12"
	}
	out, _ := exec.Command("curl", "-skL", "-m", to, "-o", "/dev/null",
		"-w", "%{http_code}", "https://"+host+"/").Output()
	code := strings.TrimSpace(string(out))
	if code == "" || code == "000" {
		return false // timeout / connection failure
	}
	return strings.Contains(","+ok+",", ","+code+",")
}

// dynHostContainers parses a stack's Traefik file-provider dynamics into a
// host→backing-container map (router.rule Host(`x`) → router.service → that
// service's loadBalancer url http://CONTAINER:port). This is what lets the
// watchdog tell a genuinely-wedged site (its container is RUNNING but the page is
// dead) apart from a Zero-Scale/Sablier sleeper (its container is intentionally
// STOPPED, so a 502 is expected). Universal-ish: it's the standard file-provider
// shape Bellz uses; label-provider hosts fall back to the stack-level check.
func dynHostContainers(stack string, cfg map[string]string) map[string]string {
	dir := cfg["DYNAMICS_DIR"]
	if dir == "" {
		dir = filepath.Join(stacksDir(), "..", "Configs", "Dynamics")
	}
	data, err := os.ReadFile(filepath.Join(dir, stack+".yml"))
	if err != nil {
		return nil
	}
	type rtr struct {
		hosts []string
		svc   string
	}
	var routers []rtr
	svcContainer := map[string]string{}
	urlRe := regexp.MustCompile(`https?://([A-Za-z0-9_.-]+)`)
	section := "" // "routers" | "services"
	cur := -1     // index into routers for the router currently being parsed
	curSvc := ""
	for _, ln := range strings.Split(string(data), "\n") {
		trim := strings.TrimSpace(ln)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		indent := len(ln) - len(strings.TrimLeft(ln, " "))
		if indent <= 2 && strings.HasSuffix(trim, ":") { // section header (routers:/services:/http:)
			switch strings.TrimSuffix(trim, ":") {
			case "routers":
				section, cur = "routers", -1
			case "services":
				section, curSvc = "services", ""
			case "http", "tcp", "udp":
				// container key — keep current section context
			default:
				section = ""
			}
			continue
		}
		if indent == 4 && strings.HasSuffix(trim, ":") { // a router name / service name
			name := strings.TrimSuffix(trim, ":")
			if section == "routers" {
				routers = append(routers, rtr{})
				cur = len(routers) - 1
			} else if section == "services" {
				curSvc = name
			}
			continue
		}
		switch section {
		case "routers":
			if cur < 0 {
				continue
			}
			if strings.HasPrefix(trim, "rule:") {
				routers[cur].hosts = extractHosts(trim)
			} else if strings.HasPrefix(trim, "service:") {
				routers[cur].svc = strings.Trim(strings.TrimSpace(strings.TrimPrefix(trim, "service:")), `"'`)
			}
		case "services":
			if curSvc == "" {
				continue
			}
			if m := urlRe.FindStringSubmatch(trim); m != nil {
				if _, ok := svcContainer[curSvc]; !ok {
					svcContainer[curSvc] = m[1]
				}
			}
		}
	}
	out := map[string]string{}
	for _, r := range routers {
		if c := svcContainer[r.svc]; c != "" {
			for _, h := range r.hosts {
				out[h] = c
			}
		}
	}
	return out
}

// (containerRunning lives in zeroscale.go — reused here.)

// wedgedSites returns the containers of a stack that are RUNNING yet whose public
// page does not serve — exactly the "up but won't connect / throwing errors"
// failure. Containers that are STOPPED are skipped (parked Zero-Scale sleepers →
// a 502 is expected; the watchdog must never wake them). One host per container is
// probed (representative).
func wedgedSites(stack string, cfg map[string]string) []string {
	hc := dynHostContainers(stack, cfg)
	if len(hc) == 0 {
		// no file-provider map (label-provider / no dynamics) → fall back to the
		// simple primary-host check, but still respect "no host = nothing to do"
		hosts := stackHosts(stack, cfg)
		if len(hosts) == 0 || siteOK(hosts[0], cfg) {
			return nil
		}
		return []string{stack}
	}
	checked := map[string]bool{}
	var wedged []string
	for host, cont := range hc {
		if checked[cont] {
			continue
		}
		checked[cont] = true
		if !containerRunning(cont) {
			continue // parked/sleeping on purpose → leave to Zero Scale
		}
		if !siteOK(host, cfg) {
			wedged = append(wedged, cont)
		}
	}
	return wedged
}

// stackSiteOK is true when none of the stack's RUNNING-backed sites are wedged.
func stackSiteOK(stack string, cfg map[string]string) bool {
	return len(wedgedSites(stack, cfg)) == 0
}

// dynSvcURLRe captures scheme + container + port from a Traefik service url, e.g.
// http://qbittorrent:8280  →  ("http","qbittorrent","8280").
var dynSvcURLRe = regexp.MustCompile(`(https?)://([A-Za-z0-9_.-]+):(\d+)`)

// dynContainerPorts maps container name → {scheme, port} from a stack's Traefik
// service loadBalancer urls. This is the container's OWN internal endpoint — what
// lets the watchdog probe the app directly (container IP:port) instead of through
// the public edge, so an edge/cert/Pangolin 5xx is never mistaken for a sick app.
func dynContainerPorts(stack string, cfg map[string]string) map[string][2]string {
	dir := cfg["DYNAMICS_DIR"]
	if dir == "" {
		dir = filepath.Join(stacksDir(), "..", "Configs", "Dynamics")
	}
	data, err := os.ReadFile(filepath.Join(dir, stack+".yml"))
	if err != nil {
		return nil
	}
	out := map[string][2]string{}
	for _, m := range dynSvcURLRe.FindAllStringSubmatch(string(data), -1) {
		scheme, cont, port := m[1], m[2], m[3]
		if _, seen := out[cont]; !seen { // first url for a container wins
			out[cont] = [2]string{scheme, port}
		}
	}
	return out
}

// containerServes probes a container's OWN web endpoint directly (its docker IP +
// the internal port Traefik routes to) and reports whether it answers acceptably.
// Returns true when we can't determine an IP (don't flag what we can't see).
func containerServes(name, scheme, port string, cfg map[string]string) bool {
	out, _ := exec.Command("docker", "inspect", "-f",
		`{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}`, name).Output()
	ips := strings.Fields(string(out))
	if len(ips) == 0 {
		return true
	}
	to := cfg["WATCH_SITE_TIMEOUT"]
	if to == "" {
		to = "8"
	}
	ok := cfg["WATCH_SITE_OK_CODES"]
	if ok == "" {
		ok = "200,204,301,302,307,308,401,403,405"
	}
	code, _ := exec.Command("curl", "-sk", "-m", to, "-o", "/dev/null",
		"-w", "%{http_code}", scheme+"://"+ips[0]+":"+port+"/").Output()
	cc := strings.TrimSpace(string(code))
	if cc == "" || cc == "000" {
		return false // refused / timed out → the app itself isn't answering
	}
	return strings.Contains(","+ok+",", ","+cc+",")
}

// bootYamlList reads a top-level string list (key:) out of boot.yaml. Parsed
// without a YAML dep so it can't drag in surprises.
func bootYamlList(key string) []string {
	data, err := os.ReadFile(filepath.Join(configDir(), "boot.yaml"))
	if err != nil {
		return nil
	}
	var out []string
	inList := false
	for _, raw := range strings.Split(string(data), "\n") {
		ln := strings.TrimRight(raw, "\r")
		if strings.HasPrefix(ln, key+":") {
			inList = true
			continue
		}
		if !inList {
			continue
		}
		ts := strings.TrimSpace(ln)
		switch {
		case ts == "" || strings.HasPrefix(ts, "#"):
			continue
		case strings.HasPrefix(ts, "- "):
			out = append(out, strings.Trim(strings.TrimSpace(ts[2:]), `"'`))
		default:
			return out // a new top-level key — this list ended
		}
	}
	return out
}

// loadBossContainers = the BOOT list (what starts when the computer starts).
func loadBossContainers() []string { return bootYamlList("boot_containers") }

// loadWatchContainers = the WATCHDOG list (what stays alive 24/7). It's SEPARATE
// from the boot list: set watch_containers: in boot.yaml to keep alive only a
// subset of what boots. When it's not set, it falls back to the full boot list —
// so by default the watchdog guards everything that starts.
func loadWatchContainers() []string {
	if w := bootYamlList("watch_containers"); len(w) > 0 {
		return w
	}
	return bootYamlList("boot_containers")
}

// containerStackIndex maps a container name → the stack that owns it, so even a
// DOWN or removed boss can be traced back to the stack that repairs/recreates it.
// Stack-file container_name: declarations seed it (covers removed containers);
// live compose labels then overwrite (authoritative for anything that exists).
var ctrNameRe = regexp.MustCompile(`(?m)^\s*container_name:\s*["']?([A-Za-z0-9_.-]+)`)

func containerStackIndex() map[string]string {
	idx := map[string]string{}
	for _, s := range dispStackList() {
		f := stackFileFor(s)
		if f == "" {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, m := range ctrNameRe.FindAllStringSubmatch(string(data), -1) {
			idx[m[1]] = s
		}
	}
	out, _ := exec.Command("docker", "ps", "-a", "--format",
		`{{.Names}}` + "\t" + `{{.Label "com.docker.compose.project"}}`).Output()
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, proj, _ := strings.Cut(ln, "\t")
		if name != "" && proj != "" {
			idx[name] = proj
		}
	}
	return idx
}

// hostForContainer returns a public hostname this container serves (if any), by
// inverting the stack's Traefik host→container map.
func hostForContainer(stack, container string, cfg map[string]string) string {
	for h, c := range dynHostContainers(stack, cfg) {
		if c == container {
			return h
		}
	}
	return ""
}

func cmdWatch(args []string) {
	once, dryRun := false, false
	for _, a := range args {
		switch a {
		case "--once", "once":
			once = true
		case "--check", "--dry", "--dry-run", "check":
			once, dryRun = true, true // one pass, report only, heal nothing
		}
	}
	bc := loadBootConfig()
	if !bc.watchEnabled && !once {
		fmt.Println("watchdog disabled (watch_enabled=0) — exiting")
		return
	}

	// The watchdog guards the 24/7 BOSS LIST (boot.yaml boot_containers), container
	// by container — not whole stacks. Each boss must stay running and, if it has a
	// public page, keep serving it. Everything NOT on the list (Zero-Scale/Sablier
	// sleepers) is left completely alone, so we never fight wake-on-visit. If the
	// boss list is empty, fall back to every container in the watched stacks.
	bossSource := "boss list"
	bosses := loadWatchContainers()
	if len(bosses) == 0 {
		bossSource = "watched stacks"
		seen := map[string]bool{}
		for _, s := range bc.watchStacks {
			for name := range stackContainerStates(s, true) {
				if !seen[name] {
					seen[name] = true
					bosses = append(bosses, name)
				}
			}
		}
	}
	fmt.Printf("\x1b[1;32m🐶 Stacks watchdog\x1b[0m  every %ds, watching %d containers (%s); site-check + restart→repair→recreate→fix\n",
		bc.watchInterval, len(bosses), bossSource)

	// Per-CONTAINER heal state: how far up the ladder we've escalated, and when we
	// last touched it (grace/backoff). Seed grace with "now" = startup warmup.
	attempts := map[string]int{}
	lastHeal := map[string]time.Time{}
	startNow := time.Now()
	for _, c := range bosses {
		lastHeal[c] = startNow
	}

	for {
		// reload config each sweep so Settings-tab edits take effect live
		bc = loadBootConfig()
		cfg := configLoad()
		checkSites := cfgBoolKey(cfg, "WATCH_SITES", false)
		grace := time.Duration(cfgInt(cfg, "HEAL_GRACE", 120)) * time.Second
		// Once the full ladder is spent on a still-broken container (e.g. the VPN
		// chain that can't come up for an external reason), back WAY off so we don't
		// hammer it — retry only every WATCH_BACKOFF seconds.
		backoff := time.Duration(cfgInt(cfg, "WATCH_BACKOFF", 1800)) * time.Second
		// WATCH_SITE_SKIP: containers whose PAGE we never check (still kept running) —
		// for sites whose errors aren't a container problem (e.g. an edge/cert issue),
		// so the watchdog won't pointlessly escalate to fix and clobber manual tweaks.
		siteSkip := map[string]bool{}
		for _, n := range strings.Fields(cfg["WATCH_SITE_SKIP"]) {
			siteSkip[n] = true
		}
		// refresh the boss list + name→stack index each sweep (Settings/boot.yaml live)
		if b := loadWatchContainers(); len(b) > 0 {
			bosses = b
		}
		idx := containerStackIndex()
		// container → {scheme,port} of its OWN endpoint, gathered once per sweep from
		// the dynamics of every stack a boss lives in. Used to probe apps directly
		// (immune to edge/cert noise) rather than via the public URL.
		endpoints := map[string][2]string{}
		epDone := map[string]bool{}
		for _, c := range bosses {
			s := idx[c]
			if s == "" || epDone[s] {
				continue
			}
			epDone[s] = true
			for cont, ep := range dynContainerPorts(s, cfg) {
				endpoints[cont] = ep
			}
		}
		// background, one-at-a-time pre-pull of any missing images for the stacks
		maybePullMissing(bc.watchStacks, cfg)

		for _, c := range bosses {
			running := containerRunning(c)
			// classify: "" = fine, "down" = not running, "wedged" = up but page dead.
			// "wedged" probes the container's OWN endpoint (not the public URL) so an
			// edge/cert/Pangolin 5xx can never be mistaken for a sick container.
			reason := ""
			if !running {
				reason = "down"
			} else if checkSites && !siteSkip[c] {
				if ep, ok := endpoints[c]; ok && !containerServes(c, ep[0], ep[1], cfg) {
					reason = "wedged"
				}
			}
			if reason == "" {
				if attempts[c] != 0 { // recovered → reset its counter
					delete(attempts, c)
				}
				lastHeal[c] = time.Now() // healthy now → restart the grace clock
				continue
			}

			// SAFE heal: ALWAYS a container-scoped `docker restart` — NEVER a
			// stack-level repair/recreate/fix. Stack ops recreate sibling
			// containers (e.g. healing crowdsec would recreate net_2 and take
			// traefik down with it — that caused a real outage). A container
			// can't take down its neighbours. After the first try, back off so a
			// permanently-broken container isn't hammered; the heavier
			// repair/recreate/fix stay MANUAL (menu/CLI) by design.
			spent := attempts[c] >= 1
			wait := grace
			if spent {
				wait = backoff
			}
			if !dryRun { // --check ignores grace so it reports the true current state
				if t, ok := lastHeal[c]; ok && time.Since(t) < wait {
					continue // still inside its grace/backoff window
				}
			}
			stack := idx[c]
			if dryRun {
				fmt.Printf("\x1b[33m• would heal %s (%s): %s → restart\x1b[0m\n", c, stack, reason)
				continue // report only — touch nothing
			}
			fmt.Printf("\x1b[31m✘ %s (%s): %s → restart\x1b[0m\n", c, stack, reason)
			_ = exec.Command("docker", "restart", "-t", "10", c).Run()
			lastHeal[c] = time.Now()
			attempts[c]++
		}

		if dryRun {
			fmt.Println("\x1b[36m(dry-run: nothing was changed)\x1b[0m")
			return
		}
		if once {
			return
		}
		time.Sleep(time.Duration(bc.watchInterval) * time.Second)
	}
}

// ── boot subcommand (install / uninstall / status) ───────────────────────────

func cmdBoot(args []string) {
	action := ""
	if len(args) > 0 {
		action = strings.TrimPrefix(strings.TrimPrefix(args[0], "--"), "-")
	}
	switch action {
	case "install":
		bootInstall()
	case "uninstall", "remove":
		bootUninstall()
	case "status", "":
		bootStatus()
	case "run":
		runBoot()
	default:
		fmt.Println("usage: stacks boot [install|uninstall|status]")
	}
}

const bootUnit = "stacks-boot.service"
const watchUnit = "stacks-watch.service"

// ensureDocker makes sure the one dependency the program needs — the Docker
// Engine + daemon — is present and running. The Docker API client is compiled
// INTO this binary (the Go SDK), so nothing separate is needed for that; we just
// need `docker` installed and the socket reachable. Installs Docker (with the
// compose plugin) via the official convenience script if it's missing.
func ensureDocker() {
	if _, err := exec.LookPath("docker"); err == nil {
		if exec.Command("docker", "info").Run() == nil {
			fmt.Println("  ✔ Docker present and the daemon is reachable")
			return
		}
		fmt.Println("  Docker is installed but the daemon isn't running — starting it…")
		_ = exec.Command("systemctl", "enable", "--now", "docker").Run()
		return
	}
	fmt.Println("  \x1b[33mDocker not found — installing it (official get.docker.com script)…\x1b[0m")
	c := exec.Command("sh", "-c", "curl -fsSL https://get.docker.com | sh")
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		fmt.Println("  ✘ Docker install failed:", err, "— install it manually, then re-run.")
		return
	}
	_ = exec.Command("systemctl", "enable", "--now", "docker").Run()
	if u := os.Getenv("SUDO_USER"); u != "" {
		_ = exec.Command("usermod", "-aG", "docker", u).Run()
		fmt.Println("  added", u, "to the docker group (re-login for it to take effect)")
	}
	fmt.Println("  ✔ Docker installed + enabled")
}

func bootInstall() {
	self := selfBin()
	confDir := configDir()
	// Bootstrap the one real dependency first: Docker Engine + daemon.
	fmt.Println("\x1b[1;36m▸ checking dependencies…\x1b[0m")
	ensureDocker()
	bootSvc := fmt.Sprintf(`[Unit]
Description=Stacks controlled boot bring-up (before login)
After=docker.service network-online.target
Wants=docker.service network-online.target
Before=display-manager.service sddm.service

[Service]
Type=oneshot
RemainAfterExit=yes
Environment=STACKS_CONFIG_DIR=%s
ExecStart=%s up --boot
TimeoutStartSec=0

[Install]
WantedBy=multi-user.target
`, confDir, self)

	watchSvc := fmt.Sprintf(`[Unit]
Description=Stacks watchdog (keep watched stacks alive 24/7)
After=stacks-boot.service docker.service
Wants=docker.service

[Service]
Type=simple
Environment=STACKS_CONFIG_DIR=%s
ExecStart=%s watch
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
`, confDir, self)

	if err := writeUnit(bootUnit, bootSvc); err != nil {
		fmt.Println("✘ write boot unit:", err)
		fmt.Println("  (need root — try: sudo", self, "boot install)")
		return
	}
	if err := writeUnit(watchUnit, watchSvc); err != nil {
		fmt.Println("✘ write watch unit:", err)
		return
	}
	run := func(a ...string) { c := exec.Command("systemctl", a...); c.Stdout, c.Stderr = os.Stdout, os.Stderr; _ = c.Run() }
	run("daemon-reload")
	run("enable", bootUnit)
	run("enable", watchUnit)
	fmt.Println("\x1b[1;32m✔ installed + enabled\x1b[0m", bootUnit, "and", watchUnit)
	fmt.Println("  boot bring-up now runs before login; watchdog keeps stacks alive.")
	fmt.Println("  tune everything in the Settings tab (boot_* / watch_* / up_parallel).")
}

func bootUninstall() {
	run := func(a ...string) { c := exec.Command("systemctl", a...); c.Stdout, c.Stderr = os.Stdout, os.Stderr; _ = c.Run() }
	run("disable", "--now", bootUnit)
	run("disable", "--now", watchUnit)
	for _, u := range []string{bootUnit, watchUnit} {
		p := filepath.Join("/etc/systemd/system", u)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			fmt.Println("✘ remove", p, err, "(need root?)")
		}
	}
	run("daemon-reload")
	fmt.Println("\x1b[1;32m✔ removed\x1b[0m", bootUnit, "and", watchUnit)
}

func bootStatus() {
	bc := loadBootConfig()
	fmt.Println("\x1b[1;36mStacks boot/watchdog status\x1b[0m")
	fmt.Printf("  boot: delay=%ds parallel=%d strategy=%s escalate=%v force=%v\n",
		bc.delay, bc.parallel, bc.strategy, bc.escalate, bc.force)
	fmt.Printf("        escalation=[%s] download_missing=%v override_docker=%v\n",
		strings.Join(bc.escalation, " "), bc.downloadMissing, bc.overrideDocker)
	fmt.Printf("        boot_stacks (%d): %s\n", len(bc.stacks), strings.Join(bc.stacks, " "))
	fmt.Printf("  watch: enabled=%v interval=%ds strategy=%s escalate=%v force=%v\n",
		bc.watchEnabled, bc.watchInterval, bc.watchStrategy, bc.watchEscalate, bc.watchForce)
	fmt.Printf("         watch_stacks (%d): %s\n", len(bc.watchStacks), strings.Join(bc.watchStacks, " "))
	for _, u := range []string{bootUnit, watchUnit} {
		out, _ := exec.Command("systemctl", "is-enabled", u).Output()
		st := strings.TrimSpace(string(out))
		if st == "" {
			st = "not-installed"
		}
		fmt.Printf("  %-20s %s\n", u, st)
	}
	fmt.Println("\n  live health:")
	for _, s := range bc.stacks {
		mark := "\x1b[31m✘\x1b[0m"
		if stackHealthy(s) {
			mark = "\x1b[32m✔\x1b[0m"
		}
		fmt.Printf("    %s %s\n", mark, s)
	}
}

func writeUnit(name, body string) error {
	return os.WriteFile(filepath.Join("/etc/systemd/system", name), []byte(body), 0644)
}
