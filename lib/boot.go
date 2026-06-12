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

// runStrategy re-invokes the stacks binary to apply one strategy to one stack.
func runStrategy(stack, strategy string) {
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

// applyTo runs the gentle strategy then (force OR unhealthy) escalates in order.
func applyTo(stack, strategy string, escalate, force bool, escalation []string) {
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
		if stackHealthy(stack) {
			return
		}
		fmt.Printf("  \x1b[33m↑ %s unhealthy → %s\x1b[0m\n", stack, step)
		runStrategy(stack, step)
	}
}

// parallelApply runs applyTo across stacks, `parallel` at a time.
func parallelApply(stacks []string, parallel int, strategy string, escalate, force bool, escalation []string) {
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for _, s := range stacks {
		wg.Add(1)
		sem <- struct{}{}
		go func(stack string) {
			defer wg.Done()
			defer func() { <-sem }()
			applyTo(stack, strategy, escalate, force, escalation)
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

	parallelApply(bc.stacks, bc.parallel, bc.strategy, bc.escalate, bc.force, bc.escalation)

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

// ── watchdog ────────────────────────────────────────────────────────────────

func cmdWatch(args []string) {
	once := false
	for _, a := range args {
		if a == "--once" || a == "once" {
			once = true
		}
	}
	bc := loadBootConfig()
	if !bc.watchEnabled && !once {
		fmt.Println("watchdog disabled (watch_enabled=0) — exiting")
		return
	}
	fmt.Printf("\x1b[1;32m🐶 Stacks watchdog\x1b[0m  every %ds, strategy=%s, %d stacks\n",
		bc.watchInterval, bc.watchStrategy, len(bc.watchStacks))
	for {
		// reload config each sweep so Settings-tab edits take effect live
		bc = loadBootConfig()
		var down []string
		for _, s := range bc.watchStacks {
			if !stackHealthy(s) {
				down = append(down, s)
			}
		}
		if len(down) > 0 {
			fmt.Printf("\x1b[33m… healing %d: %s\x1b[0m\n", len(down), strings.Join(down, " "))
			parallelApply(down, bc.parallel, bc.watchStrategy,
				bc.watchEscalate, bc.watchForce, bc.watchEscalation)
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
