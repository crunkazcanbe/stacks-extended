package lib

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// ===== from buildcmd.go =====

// buildcmd.go — faithful Go port of stacks_build.py.
//
// Interactive service scaffolder. Clean self-contained UI with a 9-line loading
// bar and inline questions. Ported line-for-line: the UI primitives (_draw,
// init_ui, update, clear_ui), ask, fzf, load_conf, hub_search, detect_db,
// find_existing, next_ip, rand_mac, setup_db, build_svc, probe_healthcheck,
// build_db_block, inject, and main().
//
// Paths use the universal helpers (stacksDir()/configDir()/home()); the Python
// hardcodes /home/bellzserver and /home/loveiznothin.

// ── UI — exactly 9 lines, never changes ───────────────────────────────────────

const buildUIH = 9 // MUST match lines printed in buildDraw()

var (
	buildStTarget = "build"
	buildStSvc    = "service"
	buildStAction = "Initializing..."
	buildStPct    = 0
	buildDrawn    = false
)

// buildTermCols mirrors the terminal-size probe in _draw(): try stdout, then
// stderr, capped at 120, defaulting to 80.
func buildTermCols() int {
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		if w, _, err := buildTermSize(f); err == nil && w > 0 {
			if w > 120 {
				return 120
			}
			return w
		}
	}
	return 80
}

// buildTermSize gets the terminal width/height via the COLUMNS env / stty as a
// portable fallback (Python uses os.get_terminal_size on the fd).
func buildTermSize(f *os.File) (int, int, error) {
	// Try `stty size` against the file's tty.
	cmd := exec.Command("stty", "size")
	cmd.Stdin = f
	out, err := cmd.Output()
	if err == nil {
		parts := strings.Fields(strings.TrimSpace(string(out)))
		if len(parts) == 2 {
			rows, e1 := strconv.Atoi(parts[0])
			cols, e2 := strconv.Atoi(parts[1])
			if e1 == nil && e2 == nil {
				return cols, rows, nil
			}
		}
	}
	if c := os.Getenv("COLUMNS"); c != "" {
		if v, e := strconv.Atoi(c); e == nil {
			return v, 24, nil
		}
	}
	return 0, 0, fmt.Errorf("no tty size")
}

// buildClip mirrors Python's s[:n] slice (rune-safe).
func buildClip(s string, n int) string {
	if n < 0 {
		n = 0
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func buildDraw() {
	cols := buildTermCols()
	bw := 38
	if cols-18 < bw {
		bw = cols - 18
	}
	if bw < 0 {
		bw = 0
	}
	filled := (buildStPct * bw) / 100
	bar := strings.Repeat("=", filled) + strings.Repeat("-", bw-filled)

	if buildDrawn {
		fmt.Fprintf(os.Stdout, "\033[%dA", buildUIH)
	}

	actionClip := cols - 8
	if actionClip < 0 {
		actionClip = 0
	}

	fmt.Fprint(os.Stdout,
		"\r\033[K\033[38;5;81m     _             _        \033[0m\n"+
			"\r\033[K\033[38;5;81m ___| |_ __ _  ___| | _____ \033[0m\n"+
			"\r\033[K\033[38;5;81m/ __| __/ _` |/ __| |/ / __|\033[0m\n"+
			"\r\033[K\033[38;5;81m\\__ \\ || (_| | (__|   <\\__ \\\033[0m\n"+
			"\r\033[K\033[38;5;218m|___/\\__\\__,_|\\___|_|\\_\\___/ \033[0m\n"+
			"\r\033[K\n"+
			fmt.Sprintf("\r\033[K\033[1;33m  📦 %s \033[90m|\033[0m \033[1;36m%s\033[0m\n",
				buildClip(buildStTarget, 30), buildClip(buildStSvc, 35))+
			fmt.Sprintf("\r\033[K\033[1;34m  ▶ %s\033[0m\n", buildClip(buildStAction, actionClip))+
			fmt.Sprintf("\r\033[K\033[1;32m  [%s] %d%%\033[0m\n", bar, buildStPct),
	)
	os.Stdout.Sync()
	buildDrawn = true
}

func buildInitUI(target, svc string) {
	buildStTarget = target
	buildStSvc = svc
	buildDrawn = false
	fmt.Fprint(os.Stdout, strings.Repeat("\n", buildUIH))
	os.Stdout.Sync()
	buildDraw()
}

func buildUpdate(action string, pct int) {
	buildStAction = action
	buildStPct = pct
	buildDraw()
}

func buildClearUI() {
	if buildDrawn {
		fmt.Fprintf(os.Stdout, "\033[%dA\033[J", buildUIH)
		os.Stdout.Sync()
	}
}

// ── Ask — prints on line BELOW the bar, then erases ──────────────────────────

func buildAsk(prompt, def string) string {
	buildStAction = "❓ " + prompt
	buildUpdate(buildStAction, buildStPct)
	exec.Command("stty", "echo").Run()
	fmt.Fprintf(os.Stdout, "  \033[1;36m%s\033[0m [\033[1;33m%s\033[0m]: ", prompt, def)
	os.Stdout.Sync()
	reader := bufio.NewReader(os.Stdin)
	// Python: sys.stdin.readline() returns "" at EOF WITHOUT raising, so an EOF
	// (e.g. Ctrl-D) yields val=="" and the function returns the default — it does
	// NOT exit. (Ctrl-C raises KeyboardInterrupt and would terminate the process
	// via signal here anyway.) So treat a plain read error the same as empty input.
	line, _ := reader.ReadString('\n')
	val := strings.TrimSpace(line)
	// erase prompt line
	fmt.Fprint(os.Stdout, "\033[1A\r\033[K")
	os.Stdout.Sync()
	if val != "" {
		return val
	}
	return def
}

// ── fzf ───────────────────────────────────────────────────────────────────────

func buildFzf(items []string, header, prompt string) string {
	if len(items) == 0 {
		return ""
	}
	if prompt == "" {
		prompt = "▶ "
	}
	if _, err := exec.LookPath("fzf"); err != nil {
		for i, it := range items {
			fmt.Printf("  %d. %s\n", i+1, it)
		}
		v := buildAsk("Number", "1")
		// Python: items[int(v)-1] with a bare except -> items[0]. Mirror Python
		// list indexing, including negative wrap (v="0" -> items[-1] = last item).
		if n, e := strconv.Atoi(strings.TrimSpace(v)); e == nil {
			idx := n - 1
			if idx < 0 {
				idx += len(items)
			}
			if idx >= 0 && idx < len(items) {
				return items[idx]
			}
		}
		return items[0]
	}
	inp := strings.Join(items, "\n")
	tf, err := os.CreateTemp("", "*.txt")
	if err != nil {
		return ""
	}
	tfp := tf.Name()
	tf.WriteString(inp)
	tf.Close()

	cmdStr := fmt.Sprintf("cat %s | fzf --ansi --no-sort --layout=reverse "+
		"--height=~50%% --border=rounded --margin=1,3 "+
		"--header=%s --prompt=%s "+
		"--color=bg:#0a1628,bg+:#1a3a5c,fg:#c8d8e8,fg+:#ffffff,"+
		"hl:#4fc3f7,border:#2a6496,header:#4fc3f7,prompt:#81d4fa",
		buildShellQuote(tfp), buildShellQuote(header), buildShellQuote(prompt))

	tty, terr := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if terr != nil {
		os.Remove(tfp)
		return ""
	}
	defer tty.Close()
	cmd := exec.Command("sh", "-c", cmdStr)
	cmd.Stderr = tty
	out, runErr := cmd.Output()
	os.Remove(tfp)

	rc := 0
	if runErr != nil {
		rc = 1
		if ee, ok := runErr.(*exec.ExitError); ok {
			rc = ee.ExitCode()
		}
	}
	if rc != 0 {
		// Restore terminal after fzf exit
		exec.Command("tput", "reset").Run()
		buildDrawn = false
		fmt.Fprint(os.Stdout, strings.Repeat("\n", buildUIH))
		os.Stdout.Sync()
		return ""
	}
	outStr := strings.TrimSpace(string(out))
	var result string
	if outStr != "" {
		result = strings.SplitN(outStr, "\n", 2)[0]
	}
	// Restore terminal cleanly after fzf
	exec.Command("tput", "reset").Run()
	buildDrawn = false
	fmt.Fprint(os.Stdout, strings.Repeat("\n", buildUIH))
	os.Stdout.Sync()
	return result
}

// buildShellQuote mirrors shlex.quote for embedding in `sh -c`.
func buildShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if regexp.MustCompile(`^[a-zA-Z0-9_@%+=:,./-]+$`).MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// ── Config ────────────────────────────────────────────────────────────────────

// buildConf is the resolved build config (mirrors the dict from load_conf()).
type buildConf struct {
	UseCommonCaps   bool
	ExtraNetworks   []interface{}
	Cpuset          string
	CPUShares       int
	StopGracePeriod string
	StopSignal      string
	Restart         string
	User            string
	Blkio           bool
	Ulimits         bool
	DeployLimits    bool
	Logging         bool
	DNS             []string
	SablierGroup    string
	ExtraEnv        []string
	ExtraLabels     []string
	ExtraVolumes    []string
}

func buildLoadConf() buildConf {
	c := buildConf{
		UseCommonCaps:   true,
		ExtraNetworks:   []interface{}{},
		Cpuset:          "0-15",
		CPUShares:       4096,
		StopGracePeriod: "120s",
		StopSignal:      "SIGTERM",
		Restart:         "no",
		User:            "0:0",
		Blkio:           true,
		Ulimits:         true,
		DeployLimits:    true,
		Logging:         true,
		DNS:             []string{"192.168.1.114", "8.8.8.8"},
		SablierGroup:    "",
		ExtraEnv:        []string{"TZ=America/New_York"},
		ExtraLabels:     []string{},
		ExtraVolumes: []string{
			"/usr/lib/x86_64-linux-gnu/libtcmalloc_minimal.so.4:" +
				"/usr/lib/x86_64-linux-gnu/libtcmalloc_minimal.so.4:ro"},
	}
	// loadDoc('build') reads build.yaml / build.conf via the shared config layer
	// (equivalent to importing stacks_config and falling back to BUILD_CONF JSON).
	doc := loadDoc("build")
	for k, v := range doc {
		if strings.HasPrefix(k, "_") {
			continue
		}
		buildApplyConf(&c, k, v)
	}
	return c
}

func buildApplyConf(c *buildConf, key string, v interface{}) {
	switch key {
	case "use_common_caps":
		c.UseCommonCaps = buildAsBool(v, c.UseCommonCaps)
	case "extra_networks":
		if l, ok := v.([]interface{}); ok {
			c.ExtraNetworks = l
		}
	case "cpuset":
		c.Cpuset = buildAsStr(v, c.Cpuset)
	case "cpu_shares":
		c.CPUShares = buildAsInt(v, c.CPUShares)
	case "stop_grace_period":
		c.StopGracePeriod = buildAsStr(v, c.StopGracePeriod)
	case "stop_signal":
		c.StopSignal = buildAsStr(v, c.StopSignal)
	case "restart":
		c.Restart = buildAsStr(v, c.Restart)
	case "user":
		c.User = buildAsStr(v, c.User)
	case "blkio":
		c.Blkio = buildAsBool(v, c.Blkio)
	case "ulimits":
		c.Ulimits = buildAsBool(v, c.Ulimits)
	case "deploy_limits":
		c.DeployLimits = buildAsBool(v, c.DeployLimits)
	case "logging":
		c.Logging = buildAsBool(v, c.Logging)
	case "dns":
		c.DNS = buildAsStrList(v, c.DNS)
	case "sablier_group":
		c.SablierGroup = buildAsStr(v, c.SablierGroup)
	case "extra_env":
		c.ExtraEnv = buildAsStrList(v, c.ExtraEnv)
	case "extra_labels":
		c.ExtraLabels = buildAsStrList(v, c.ExtraLabels)
	case "extra_volumes":
		c.ExtraVolumes = buildAsStrList(v, c.ExtraVolumes)
	}
}

func buildAsBool(v interface{}, def bool) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "true" || s == "1" || s == "yes"
	}
	return def
}

func buildAsStr(v interface{}, def string) string {
	switch t := v.(type) {
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case float64:
		return strconv.Itoa(int(t))
	case bool:
		if t {
			return "True"
		}
		return "False"
	}
	return def
}

func buildAsInt(v interface{}, def int) int {
	switch t := v.(type) {
	case int:
		return t
	case float64:
		return int(t)
	case string:
		if n, e := strconv.Atoi(strings.TrimSpace(t)); e == nil {
			return n
		}
	}
	return def
}

func buildAsStrList(v interface{}, def []string) []string {
	if l, ok := v.([]interface{}); ok {
		out := make([]string, 0, len(l))
		for _, it := range l {
			out = append(out, buildAsStr(it, ""))
		}
		return out
	}
	return def
}

// ── Registry search — uses stacks regsearch TUI ───────────────────────────────

func buildHubSearch(term string) string {
	buildUpdate("Searching registries for: "+term, 15)
	// Clear UI so regsearch has full screen
	buildClearUI()
	// Use THIS binary's native regsearch (12 registries: Docker Hub, Hub
	// Official, ghcr.io, Self-Hosted, Quay, GitLab, Verified/AWS, Codeberg,
	// LinuxServer.io, Bitnami, Microsoft MCR, ArtifactHub). --select writes the
	// chosen image to /tmp/stacks_build_selected and exits. No Python.
	os.Remove("/tmp/stacks_build_selected")
	cmd := exec.Command(selfExe(), "regsearch", term, "--select")
	cmd.Env = dockerEnv()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
	// regsearch writes selected image to /tmp/stacks_build_selected
	selPath := "/tmp/stacks_build_selected"
	if data, err := os.ReadFile(selPath); err == nil {
		image := strings.TrimSpace(string(data))
		os.Remove(selPath)
		if image != "" {
			// Reset UI state so it redraws from scratch after TUI
			buildDrawn = false
			return image
		}
	}
	return ""
}

// ── Detect db needs ───────────────────────────────────────────────────────────

func buildDetectDB(image string) map[string]bool {
	buildUpdate("Inspecting image for requirements...", 30)
	reqs := map[string]bool{"postgres": false, "mysql": false, "redis": false, "mongo": false}

	data := buildInspectImageJSON(image)
	if data == nil {
		// docker pull then re-inspect
		buildDockerPull(image)
		data = buildInspectImageJSON(image)
	}
	if len(data) == 0 {
		return reqs
	}
	cfg, _ := data[0]["Config"].(map[string]interface{})
	text := ""
	if cfg != nil {
		if env, ok := cfg["Env"].([]interface{}); ok {
			for _, e := range env {
				if s, ok := e.(string); ok {
					text += " " + s
				}
			}
		}
		if labels, ok := cfg["Labels"].(map[string]interface{}); ok {
			for _, lv := range labels {
				if s, ok := lv.(string); ok {
					text += " " + s
				}
			}
		}
	}
	if regexp.MustCompile(`(?i)POSTGRES|DATABASE_URL.*post|PGHOST`).MatchString(text) {
		reqs["postgres"] = true
	}
	if regexp.MustCompile(`(?i)MYSQL|MARIADB`).MatchString(text) {
		reqs["mysql"] = true
	}
	if regexp.MustCompile(`(?i)REDIS|REDIS_HOST`).MatchString(text) {
		reqs["redis"] = true
	}
	if regexp.MustCompile(`(?i)MONGO`).MatchString(text) {
		reqs["mongo"] = true
	}
	return reqs
}

// buildInspectImageJSON runs `docker inspect <image>` (10s timeout) returning the
// parsed JSON array, or nil on failure (mirrors subprocess + json.loads).
func buildInspectImageJSON(image string) []map[string]interface{} {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "inspect", image)
	cmd.Env = dockerEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var arr []map[string]interface{}
	if json.Unmarshal(out, &arr) != nil {
		return nil
	}
	return arr
}

func buildDockerPull(image string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "pull", image)
	cmd.Env = dockerEnv()
	cmd.Run()
}

// ── Find existing db containers ───────────────────────────────────────────────

// buildDBRec mirrors the per-db dict used throughout (find_existing/setup_db).
type buildDBRec struct {
	Type     string
	Name     string
	IP       string
	Port     string
	Stack    string
	Image    string
	Password string
	DBName   string
	Net      string
	New      bool
}

func buildFindExisting(dbType string) []buildDBRec {
	var found []buildDBRec
	pats := map[string]string{
		"postgres": `postgres`, "mysql": `mysql|mariadb`,
		"redis": `redis`, "mongo": `mongo`}
	patStr := pats[dbType]
	var pat *regexp.Regexp
	if patStr != "" {
		pat = regexp.MustCompile(`(?i)` + patStr)
	}

	files := buildListYML(stacksDir())
	sort.Strings(files)

	reServices := regexp.MustCompile(`^services:`)
	reTop := regexp.MustCompile(`^[a-zA-Z]`)
	reSvc := regexp.MustCompile(`^  ([a-zA-Z0-9_.\-]+):\s*$`)
	reImg := regexp.MustCompile(`\s+image:\s+(.+)`)
	reIPPort := regexp.MustCompile(`(\d+\.\d+\.\d+\.\d+):(\d+):\d+`)

	matchPat := func(s string) bool {
		// Python: re.search(pat, img, re.I). An empty pattern (unknown db_type)
		// matches everything in Python, so mirror that here.
		if pat == nil {
			return true
		}
		return pat.MatchString(s)
	}

	for _, f := range files {
		content, err := os.ReadFile(filepath.Join(stacksDir(), f))
		if err != nil {
			continue
		}
		inSvc := false
		cur, img, ip, port := "", "", "", ""
		for _, line := range strings.Split(string(content), "\n") {
			if reServices.MatchString(line) {
				inSvc = true
				continue
			}
			if reTop.MatchString(line) && !strings.HasPrefix(line, " ") {
				inSvc = false
			}
			if !inSvc {
				continue
			}
			if m := reSvc.FindStringSubmatch(line); m != nil {
				if cur != "" && matchPat(img) {
					found = append(found, buildDBRec{Name: cur, Image: img, IP: ip, Port: port, Stack: f})
				}
				cur = m[1]
				img, ip, port = "", "", ""
				continue
			}
			if cur != "" {
				if mi := reImg.FindStringSubmatch(line); mi != nil {
					img = strings.TrimSpace(mi[1])
				}
				if mp := reIPPort.FindStringSubmatch(line); mp != nil {
					ip = mp[1]
					port = mp[2]
				}
			}
		}
		if cur != "" && matchPat(img) {
			found = append(found, buildDBRec{Name: cur, Image: img, IP: ip, Port: port, Stack: f})
		}
	}
	return found
}

// buildListYML lists *.yml filenames (not full paths) in a dir.
func buildListYML(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yml") {
			out = append(out, e.Name())
		}
	}
	return out
}

func buildNextIP() string {
	used := map[int]bool{}
	re := regexp.MustCompile(`192\.168\.1\.(\d+)`)
	for _, f := range buildListYML(stacksDir()) {
		data, err := os.ReadFile(filepath.Join(stacksDir(), f))
		if err != nil {
			continue
		}
		for _, m := range re.FindAllStringSubmatch(string(data), -1) {
			if n, e := strconv.Atoi(m[1]); e == nil {
				used[n] = true
			}
		}
	}
	for i := 150; i < 254; i++ {
		if !used[i] {
			return fmt.Sprintf("192.168.1.%d", i)
		}
	}
	return "192.168.1.200"
}

func buildRandMac() string {
	return fmt.Sprintf("02:42:ac:%02x:%02x:%02x",
		rand.Intn(256), rand.Intn(256), rand.Intn(256))
}

// ── DB setup ──────────────────────────────────────────────────────────────────

func buildSetupDB(dbType, svcName string) *buildDBRec {
	buildUpdate(fmt.Sprintf("Setting up %s...", dbType), 55)
	existing := buildFindExisting(dbType)

	var dbStacks []string
	reDB := regexp.MustCompile(`db_\d+\.yml`)
	for _, f := range buildListYML(stacksDir()) {
		if reDB.MatchString(f) {
			dbStacks = append(dbStacks, f)
		}
	}
	sort.Strings(dbStacks)

	var choices []string
	for _, e := range existing {
		ipStr := "no IP"
		if e.IP != "" {
			ipStr = fmt.Sprintf("%s:%s", e.IP, e.Port)
		}
		choices = append(choices, fmt.Sprintf("USE  %s  (%s)  [%s]", e.Name, ipStr, e.Stack))
	}
	choices = append(choices, fmt.Sprintf("NEW  Create new %s container", dbType))

	choice := buildFzf(choices, fmt.Sprintf("Use existing %s or create new?", dbType), "")
	if choice == "" {
		return nil
	}
	if strings.HasPrefix(choice, "USE") {
		flds := strings.Fields(choice)
		if len(flds) < 2 {
			return nil
		}
		name := flds[1]
		var match *buildDBRec
		for i := range existing {
			if existing[i].Name == name {
				match = &existing[i]
				break
			}
		}
		if match == nil {
			return nil
		}
		return &buildDBRec{Type: dbType, Name: name, IP: match.IP,
			Port: match.Port, Stack: match.Stack,
			New: false, Net: svcName + "_net"}
	}

	sc := buildFzf(dbStacks, "Which db stack?", "")
	if sc == "" {
		sc = "db_0.yml"
	}
	defs := map[string][2]string{
		"postgres": {"postgres:16-alpine", "5432"},
		"mysql":    {"mariadb:10.11", "3306"},
		"redis":    {"redis:7-alpine", "6379"},
		"mongo":    {"mongo:7", "27017"},
	}
	def, ok := defs[dbType]
	if !ok {
		def = [2]string{"postgres:16-alpine", "5432"}
	}
	img, port := def[0], def[1]
	dbName := buildAsk("DB container name", fmt.Sprintf("%s-%s", svcName, dbType))
	dbIP := buildAsk("DB IP address", buildNextIP())
	dbPass := buildAsk("DB password", "bellzpass")
	dbDB := buildAsk("DB name", strings.ReplaceAll(svcName, "-", "_"))
	return &buildDBRec{Type: dbType, Name: dbName, IP: dbIP, Port: port,
		Image: img, Password: dbPass, DBName: dbDB,
		Stack: sc, New: true, Net: svcName + "_net"}
}

// ── Build YAML blocks ─────────────────────────────────────────────────────────

func buildSvc(name, image, ip, port string, cfg buildConf, svcNum int, db, redis *buildDBRec) string {
	net := name + "_net"
	mac := buildRandMac()
	nets := fmt.Sprintf("    networks:\n      traefik_net:\n        priority: 1000\n"+
		"      %s:\n        priority: 500", net)
	for _, xn := range cfg.ExtraNetworks {
		if m, ok := xn.(map[string]interface{}); ok {
			for nn, np := range m {
				nets += fmt.Sprintf("\n      %s:\n        priority: %v", nn, np)
			}
		}
	}
	var env []string
	for _, e := range cfg.ExtraEnv {
		env = append(env, fmt.Sprintf(`      - "%s"`, e))
	}
	if db != nil {
		dt, dip, dport := db.Type, db.IP, db.Port
		dpw := db.Password
		if dpw == "" {
			dpw = "pass"
		}
		ddb := db.DBName
		if ddb == "" {
			ddb = name
		}
		switch dt {
		case "postgres":
			env = append(env, fmt.Sprintf(`      - "DATABASE_URL=postgresql://postgres:%s@%s:%s/%s"`, dpw, dip, dport, ddb))
		case "mysql":
			env = append(env, fmt.Sprintf(`      - "DATABASE_URL=mysql://root:%s@%s:%s/%s"`, dpw, dip, dport, ddb))
		case "redis":
			env = append(env, fmt.Sprintf(`      - "REDIS_URL=redis://%s:%s/0"`, dip, dport))
		case "mongo":
			env = append(env, fmt.Sprintf(`      - "MONGODB_URI=mongodb://%s:%s/%s"`, dip, dport, ddb))
		}
	}
	if redis != nil {
		rip := redis.IP
		if rip == "" {
			rip = "127.0.0.1"
		}
		rpt := redis.Port
		if rpt == "" {
			rpt = "6379"
		}
		env = append(env, fmt.Sprintf(`      - "REDIS_URL=redis://%s:%s/0"`, rip, rpt))
	}
	envB := ""
	if len(env) > 0 {
		envB = "    environment:\n" + strings.Join(env, "\n") + "\n"
	}

	vols := []string{fmt.Sprintf(`      - "%s/docker/%s:/data"`, home(), name)}
	for _, v := range cfg.ExtraVolumes {
		vols = append(vols, fmt.Sprintf(`      - "%s"`, v))
	}

	sg := cfg.SablierGroup
	if sg == "" {
		sg = strings.ReplaceAll(strings.ReplaceAll(name, "-", ""), "_", "")
	}
	labels := []string{
		`      - "traefik.enable=true"`,
		`      - "sablier.enable=true"`,
		fmt.Sprintf(`      - "sablier.group=%s"`, sg),
	}
	for _, l := range cfg.ExtraLabels {
		labels = append(labels, fmt.Sprintf(`      - "%s"`, l))
	}

	useCaps := cfg.UseCommonCaps
	caps := ""
	if useCaps {
		caps = "    <<: *common-caps\n"
	}
	blkio := ""
	if cfg.Blkio {
		blkio = "    blkio_config: {weight: 500, device_read_bps: [{path: /dev/nvme0n1, rate: 300mb}], device_write_bps: [{path: /dev/nvme0n1, rate: 300mb}]}\n"
	}
	ulim := ""
	if cfg.Ulimits {
		ulim = "    ulimits: {memlock: {soft: -1, hard: -1}, nofile: {soft: 65535, hard: 65535}, nproc: 65535}\n"
	}
	dep := ""
	if cfg.DeployLimits {
		dep = "    deploy: {resources: {limits: {memory: 2G, cpus: '4.0', pids: 1000}, reservations: {memory: 256M, cpus: '0.5'}}}\n"
	}
	log := ""
	if !useCaps && cfg.Logging {
		log = "    logging: {driver: json-file, options: {max-size: 50m, max-file: '5'}}\n"
	}
	// Python: cfg.get("dns", [default]) — the default only applies when the key
	// is missing, NOT when it is an explicit empty list. load_conf always sets
	// the key, so we use cfg.DNS verbatim (empty list -> empty dns block).
	dnsList := cfg.DNS
	var dnsParts []string
	for _, d := range dnsList {
		dnsParts = append(dnsParts, fmt.Sprintf(`      - "%s"`, d))
	}
	dns := strings.Join(dnsParts, "\n")
	num := fmt.Sprintf("%02d", svcNum)
	hc := buildProbeHealthcheck(image, port)

	// The per-line settings block (only when not using common caps).
	settings := ""
	if !useCaps {
		settings = fmt.Sprintf("    cpuset: \"%s\"\n    cpu_shares: %d\n    stop_grace_period: %s\n    stop_signal: %s\n    restart: %s\n    user: \"%s\"\n",
			cfg.Cpuset, cfg.CPUShares, cfg.StopGracePeriod, cfg.StopSignal, cfg.Restart, cfg.User)
	}
	dnsBlock := ""
	if !useCaps {
		dnsBlock = "    dns:\n" + dns + "\n"
	}

	volsJoined := ""
	{
		var b []string
		for _, v := range vols {
			b = append(b, "  "+v)
		}
		volsJoined = strings.Join(b, "\n")
	}
	labelsJoined := ""
	{
		var b []string
		for _, l := range labels {
			b = append(b, "  "+l)
		}
		labelsJoined = strings.Join(b, "\n")
	}

	return fmt.Sprintf(`
  # ---------------------------------------------------------
  # %s. %s 🐳
  # Description: %s service — edit description here ✅
  # ---------------------------------------------------------
  %s:
%s    image: %s
    container_name: %s
    hostname: %s
    domainname: %s.loveiznothin.com
    mac_address: "%s"
%s%s
    ports:
      - "%s:%s:%s"
%s    volumes:
%s
    labels:
%s
%s%s%s%s%s%s`,
		num, strings.ToUpper(name),
		image,
		name,
		caps, image,
		name,
		name,
		name,
		mac,
		settings, nets,
		ip, port, port,
		envB,
		volsJoined,
		labelsJoined,
		dnsBlock, hc, blkio, ulim, dep, log)
}

func buildProbeHealthcheck(image, port string) string {
	// Inspect the IMAGE and pick a healthcheck that fits what's inside.
	probe := "command -v nc   >/dev/null 2>&1 && echo HAS_nc; " +
		"command -v wget >/dev/null 2>&1 && echo HAS_wget; " +
		"command -v curl >/dev/null 2>&1 && echo HAS_curl; echo SHELLOK"
	out := ""
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none",
		"--entrypoint", "sh", image, "-c", probe)
	env := os.Environ()
	if os.Getenv("DOCKER_HOST") == "" {
		env = append(env, "DOCKER_HOST=unix:///var/run/docker.sock")
	}
	cmd.Env = env
	// Python concatenates r.stdout + r.stderr; CombinedOutput captures both.
	combined, _ := cmd.CombinedOutput()
	out = string(combined)

	if !strings.Contains(out, "SHELLOK") {
		return "" // distroless / no shell
	}
	var test string
	switch {
	case strings.Contains(out, "HAS_nc"):
		test = fmt.Sprintf("nc -z 127.0.0.1 %s || exit 1", port)
	case strings.Contains(out, "HAS_wget"):
		test = fmt.Sprintf("wget -qO- http://127.0.0.1:%s/ || exit 1", port)
	case strings.Contains(out, "HAS_curl"):
		test = fmt.Sprintf("curl -sf http://127.0.0.1:%s/ || exit 1", port)
	default:
		return "" // shell but no probe tool
	}
	return "    healthcheck:\n" +
		fmt.Sprintf("      test: [\"CMD-SHELL\", \"%s\"]\n", test) +
		"      interval: 10s\n" +
		"      timeout: 5s\n" +
		"      retries: 10\n" +
		"      start_period: 30s\n"
}

func buildDBBlock(db *buildDBRec, svcName string) (string, string) {
	dt, name, ip, port := db.Type, db.Name, db.IP, db.Port
	img := db.Image
	if img == "" {
		img = "postgres:16-alpine"
	}
	pw := db.Password
	if pw == "" {
		pw = "bellzpass"
	}
	dbn := db.DBName
	if dbn == "" {
		dbn = strings.ReplaceAll(svcName, "-", "_")
	}
	net := svcName + "_net"
	mac := buildRandMac()
	envMap := map[string]string{
		"postgres": fmt.Sprintf("      - \"POSTGRES_PASSWORD=%s\"\n      - \"POSTGRES_DB=%s\"", pw, dbn),
		"mysql":    fmt.Sprintf("      - \"MYSQL_ROOT_PASSWORD=%s\"\n      - \"MARIADB_DATABASE=%s\"", pw, dbn),
		"redis":    `      - "REDIS_REPLICATION_MODE=master"`,
		"mongo":    fmt.Sprintf("      - \"MONGO_INITDB_DATABASE=%s\"", dbn),
	}
	vol := name + "-data"
	block := fmt.Sprintf(`
  # ---------------------------------------------------------
  # %s — %s for %s 🐳
  # ---------------------------------------------------------
  %s:
    image: %s
    container_name: %s
    hostname: %s
    mac_address: "%s"
    restart: "no"
    networks:
      %s:
        priority: 1000
    ports:
      - "%s:%s:%s"
    environment:
%s
    volumes:
      - "%s:/var/lib/postgresql/data"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres || exit 1"]
      interval: 5s
      timeout: 3s
      retries: 10
      start_period: 30s
`,
		strings.ToUpper(name), strings.ToUpper(dt), svcName,
		name,
		img,
		name,
		name,
		mac,
		net,
		ip, port, port,
		envMap[dt],
		vol)
	return block, vol
}

// ── Inject into stack file ────────────────────────────────────────────────────

func buildInject(stack, block, network, volume string) bool {
	fpath := filepath.Join(stacksDir(), stack)
	if !strings.HasSuffix(fpath, ".yml") {
		fpath += ".yml"
	}
	st, err := os.Stat(fpath)
	if err != nil || st.IsDir() {
		return false
	}
	data, err := os.ReadFile(fpath)
	if err != nil {
		return false
	}
	content := string(data)

	// Duplicate check
	reSvc := regexp.MustCompile(`(?m)^  ([a-zA-Z0-9_.\-]+):`)
	m := reSvc.FindStringSubmatch(block)
	if m != nil {
		dup := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(m[1]) + `:`)
		if dup.MatchString(content) {
			fmt.Printf("\n  \033[1;31m✘ %s already exists in %s\033[0m\n", m[1], stack)
			return false
		}
	}

	if network != "" && !strings.Contains(content, network) {
		re := regexp.MustCompile(`(?m)^(networks:\n)`)
		content = buildReplaceFirst(re, content,
			fmt.Sprintf("${1}  %s: {name: %s, external: true}\n", network, network))
	}
	if volume != "" && !strings.Contains(content, volume) {
		re := regexp.MustCompile(`(?m)^(volumes:\n)`)
		content = buildReplaceFirst(re, content,
			fmt.Sprintf("${1}  %s: {name: %s, external: true}\n", volume, volume))
	}

	if strings.Contains(content, "##BELLZART_START_FOOTER") {
		content = strings.Replace(content, "##BELLZART_START_FOOTER",
			strings.TrimRight(block, "\n")+"\n\n##BELLZART_START_FOOTER", 1)
	} else {
		// Find last top-level # line (footer art) and insert before it
		lines := buildSplitKeepEnds(content)
		insert := len(lines)
		for i := len(lines) - 1; i >= 0; i-- {
			if !strings.HasPrefix(lines[i], "#") && strings.TrimSpace(lines[i]) != "" {
				insert = i + 1
				break
			}
		}
		newLines := make([]string, 0, len(lines)+1)
		newLines = append(newLines, lines[:insert]...)
		newLines = append(newLines, strings.TrimRight(block, "\n")+"\n\n")
		newLines = append(newLines, lines[insert:]...)
		content = strings.Join(newLines, "")
	}
	// Tidy spacing so repeated builds never leave gaps: collapse any run of 2+
	// blank lines down to a single blank line, and end the file with exactly one
	// newline. (Double-blank lines are never meaningful in compose YAML.)
	content = regexp.MustCompile(`\n{3,}`).ReplaceAllString(content, "\n\n")
	content = strings.TrimRight(content, "\n") + "\n"
	return os.WriteFile(fpath, []byte(content), 0644) == nil
}

// buildReplaceFirst mirrors re.sub(..., count=1): replace only the first match,
// expanding ${1}-style group references in the replacement (count=1, MULTILINE).
func buildReplaceFirst(re *regexp.Regexp, s, repl string) string {
	loc := re.FindStringSubmatchIndex(s)
	if loc == nil {
		return s
	}
	expanded := re.ExpandString(nil, repl, s, loc)
	return s[:loc[0]] + string(expanded) + s[loc[1]:]
}

// buildSplitKeepEnds mirrors str.splitlines(keepends=True).
func buildSplitKeepEnds(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// ── Main ──────────────────────────────────────────────────────────────────────

func cmdBuild(argv []string) {
	// New default: the centered in-a-box wizard (matches the Python run_build_wizard
	// look — everything inside one small box, no full-screen takeover). The old
	// bottom-panel + full-screen-search flow is kept reachable with `--classic`.
	classic := false
	for _, a := range argv {
		if a == "--classic" {
			classic = true
		}
	}
	if !classic {
		cmdBuildBox(argv)
		return
	}

	// args = [a for a in sys.argv[1:] if a not in ('--progress',) and not startswith('/tmp/')]
	var args []string
	for _, a := range argv {
		if a == "--progress" || strings.HasPrefix(a, "/tmp/") {
			continue
		}
		args = append(args, a)
	}
	var log []string
	cfg := buildLoadConf()

	image, svcName, targetStack := "", "", ""
	if len(args) >= 3 {
		image = args[0]
		svcName = args[1]
		targetStack = args[2]
	} else if len(args) == 2 {
		svcName = args[0]
		targetStack = args[1]
	} else if len(args) == 1 {
		svcName = args[0]
	}

	uiTarget := targetStack
	if uiTarget == "" {
		uiTarget = "build"
	}
	uiSvc := svcName
	if uiSvc == "" {
		uiSvc = "service"
	}
	buildInitUI(uiTarget, uiSvc)

	// ── 1. Stack selection ─────────────────────────────────────────────────
	if targetStack == "" {
		buildUpdate("Select target stack...", 5)
		var stacks []string
		for _, f := range buildListYML(stacksDir()) {
			if !strings.HasPrefix(f, "db_") {
				stacks = append(stacks, strings.TrimSuffix(f, ".yml"))
			}
		}
		sort.Strings(stacks)
		targetStack = buildFzf(stacks, "Which stack to add service to?", "")
		if targetStack == "" {
			buildClearUI()
			fmt.Println("  \033[1;31m✘ Cancelled.\033[0m")
			os.Exit(1)
		}
		buildStTarget = targetStack
	}

	// ── 2. Image search ────────────────────────────────────────────────────
	if image == "" {
		buildUpdate(fmt.Sprintf("Searching Docker Hub for %s...", svcName), 10)
		image = buildHubSearch(svcName)
		if image == "" {
			buildClearUI()
			fmt.Println("  \033[1;31m✘ Cancelled.\033[0m")
			os.Exit(1)
		}
		log = append(log, "Image: "+image)
	}

	buildUpdate("Image: "+image, 20)

	// ── 3. Service details ─────────────────────────────────────────────────
	buildUpdate("Getting service details...", 25)
	svcIP := buildAsk("Service IP (192.168.1.x)", buildNextIP())
	svcPort := buildAsk("Service port", "8080")
	svcNameIn := buildAsk("Container name", svcName)
	if svcNameIn != "" {
		svcName = svcNameIn
	}
	log = append(log, fmt.Sprintf("Name: %s  IP: %s", svcName, svcIP))

	// ── 4. Detect & configure database ────────────────────────────────────
	buildUpdate("Inspecting image...", 30)
	reqs := buildDetectDB(image)
	var detected []string
	// preserve insertion order (postgres,mysql,redis,mongo)
	for _, k := range []string{"postgres", "mysql", "redis", "mongo"} {
		if reqs[k] {
			detected = append(detected, k)
		}
	}
	if len(detected) > 0 {
		buildUpdate("Detected: "+strings.Join(detected, ", "), 35)
	}

	var dbInfo *buildDBRec
	var redisInfo *buildDBRec
	for _, dt := range []string{"postgres", "mysql", "redis", "mongo"} {
		if !reqs[dt] {
			continue
		}
		// Check if a db for this service already exists
		existing := buildFindExisting(dt)
		svcKey := strings.ReplaceAll(strings.ReplaceAll(svcName, "-", ""), "_", "")
		var already []buildDBRec
		for _, e := range existing {
			eKey := strings.ReplaceAll(strings.ReplaceAll(e.Name, "-", ""), "_", "")
			if strings.Contains(eKey, svcKey) {
				already = append(already, e)
			}
		}
		if len(already) > 0 {
			e := already[0]
			fmt.Printf("  \033[1;32m✔ Found existing %s: %s (%s:%s)\033[0m\n", dt, e.Name, e.IP, e.Port)
			info := &buildDBRec{Type: dt, Name: e.Name, IP: e.IP,
				Port: e.Port, Stack: e.Stack, New: false,
				Net: svcName + "_net"}
			if dt == "redis" {
				redisInfo = info
			} else {
				dbInfo = info
			}
		} else {
			fmt.Printf("  \033[1;33m⚠ No existing %s found for %s\033[0m\n", dt, svcName)
			yn := buildAsk(fmt.Sprintf("Add a new %s database? (y/n)", dt), "y")
			if strings.ToLower(yn) == "y" {
				var dbStacks []string
				reDB := regexp.MustCompile(`db_\d+\.yml`)
				for _, f := range buildListYML(stacksDir()) {
					if reDB.MatchString(f) {
						dbStacks = append(dbStacks, strings.TrimSuffix(f, ".yml"))
					}
				}
				sort.Strings(dbStacks)
				buildUpdate(fmt.Sprintf("Select db stack for %s...", dt), 50)
				dbTarget := buildFzf(dbStacks, fmt.Sprintf("Which db stack to add %s to?", dt), "")
				if dbTarget != "" {
					info := buildSetupDB(dt, svcName)
					if info != nil {
						info.Stack = dbTarget + ".yml"
					}
					if dt == "redis" {
						redisInfo = info
					} else {
						dbInfo = info
					}
				}
			}
		}
	}

	// ── 5. Manual db prompt ────────────────────────────────────────────────
	if dbInfo == nil {
		yn := buildAsk("Does this service need a database? (y/n)", "n")
		if strings.ToLower(yn) == "y" {
			dt := buildFzf([]string{"postgres", "mysql", "redis", "mongo", "none"}, "Database type?", "")
			if dt != "" && dt != "none" {
				dbInfo = buildSetupDB(dt, svcName)
			}
		}
	}

	// ── 6. Redis ───────────────────────────────────────────────────────────
	if redisInfo == nil {
		yn := buildAsk("Does this service need Redis? (y/n)", "n")
		if strings.ToLower(yn) == "y" {
			redisInfo = buildSetupDB("redis", svcName)
		}
	}

	// ── 6b. Companion container ───────────────────────────────────────────
	var companionInfo *struct {
		Name, Image, Stack, Desc string
	}
	yn := buildAsk("Does this service need a companion container? (y/n)", "n")
	if strings.ToLower(yn) == "y" {
		buildUpdate("Search for companion image...", 50)
		compImg := buildHubSearch(svcName)
		if compImg != "" {
			compName := buildAsk("Companion container name", svcName+"-worker")
			var compStacks []string
			for _, f := range buildListYML(stacksDir()) {
				if !strings.HasPrefix(f, "db_") {
					compStacks = append(compStacks, strings.TrimSuffix(f, ".yml"))
				}
			}
			sort.Strings(compStacks)
			buildUpdate("Select stack for companion...", 55)
			compStack := buildFzf(compStacks, fmt.Sprintf("Which stack for %s?", compName), "")
			if compStack != "" {
				companionInfo = &struct {
					Name, Image, Stack, Desc string
				}{Name: compName, Image: compImg, Stack: compStack, Desc: "companion service"}
			}
		}
	}

	// ── 6c. Network / volume (netvol step — mirrors the Python wizard) ──────
	buildUpdate("Network & volume...", 65)
	autoNetwork, autoVolume, externalNet := true, true, true
	nv := buildAsk("Auto-create network & volume for this container? (y/n)", "y")
	if strings.ToLower(nv) == "y" {
		nt := buildFzf([]string{
			"External (stored in creator/core file)",
			"Internal (stored in this compose file)",
		}, "Network/Volume type?", "")
		if nt != "" {
			externalNet = strings.Contains(nt, "External")
		}
		log = append(log, fmt.Sprintf("Net/Vol: auto (%s)",
			map[bool]string{true: "external", false: "internal"}[externalNet]))
	} else {
		autoNetwork, autoVolume = false, false
		log = append(log, "Net/Vol: skipped (user)")
	}
	_ = externalNet

	// ── 7. Build scaffold ──────────────────────────────────────────────────
	buildUpdate("Building compose scaffold...", 70)
	var fpath string
	if strings.HasSuffix(targetStack, ".yml") {
		fpath = filepath.Join(stacksDir(), targetStack)
	} else {
		fpath = filepath.Join(stacksDir(), targetStack+".yml")
	}
	svcCount := 0
	if data, err := os.ReadFile(fpath); err == nil {
		inServices := false
		reServices := regexp.MustCompile(`^services:`)
		reTop := regexp.MustCompile(`^[a-zA-Z]`)
		reSvcLine := regexp.MustCompile(`^  [a-zA-Z0-9][a-zA-Z0-9_.\-]+:\s*$`)
		for _, line := range strings.Split(string(data), "\n") {
			if reServices.MatchString(line) {
				inServices = true
				continue
			}
			if reTop.MatchString(line) && !strings.HasPrefix(line, " ") {
				inServices = false
				continue
			}
			if !inServices {
				continue
			}
			if reSvcLine.MatchString(line) && !strings.HasPrefix(strings.TrimSpace(line), "x-") {
				svcCount++
			}
		}
	}
	svcNum := svcCount + 1
	svcNet := svcName + "_net"
	svcBlock := buildSvc(svcName, image, svcIP, svcPort, cfg, svcNum, dbInfo, redisInfo)

	// ── 8. Inject ──────────────────────────────────────────────────────────
	buildUpdate(fmt.Sprintf("Injecting into %s...", targetStack), 80)
	if buildInject(targetStack, svcBlock, svcNet, "") {
		log = append(log, fmt.Sprintf("✔ Added #%d %s to %s", svcNum, svcName, targetStack))
	}

	if dbInfo != nil && dbInfo.New {
		buildUpdate(fmt.Sprintf("Adding DB to %s...", dbInfo.Stack), 85)
		dblk, dvol := buildDBBlock(dbInfo, svcName)
		if buildInject(dbInfo.Stack, dblk, svcNet, dvol) {
			log = append(log, fmt.Sprintf("✔ DB %s → %s", dbInfo.Name, dbInfo.Stack))
		}
		if autoVolume {
			exec.Command("docker", "volume", "create", dvol).Run()
		}
	}

	if redisInfo != nil && redisInfo.New {
		buildUpdate(fmt.Sprintf("Adding Redis to %s...", redisInfo.Stack), 87)
		rblk, rvol := buildDBBlock(redisInfo, svcName)
		if buildInject(redisInfo.Stack, rblk, svcNet, rvol) {
			log = append(log, fmt.Sprintf("✔ Redis → %s", redisInfo.Stack))
		}
	}

	if companionInfo != nil {
		buildUpdate(fmt.Sprintf("Adding companion %s...", companionInfo.Name), 89)
		compBlock := buildSvc(companionInfo.Name, companionInfo.Image,
			buildNextIP(), svcPort, cfg, svcNum+1, nil, nil)
		if buildInject(companionInfo.Stack, compBlock, svcNet, "") {
			log = append(log, fmt.Sprintf("✔ Companion %s → %s", companionInfo.Name, companionInfo.Stack))
		}
	}

	// ── 9. Network ─────────────────────────────────────────────────────────
	if autoNetwork {
		buildUpdate(fmt.Sprintf("Creating network %s...", svcNet), 92)
		if exec.Command("docker", "network", "inspect", svcNet).Run() != nil {
			exec.Command("docker", "network", "create", svcNet).Run()
			log = append(log, "Network: "+svcNet)
		}
	}

	buildUpdate("Build complete! ✨", 100)
	time.Sleep(300 * time.Millisecond)

	// Write log into the central logs folder (logDir = <data>/logs by default).
	lpath := logPath(fmt.Sprintf("stacks_build_%s.log", svcName))
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("=== Build: %s ===\n", svcName))
	for _, l := range log {
		sb.WriteString(l + "\n")
	}
	os.WriteFile(lpath, []byte(sb.String()), 0644)

	// ── 10. Ask to start ───────────────────────────────────────────────────
	// Final questions — reset terminal state first
	fmt.Fprint(os.Stdout, "\033[0m\n")
	os.Stdout.Sync()
	buildUpdate(fmt.Sprintf("✨ %s built successfully!", svcName), 100)
	yn2start := buildAsk(fmt.Sprintf("Start %s now? (y/n)", svcName), "n")
	if strings.ToLower(yn2start) == "y" {
		mode := buildAsk("(s)ervice only or (w)hole stack?", "s")
		if strings.HasPrefix(strings.ToLower(mode), "w") {
			fmt.Printf("\n\033[1;32m✨ BUILD COMPLETE: %s\033[0m\n", svcName)
			fmt.Printf("BUILD_OK:%s\n", svcName)
			fmt.Printf("BUILD_START:%s\n", targetStack)
		} else {
			fmt.Printf("\n\033[1;32m✨ BUILD COMPLETE: %s\033[0m\n", svcName)
			fmt.Printf("BUILD_OK:%s\n", svcName)
			fmt.Printf("BUILD_START:%s %s\n", targetStack, svcName)
		}
	} else {
		yn3 := buildAsk(fmt.Sprintf("Start whole stack %s? (y/n)", targetStack), "n")
		fmt.Printf("\n\033[1;32m✨ BUILD COMPLETE: %s\033[0m\n", svcName)
		if strings.ToLower(yn3) == "y" {
			fmt.Printf("BUILD_OK:%s\n", svcName)
			fmt.Printf("BUILD_START:%s\n", targetStack)
		} else {
			fmt.Printf("BUILD_OK:%s\n", svcName)
		}
	}
}

// ===== from search.go =====

// search.go — faithful Go port of stacks_search.py.
//
// "stacks regsearch" — a half-screen registry-search TUI.
//   * Concurrently queries ~12 container registries / package indexes.
//   * Enter = `docker pull` the selected image directly (or, with --select,
//     write the chosen image to /tmp/stacks_build_selected and exit).
//
// The Python uses the `curses` library; there is no curses dependency in this
// Go project, so the terminal UI is reproduced with raw ANSI escape sequences
// plus raw-mode keyboard input via golang.org/x/sys/unix (already a dep).
// Behaviour (keys, layout, colors, flow) is kept as close to the original as
// the medium allows.

// ── constants (UA / TIMEOUT / PAGE_SIZE_PER_REGISTRY) ───────────────────────

const searchUA = "Mozilla/5.0 (regsearch/2.0)"
const searchTimeout = 15 * time.Second
const searchPageSizePerRegistry = 100

// searchResult mirrors the dict the Python builds for each repository.
// "_error" is carried as a distinct field rather than a magic dict key.
type searchResult struct {
	Name      string
	Namespace string
	Registry  string
	Pull      string
	Stars     interface{} // may be int/float/string/nil (kept loose like Python)
	Pulls     interface{}
	Desc      string
	URL       string
	Official  bool
	Source    string // "_source": which registry produced this row
	Err       string // "_error": non-empty means this is an error entry
}

// ── http_get_json ───────────────────────────────────────────────────────────

// searchHTTPGetJSON mirrors http_get_json(url, headers, timeout).
// Returns a parsed JSON value (map / slice / scalar) and a non-empty error
// string on failure (the Python returns {"_error": ...}).
func searchHTTPGetJSON(rawurl string, headers map[string]string, timeout time.Duration) (interface{}, string) {
	if timeout == 0 {
		timeout = searchTimeout
	}
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		return nil, err.Error()
	}
	req.Header.Set("User-Agent", searchUA)
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	cl := &http.Client{Timeout: timeout}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err.Error()
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err.Error()
	}
	if strings.TrimSpace(string(body)) == "" {
		return nil, "empty response"
	}
	var out interface{}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Sprintf("invalid JSON: %v", err)
	}
	return out, ""
}

// ── tiny JSON accessors (Python dict .get equivalents) ──────────────────────

func searchAsMap(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return nil
}

func searchAsSlice(v interface{}) []interface{} {
	if s, ok := v.([]interface{}); ok {
		return s
	}
	return nil
}

// searchGetStr does dict.get(key, "") then strips? (no strip unless asked).
func searchGetStr(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// searchGetStrStrip does (dict.get(key) or "").strip().
func searchGetStrStrip(m map[string]interface{}, key string) string {
	return strings.TrimSpace(searchGetStr(m, key))
}

// searchGetAny returns the raw value for a key (for stars/pulls which the
// Python leaves untyped). Returns nil when absent.
func searchGetAny(m map[string]interface{}, key string) interface{} {
	if m == nil {
		return nil
	}
	if v, ok := m[key]; ok {
		return v
	}
	return nil
}

// searchGetNested does dict.get(outer, {}).get(inner, "").
func searchGetNested(m map[string]interface{}, outer, inner string) string {
	return searchGetStr(searchAsMap(searchGetAny(m, outer)), inner)
}

func searchErrResult(msg string) searchResult { return searchResult{Err: msg} }

// ── per-registry searchers ──────────────────────────────────────────────────

func searchDockerHub(keyword string, page, limit int) []searchResult {
	out := []searchResult{}
	seen := map[string]bool{}
	for p := 1; p <= 3; p++ {
		u := fmt.Sprintf("https://hub.docker.com/v2/search/repositories/?query=%s&page_size=100&page=%d",
			url.QueryEscape(keyword), p)
		data, errs := searchHTTPGetJSON(u, nil, searchTimeout)
		if errs != "" {
			break
		}
		m := searchAsMap(data)
		results := searchAsSlice(searchGetAny(m, "results"))
		if len(results) == 0 {
			break
		}
		for _, ri := range results {
			r := searchAsMap(ri)
			ns := searchGetStr(r, "repo_owner")
			if ns == "" {
				ns = "library"
			}
			name := searchGetStr(r, "repo_name")
			key := ns + "/" + name
			if seen[key] {
				continue
			}
			seen[key] = true
			pull := name
			if ns != "library" {
				pull = ns + "/" + name
			}
			out = append(out, searchResult{
				Name: name, Namespace: ns, Registry: "docker.io",
				Pull:  pull,
				Stars: searchGetAny(r, "star_count"), Pulls: searchGetAny(r, "pull_count"),
				Desc: searchGetStrStrip(r, "short_description"),
				URL:  fmt.Sprintf("https://hub.docker.com/r/%s/%s", ns, name),
			})
		}
		if len(results) < 100 {
			break // last page
		}
	}
	return out
}

func searchGhcr(keyword string, page, limit int) []searchResult {
	u := fmt.Sprintf("https://api.github.com/search/repositories?q=%s&per_page=%d&page=%d&sort=stars",
		url.QueryEscape(keyword), limit, page)
	data, errs := searchHTTPGetJSON(u, map[string]string{
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
	}, searchTimeout)
	if errs != "" {
		return []searchResult{searchErrResult(errs)}
	}
	m := searchAsMap(data)
	out := []searchResult{}
	for _, ri := range searchAsSlice(searchGetAny(m, "items")) {
		r := searchAsMap(ri)
		owner := searchGetNested(r, "owner", "login")
		name := searchGetStr(r, "name")
		if owner == "" || name == "" {
			continue
		}
		out = append(out, searchResult{
			Name: name, Namespace: owner, Registry: "ghcr.io",
			Pull:  strings.ToLower(fmt.Sprintf("ghcr.io/%s/%s", owner, name)),
			Stars: searchGetAny(r, "stargazers_count"),
			Pulls: searchGetAny(r, "watchers_count"),
			Desc:  searchGetStrStrip(r, "description"),
			URL:   searchGetStr(r, "html_url"),
		})
	}
	return out
}

func searchQuay(keyword string, page, limit int) []searchResult {
	u := fmt.Sprintf("https://quay.io/api/v1/find/repositories?query=%s&page=%d&includeUsage=true",
		url.QueryEscape(keyword), page)
	data, errs := searchHTTPGetJSON(u, nil, searchTimeout)
	if errs != "" {
		return []searchResult{searchErrResult(errs)}
	}
	m := searchAsMap(data)
	out := []searchResult{}
	results := searchAsSlice(searchGetAny(m, "results"))
	if len(results) > limit {
		results = results[:limit]
	}
	for _, ri := range results {
		r := searchAsMap(ri)
		var nsName string
		nsVal := searchGetAny(r, "namespace")
		if nsMap := searchAsMap(nsVal); nsMap != nil {
			nsName = searchGetStr(nsMap, "name")
		} else if s, ok := nsVal.(string); ok {
			nsName = s
		}
		name := searchGetStr(r, "name")
		pull := name
		urlStr := ""
		if nsName != "" {
			pull = fmt.Sprintf("quay.io/%s/%s", nsName, name)
			urlStr = fmt.Sprintf("https://quay.io/repository/%s/%s", nsName, name)
		}
		out = append(out, searchResult{
			Name: name, Namespace: nsName, Registry: "quay.io",
			Pull:  pull,
			Stars: searchGetAny(r, "popularity"), Pulls: searchGetAny(r, "usage_count"),
			Desc: searchGetStrStrip(r, "description"),
			URL:  urlStr,
		})
	}
	return out
}

func searchLscr(keyword string, page, limit int) []searchResult {
	u := fmt.Sprintf("https://hub.docker.com/v2/repositories/linuxserver/?page_size=100&page=%d", page)
	data, errs := searchHTTPGetJSON(u, nil, searchTimeout)
	if errs != "" {
		return []searchResult{searchErrResult(errs)}
	}
	m := searchAsMap(data)
	kw := strings.ToLower(keyword)
	out := []searchResult{}
	for _, ri := range searchAsSlice(searchGetAny(m, "results")) {
		r := searchAsMap(ri)
		name := searchGetStr(r, "name")
		desc := searchGetStr(r, "description")
		if kw != "" && !strings.Contains(strings.ToLower(name), kw) &&
			!strings.Contains(strings.ToLower(desc), kw) {
			continue
		}
		out = append(out, searchResult{
			Name: name, Namespace: "linuxserver", Registry: "lscr.io",
			Pull:  fmt.Sprintf("lscr.io/linuxserver/%s", name),
			Stars: searchGetAny(r, "star_count"), Pulls: searchGetAny(r, "pull_count"),
			Desc: strings.TrimSpace(desc),
			URL:  fmt.Sprintf("https://hub.docker.com/r/linuxserver/%s", name),
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

func searchBitnami(keyword string, page, limit int) []searchResult {
	u := fmt.Sprintf("https://hub.docker.com/v2/repositories/bitnami/?page_size=%d&page=%d&name=%s",
		limit, page, url.QueryEscape(keyword))
	data, errs := searchHTTPGetJSON(u, nil, searchTimeout)
	if errs != "" {
		return []searchResult{searchErrResult(errs)}
	}
	m := searchAsMap(data)
	out := []searchResult{}
	results := searchAsSlice(searchGetAny(m, "results"))
	if len(results) > limit {
		results = results[:limit]
	}
	for _, ri := range results {
		r := searchAsMap(ri)
		name := searchGetStr(r, "name")
		out = append(out, searchResult{
			Name: name, Namespace: "bitnami", Registry: "docker.io/bitnami",
			Pull:  fmt.Sprintf("bitnami/%s", name),
			Stars: searchGetAny(r, "star_count"), Pulls: searchGetAny(r, "pull_count"),
			Desc: searchGetStrStrip(r, "description"),
			URL:  fmt.Sprintf("https://hub.docker.com/r/bitnami/%s", name),
		})
	}
	return out
}

func searchGitlab(keyword string, page, limit int) []searchResult {
	u := fmt.Sprintf("https://gitlab.com/api/v4/projects?search=%s&per_page=%d&page=%d&order_by=star_count&sort=desc&with_packages=true",
		url.QueryEscape(keyword), limit, page)
	data, errs := searchHTTPGetJSON(u, map[string]string{"Accept": "application/json"}, searchTimeout)
	if errs != "" {
		return []searchResult{searchErrResult(errs)}
	}
	arr := searchAsSlice(data)
	if arr == nil { // not a list -> []
		return []searchResult{}
	}
	if len(arr) > limit {
		arr = arr[:limit]
	}
	out := []searchResult{}
	for _, ri := range arr {
		r := searchAsMap(ri)
		path := searchGetStr(r, "path_with_namespace")
		if path == "" {
			continue
		}
		out = append(out, searchResult{
			Name: searchGetStr(r, "path"), Namespace: searchGetNested(r, "namespace", "path"),
			Registry: "registry.gitlab.com",
			Pull:     fmt.Sprintf("registry.gitlab.com/%s", path),
			Stars:    searchGetAny(r, "star_count"),
			Desc:     searchGetStrStrip(r, "description"),
			URL:      searchGetStr(r, "web_url"),
		})
	}
	return out
}

func searchMcr(keyword string, page, limit int) []searchResult {
	data, errs := searchHTTPGetJSON("https://mcr.microsoft.com/v2/_catalog?n=10000", nil, searchTimeout)
	if errs != "" {
		return []searchResult{searchErrResult(errs)}
	}
	m := searchAsMap(data)
	kw := strings.ToLower(keyword)
	matches := []string{}
	for _, ri := range searchAsSlice(searchGetAny(m, "repositories")) {
		if s, ok := ri.(string); ok {
			if strings.Contains(strings.ToLower(s), kw) {
				matches = append(matches, s)
			}
		}
	}
	start := (page - 1) * limit
	out := []searchResult{}
	if start < 0 {
		start = 0
	}
	for i := start; i < start+limit && i < len(matches); i++ {
		repo := matches[i]
		parts := strings.Split(repo, "/")
		name := parts[len(parts)-1]
		ns := strings.Join(parts[:len(parts)-1], "/")
		out = append(out, searchResult{
			Name:      name,
			Namespace: ns,
			Registry:  "mcr.microsoft.com",
			Pull:      fmt.Sprintf("mcr.microsoft.com/%s", repo),
			Desc:      "Microsoft Container Registry image",
		})
	}
	return out
}

func searchArtifacthub(keyword string, page, limit int) []searchResult {
	offset := (page - 1) * limit
	// kind=12 = container images only (not helm charts)
	u := fmt.Sprintf("https://artifacthub.io/api/v1/packages/search?ts_query_web=%s&limit=%d&offset=%d&kind=12",
		url.QueryEscape(keyword), limit, offset)
	data, errs := searchHTTPGetJSON(u, nil, searchTimeout)
	if errs != "" {
		return []searchResult{searchErrResult(errs)}
	}
	m := searchAsMap(data)
	out := []searchResult{}
	pkgs := searchAsSlice(searchGetAny(m, "packages"))
	if len(pkgs) > limit {
		pkgs = pkgs[:limit]
	}
	for _, ri := range pkgs {
		r := searchAsMap(ri)
		repo := searchAsMap(searchGetAny(r, "repository"))
		kind := searchGetStr(repo, "kind_name")
		if kind == "" {
			kind = "helm"
		}
		name := searchGetStr(r, "name")
		repoName := searchGetStr(repo, "name")
		pull := name
		if repoName != "" {
			pull = fmt.Sprintf("%s/%s", repoName, name)
		}
		out = append(out, searchResult{
			Name: name, Namespace: repoName,
			Registry: fmt.Sprintf("artifacthub:%s", kind),
			Pull:     pull,
			Stars:    searchGetAny(r, "stars"),
			Desc:     searchGetStrStrip(r, "description"),
			URL:      fmt.Sprintf("https://artifacthub.io/packages/%s/%s/%s", kind, repoName, name),
		})
	}
	return out
}

// searchAwsEcr — Docker Hub verified publishers (replaces broken ECR API).
func searchAwsEcr(keyword string, page, limit int) []searchResult {
	u := fmt.Sprintf("https://hub.docker.com/v2/search/repositories/?query=%s&page_size=%d&page=%d&content_types=image&image_filter=store",
		url.QueryEscape(keyword), limit, page)
	data, errs := searchHTTPGetJSON(u, nil, searchTimeout)
	if errs != "" {
		return []searchResult{}
	}
	m := searchAsMap(data)
	out := []searchResult{}
	results := searchAsSlice(searchGetAny(m, "results"))
	if len(results) > limit {
		results = results[:limit]
	}
	for _, ri := range results {
		r := searchAsMap(ri)
		ns := searchGetStr(r, "repo_owner")
		if ns == "" {
			ns = "library"
		}
		name := searchGetStr(r, "repo_name")
		pull := name
		if ns != "library" {
			pull = ns + "/" + name
		}
		out = append(out, searchResult{
			Name: name, Namespace: ns, Registry: "docker.io (verified)",
			Pull:  pull,
			Stars: searchGetAny(r, "star_count"), Pulls: searchGetAny(r, "pull_count"),
			Desc: searchGetStrStrip(r, "short_description"),
		})
	}
	return out
}

// searchForgejo — Codeberg/Forgejo container registry.
func searchForgejo(keyword string, page, limit int) []searchResult {
	u := fmt.Sprintf("https://codeberg.org/api/v1/repos/search?q=%s&limit=%d&page=%d&topic=true",
		url.QueryEscape(keyword), limit, page)
	data, errs := searchHTTPGetJSON(u, map[string]string{"Accept": "application/json"}, searchTimeout)
	if errs != "" {
		return []searchResult{}
	}
	m := searchAsMap(data)
	out := []searchResult{}
	items := searchAsSlice(searchGetAny(m, "data"))
	if len(items) > limit {
		items = items[:limit]
	}
	for _, ri := range items {
		r := searchAsMap(ri)
		owner := searchGetNested(r, "owner", "login")
		name := searchGetStr(r, "name")
		if owner == "" {
			continue
		}
		out = append(out, searchResult{
			Name: name, Namespace: owner, Registry: "codeberg.org",
			Pull:  strings.ToLower(fmt.Sprintf("codeberg.org/%s/%s", owner, name)),
			Stars: searchGetAny(r, "stars_count"),
			Desc:  searchGetStrStrip(r, "description"),
		})
	}
	return out
}

// searchDockerhubOfficial — Docker Hub official images.
func searchDockerhubOfficial(keyword string, page, limit int) []searchResult {
	u := fmt.Sprintf("https://hub.docker.com/v2/search/repositories/?query=%s&page_size=50&page=%d&is_official=1",
		url.QueryEscape(keyword), page)
	data, errs := searchHTTPGetJSON(u, nil, searchTimeout)
	if errs != "" {
		return []searchResult{}
	}
	m := searchAsMap(data)
	out := []searchResult{}
	for _, ri := range searchAsSlice(searchGetAny(m, "results")) {
		r := searchAsMap(ri)
		ns := searchGetStr(r, "repo_owner")
		if ns == "" {
			ns = "library"
		}
		name := searchGetStr(r, "repo_name")
		pull := name
		if ns != "library" {
			pull = fmt.Sprintf("%s/%s", ns, name)
		}
		out = append(out, searchResult{
			Name: name, Namespace: ns, Registry: "docker.io (official)",
			Pull:  pull,
			Stars: searchGetAny(r, "star_count"), Pulls: searchGetAny(r, "pull_count"),
			Desc: searchGetStrStrip(r, "short_description"),
		})
	}
	return out
}

// searchSelfhostedIo — awesome-selfhosted via GitHub.
func searchSelfhostedIo(keyword string, page, limit int) []searchResult {
	u := fmt.Sprintf("https://api.github.com/search/repositories?q=%s+topic:self-hosted&per_page=%d&page=%d&sort=stars",
		url.QueryEscape(keyword), limit, page)
	data, errs := searchHTTPGetJSON(u, map[string]string{"Accept": "application/vnd.github+json"}, searchTimeout)
	if errs != "" {
		return []searchResult{}
	}
	m := searchAsMap(data)
	out := []searchResult{}
	for _, ri := range searchAsSlice(searchGetAny(m, "items")) {
		r := searchAsMap(ri)
		owner := searchGetNested(r, "owner", "login")
		name := searchGetStr(r, "name")
		if owner == "" {
			continue
		}
		out = append(out, searchResult{
			Name: name, Namespace: owner, Registry: "ghcr.io",
			Pull:  strings.ToLower(fmt.Sprintf("ghcr.io/%s/%s", owner, name)),
			Stars: searchGetAny(r, "stargazers_count"),
			Desc:  searchGetStrStrip(r, "description"),
		})
	}
	return out
}

// searchPortainerTemplates — Portainer community templates.
func searchPortainerTemplates(keyword string, page, limit int) []searchResult {
	u := "https://raw.githubusercontent.com/portainer/templates/master/templates-2.0.json"
	data, errs := searchHTTPGetJSON(u, nil, searchTimeout)
	if errs != "" {
		return []searchResult{}
	}
	m := searchAsMap(data)
	out := []searchResult{}
	kw := strings.ToLower(keyword)
	for _, ti := range searchAsSlice(searchGetAny(m, "templates")) {
		t := searchAsMap(ti)
		name := strings.ToLower(searchGetStr(t, "name"))
		title := strings.ToLower(searchGetStr(t, "title"))
		if strings.Contains(name, kw) || strings.Contains(title, kw) {
			image := searchGetStr(t, "image")
			out = append(out, searchResult{
				Name: searchGetStr(t, "name"), Namespace: "",
				Registry: "portainer-templates",
				Pull:     image,
				Desc:     searchGetStrStrip(t, "description"),
			})
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ── REGISTRIES (ordered like the Python dict) ───────────────────────────────

type searchRegistry struct {
	name string
	fn   func(keyword string, page, limit int) []searchResult
}

var searchRegistries = []searchRegistry{
	{"Docker Hub", searchDockerHub},
	{"Hub Official", searchDockerhubOfficial},
	{"GitHub (ghcr.io)", searchGhcr},
	{"Self-Hosted", searchSelfhostedIo},
	{"Quay.io", searchQuay},
	{"GitLab Registry", searchGitlab},
	{"Verified Pub", searchAwsEcr},
	{"Codeberg", searchForgejo},
	{"LinuxServer.io", searchLscr},
	{"Bitnami", searchBitnami},
	{"Microsoft MCR", searchMcr},
	{"ArtifactHub", searchArtifacthub},
}

// searchAll mirrors search_all(): query every registry concurrently.
func searchAll(keyword string, page, limitPerReg int) map[string][]searchResult {
	results := map[string][]searchResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, reg := range searchRegistries {
		wg.Add(1)
		go func(reg searchRegistry) {
			defer wg.Done()
			var res []searchResult
			func() {
				defer func() {
					if r := recover(); r != nil {
						res = []searchResult{searchErrResult(fmt.Sprintf("%v", r))}
					}
				}()
				res = reg.fn(keyword, page, limitPerReg)
			}()
			mu.Lock()
			results[reg.name] = res
			mu.Unlock()
		}(reg)
	}
	wg.Wait()
	return results
}

// ── human_num / trunc ───────────────────────────────────────────────────────

// searchToInt mimics Python int(n) on the loose stars/pulls value.
func searchToInt(n interface{}) (int64, bool) {
	switch v := n.(type) {
	case nil:
		return 0, false
	case int:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		return int64(v), true
	case json.Number:
		i, err := v.Int64()
		if err == nil {
			return i, true
		}
		f, err := v.Float64()
		if err == nil {
			return int64(f), true
		}
		return 0, false
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err == nil {
			return i, true
		}
		return 0, false
	}
	return 0, false
}

// searchPyFloat renders a float64 the way Python's str()/f-string does for a
// float: shortest round-trippable decimal, but always with at least one
// fractional digit (str(2.0) == "2.0", str(1.5) == "1.5", str(15.234) ==
// "15.234"). Go's FormatFloat 'g' drops the trailing ".0", which Python keeps.
func searchPyFloat(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	// If there's no decimal point and no exponent, Python appends ".0".
	if !strings.ContainsAny(s, ".eEnN") { // n/N guards inf/nan spellings
		s += ".0"
	}
	return s
}

// searchRawVal renders a loose stars/pulls value the way Python would print
// the raw JSON scalar (e.g. in show_info's "Stars: {r['stars']}"). Python's
// json decodes whole numbers as int, so a JSON 1500000 prints as "1500000",
// not "1.5e+06" (which is what Go's %v on a float64 would produce).
func searchRawVal(v interface{}) string {
	switch x := v.(type) {
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return searchPyFloat(x)
	case json.Number:
		return x.String()
	case string:
		return x
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

// searchHumanNum mirrors human_num(n).
//
// Python: after the first `n /= 1000`, n becomes a *float*, so `f"{n}{u}"`
// uses float repr (e.g. 1.5K, 2.0K, 15.234K) — NOT an integer. Only the very
// first iteration (when n is still the original int) prints as an int.
func searchHumanNum(n interface{}) string {
	if n == nil {
		return ""
	}
	val, ok := searchToInt(n)
	if !ok {
		// Python: except -> return str(n)
		return fmt.Sprintf("%v", n)
	}
	// First iteration: n is still an int.
	if val < 0 {
		if -val < 1000 {
			return fmt.Sprintf("%d%s", val, "")
		}
	} else if val < 1000 {
		return fmt.Sprintf("%d%s", val, "")
	}
	// Subsequent iterations: n is a float.
	f := float64(val) / 1000
	for _, u := range []string{"K", "M", "B"} {
		var a float64
		if f < 0 {
			a = -f
		} else {
			a = f
		}
		if a < 1000 {
			return searchPyFloat(f) + u
		}
		f /= 1000
	}
	return fmt.Sprintf("%.1fT", f)
}

// searchTrunc mirrors trunc(s, n): collapse whitespace, ellipsize at n.
func searchTrunc(s string, n int) string {
	if s == "" {
		return ""
	}
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n-1 < 0 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// searchStatsTruthy mirrors the Python `if r.get("stars"):` truthiness
// (nil / 0 / "" / "0" are all falsey).
func searchStatsTruthy(v interface{}) bool {
	if v == nil {
		return false
	}
	if s, ok := v.(string); ok {
		return s != "" && s != "0"
	}
	if i, ok := searchToInt(v); ok {
		return i != 0
	}
	return true
}

// ── raw-terminal screen layer (replaces curses) ─────────────────────────────

// searchScreen holds the raw-mode terminal handle and color helpers.
type searchScreen struct {
	in       *os.File
	out      *os.File
	oldState *unix.Termios
}

// color pair → ANSI sequence. Mirrors the curses init_pair() table:
//
//	1 cyan, 2 green, 3 yellow, 4 magenta, 5 red, 6 blue,
//	7 white-on-blue, 8 white-on-default.
func searchColor(pair int, bold bool) string {
	var fg, bg string
	switch pair {
	case 1:
		fg = "36"
	case 2:
		fg = "32"
	case 3:
		fg = "33"
	case 4:
		fg = "35"
	case 5:
		fg = "31"
	case 6:
		fg = "34"
	case 7:
		fg, bg = "37", "44"
	case 8:
		fg = "37"
	default:
		fg = "39"
	}
	seq := "\x1b[" + fg
	if bg != "" {
		seq += ";" + bg
	}
	if bold {
		seq += ";1"
	}
	return seq + "m"
}

const searchReset = "\x1b[0m"

func searchNewScreen() (*searchScreen, error) {
	s := &searchScreen{in: os.Stdin, out: os.Stdout}
	if err := s.enterRaw(); err != nil {
		return nil, err
	}
	s.write("\x1b[?25l") // hide cursor (curses.curs_set(0))
	return s, nil
}

func (s *searchScreen) enterRaw() error {
	fd := int(s.in.Fd())
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return err
	}
	old := *t
	s.oldState = &old
	// cbreak + noecho: disable canonical mode, echo, signals stay (curses cbreak).
	t.Lflag &^= unix.ICANON | unix.ECHO | unix.IEXTEN
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	return unix.IoctlSetTermios(fd, unix.TCSETS, t)
}

func (s *searchScreen) restore() {
	if s.oldState != nil {
		unix.IoctlSetTermios(int(s.in.Fd()), unix.TCSETS, s.oldState)
	}
}

func (s *searchScreen) close() {
	s.write("\x1b[?25h") // show cursor
	s.restore()
}

func (s *searchScreen) write(str string) { s.out.WriteString(str) }

func (s *searchScreen) size() (h, w int) {
	ws, err := unix.IoctlGetWinsize(int(s.out.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Row == 0 || ws.Col == 0 {
		return 24, 80
	}
	return int(ws.Row), int(ws.Col)
}

// addStr writes text at (row, col) — both 0-based, like curses addstr.
func (s *searchScreen) addStr(row, col int, text string) {
	s.write(fmt.Sprintf("\x1b[%d;%dH%s", row+1, col+1, text))
}

// addNStr writes text clipped to n runes at (row,col) with a color attr.
func (s *searchScreen) addNStr(row, col, n int, text, attr string) {
	if n < 0 {
		n = 0
	}
	r := []rune(text)
	if len(r) > n {
		r = r[:n]
	}
	s.addStr(row, col, attr+string(r)+searchReset)
}

func (s *searchScreen) refresh() {} // ANSI writes are unbuffered; no-op

// ── key codes (decoded from raw byte stream) ────────────────────────────────

const (
	skUp = iota + 256
	skDown
	skLeft
	skRight
	skPgUp
	skPgDn
)

// getKey reads one logical key, decoding arrow/page escape sequences.
func (s *searchScreen) getKey() int {
	buf := make([]byte, 1)
	n, err := s.in.Read(buf)
	if err != nil || n == 0 {
		return 27 // treat as ESC
	}
	b := buf[0]
	if b != 0x1b {
		return int(b)
	}
	// Possible escape sequence. Peek the next bytes.
	seq := make([]byte, 2)
	n2, _ := s.in.Read(seq[:1])
	if n2 == 0 {
		return 27 // bare ESC
	}
	if seq[0] != '[' && seq[0] != 'O' {
		return 27
	}
	n3, _ := s.in.Read(seq[1:2])
	if n3 == 0 {
		return 27
	}
	switch seq[1] {
	case 'A':
		return skUp
	case 'B':
		return skDown
	case 'C':
		return skRight
	case 'D':
		return skLeft
	case '5': // PgUp: ESC [ 5 ~
		tmp := make([]byte, 1)
		s.in.Read(tmp)
		return skPgUp
	case '6': // PgDn: ESC [ 6 ~
		tmp := make([]byte, 1)
		s.in.Read(tmp)
		return skPgDn
	}
	return 27
}

// ── ljust / center / rjust style helpers (rune-aware) ───────────────────────

func searchLjust(s string, w int) string {
	r := []rune(s)
	if len(r) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(r))
}

func searchCenter(s string, w int) string {
	r := []rune(s)
	if len(r) >= w {
		return s
	}
	total := w - len(r)
	left := total / 2
	right := total - left
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", right)
}

// searchWrap mirrors textwrap.wrap(text, width) closely enough for display.
func searchWrap(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	lines := []string{}
	cur := ""
	for _, wd := range words {
		if cur == "" {
			cur = wd
			continue
		}
		if len([]rune(cur))+1+len([]rune(wd)) <= width {
			cur += " " + wd
		} else {
			lines = append(lines, cur)
			cur = wd
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// ── docker_pull_inline ──────────────────────────────────────────────────────

// searchDockerPullInline mirrors docker_pull_inline(): stream a real
// `docker pull` in the foreground, then re-enter raw mode.
func searchDockerPullInline(pullCmd string, sc *searchScreen, panelTop, panelH, w int) {
	fullH, _ := sc.size()
	image := strings.TrimSpace(strings.ReplaceAll(pullCmd, "docker pull ", ""))

	// Clear panel.
	for row := panelTop; row < fullH; row++ {
		sc.addStr(row, 0, strings.Repeat(" ", w-1))
	}
	sc.addNStr(panelTop, 0, w-1, searchLjust(fmt.Sprintf(" ⬇  Pulling: %s ", image), w-1),
		searchColor(7, true))
	sc.refresh()

	// Leave raw mode / give the terminal back to docker.
	sc.close()

	fmt.Printf("\n\033[1;35m⬇  docker pull %s\033[0m\n", image)
	cmd := exec.Command("docker", "pull", image)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	ret := 0
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			ret = ee.ExitCode()
		} else {
			ret = 1
		}
	}
	if ret == 0 {
		fmt.Printf("\033[1;32m✔  Pull complete: %s\033[0m\n", image)
	} else {
		fmt.Printf("\033[1;31m✘  Pull failed (exit %d)\033[0m\n", ret)
	}
	fmt.Printf("\n\033[1;36mPress Enter to return to search...\033[0m\n")
	// input() — read a line from stdin (terminal is in cooked mode now).
	rd := make([]byte, 1)
	for {
		_, err := os.Stdin.Read(rd)
		if err != nil || rd[0] == '\n' {
			break
		}
	}

	// Re-init raw mode (mirrors curses.initscr()/restart).
	sc.enterRaw()
	sc.write("\x1b[?25l")
}

// ── TUI ─────────────────────────────────────────────────────────────────────

type searchTUI struct {
	sc             *searchScreen
	keyword        string
	selectMode     bool
	page           int
	results        []searchResult
	filtered       []searchResult
	cursor         int
	scroll         int
	registryFilter string
	statusMsg      string
	loading        bool
}

func newSearchTUI(sc *searchScreen, keyword string, selectMode bool) *searchTUI {
	t := &searchTUI{
		sc:             sc,
		keyword:        keyword,
		selectMode:     selectMode,
		page:           1,
		registryFilter: "ALL",
		statusMsg:      "[/]search  [↑↓]nav  [n/p]page  [f]filter  [i]info  [Enter]PULL  [q]quit",
	}
	if keyword != "" {
		t.doSearch()
	}
	return t
}

func (t *searchTUI) getPanel() (panelTop, panelH, w int) {
	fullH, fullW := t.sc.size()
	return 0, fullH, fullW
}

func (t *searchTUI) doSearch() {
	if t.keyword == "" {
		return
	}
	t.loading = true
	t.draw()
	t.sc.refresh()
	resultsByReg := searchAll(t.keyword, t.page, searchPageSizePerRegistry)
	t.results = []searchResult{}
	// Preserve a deterministic registry order (the original dict order).
	for _, reg := range searchRegistries {
		items, ok := resultsByReg[reg.name]
		if !ok {
			continue
		}
		for _, item := range items {
			if item.Err != "" {
				continue
			}
			item.Source = reg.name
			t.results = append(t.results, item)
		}
	}
	t.applyFilter()
	t.cursor = 0
	t.scroll = 0
	t.loading = false
	t.statusMsg = fmt.Sprintf(
		"[/]search  [↑↓]nav  [n/p]page  [f]filter  [i]info  [Enter]PULL  [q]quit  │  %d results p%d",
		len(t.results), t.page)
}

func (t *searchTUI) applyFilter() {
	if t.registryFilter == "ALL" {
		t.filtered = append([]searchResult(nil), t.results...)
	} else {
		t.filtered = nil
		for _, r := range t.results {
			if r.Source == t.registryFilter {
				t.filtered = append(t.filtered, r)
			}
		}
	}
}

// prompt mirrors the bottom-line text input. initial seeds the first char.
func (t *searchTUI) prompt(promptText, initial string) string {
	fullH, fullW := t.sc.size()
	t.sc.write("\x1b[?25h") // show cursor (echo on)
	t.sc.addStr(fullH-1, 0, strings.Repeat(" ", fullW-1))
	t.sc.addStr(fullH-1, 0, promptText)
	t.sc.refresh()

	// Temporarily go to a line-edit loop in raw mode (we handle echo ourselves).
	buf := []rune(initial)
	if initial != "" {
		t.sc.write(initial)
	}
	for {
		k := t.sc.getKey()
		switch {
		case k == 10 || k == 13: // Enter
			t.sc.write("\x1b[?25l")
			return strings.TrimSpace(string(buf))
		case k == 127 || k == 8: // Backspace
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				t.sc.write("\b \b")
			}
		case k == 27: // ESC -> cancel
			t.sc.write("\x1b[?25l")
			return ""
		case k >= 32 && k < 256:
			buf = append(buf, rune(k))
			t.sc.write(string(rune(k)))
		}
	}
}

func (t *searchTUI) draw() {
	fullH, _ := t.sc.size()
	panelTop, panelH, w := t.getPanel()

	// Clear panel (bottom half).
	for row := panelTop; row < fullH; row++ {
		t.sc.addStr(row, 0, strings.Repeat(" ", w-1))
	}

	// Separator.
	t.sc.addNStr(panelTop, 0, w-1,
		searchLjust(fmt.Sprintf("─── 🔍 regsearch: '%s' │ page %d │ %s ",
			t.keyword, t.page, t.registryFilter), w-1),
		searchColor(3, true))

	if t.loading {
		t.sc.addNStr(panelTop+panelH/2, searchMax(0, (w-16)/2), w-1,
			"  Searching...  ", searchColor(3, true))
		return
	}

	bodyStart := panelTop + 1
	bodyHeight := panelH - 3 // sep + desc + status

	if len(t.filtered) == 0 {
		t.sc.addNStr(bodyStart, 2, w-3, "No results. Type / to search.", searchColor(3, false))
	} else {
		if t.cursor < t.scroll {
			t.scroll = t.cursor
		} else if t.cursor >= t.scroll+bodyHeight {
			t.scroll = t.cursor - bodyHeight + 1
		}
		for i := 0; i < bodyHeight; i++ {
			idx := t.scroll + i
			if idx >= len(t.filtered) {
				break
			}
			r := t.filtered[idx]
			y := bodyStart + i
			selected := idx == t.cursor
			line := " " + r.Pull
			stats := []string{}
			if searchStatsTruthy(r.Stars) {
				stats = append(stats, "★"+searchHumanNum(r.Stars))
			}
			if searchStatsTruthy(r.Pulls) {
				stats = append(stats, "↓"+searchHumanNum(r.Pulls))
			}
			statsStr := strings.Join(stats, " ")
			source := r.Source
			padW := searchMax(20, w-len(statsStr)-len(source)-8)
			full := fmt.Sprintf("%s %s [%s]", searchLjust(line, padW), statsStr, source)
			if selected {
				t.sc.addNStr(y, 0, w-1, searchLjust(full, w-1), searchColor(7, true))
			} else {
				t.sc.addNStr(y, 0, w-1, full, searchColor(1, false))
			}
		}
	}

	// Desc line.
	descY := fullH - 2
	if len(t.filtered) > 0 && t.cursor >= 0 && t.cursor < len(t.filtered) {
		r := t.filtered[t.cursor]
		desc := searchTrunc(r.Desc, w-4)
		t.sc.addNStr(descY, 0, w-1, " "+desc, searchColor(2, false))
	}

	// Status.
	t.sc.addNStr(fullH-1, 0, w-1, searchLjust(t.statusMsg, w-1), searchColor(7, false))
}

func (t *searchTUI) showInfo(r searchResult) {
	fullH, _ := t.sc.size()
	panelTop, panelH, w := t.getPanel()

	fullDesc := r.Desc
	if strings.Contains(r.Registry, "docker.io") || r.Registry == "lscr.io" {
		ns := r.Namespace
		if ns == "" {
			ns = "library"
		}
		u := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/%s/", ns, r.Name)
		data, errs := searchHTTPGetJSON(u, nil, 3*time.Second)
		if errs == "" {
			m := searchAsMap(data)
			if fd := searchGetStr(m, "full_description"); fd != "" {
				fullDesc = fd
			} else if d := searchGetStr(m, "description"); d != "" {
				fullDesc = d
			}
		}
	}

	lines := []string{
		fmt.Sprintf("Name:     %s", r.Name),
		fmt.Sprintf("Registry: %s", r.Registry),
		fmt.Sprintf("Pull:     %s", r.Pull),
	}
	stats := []string{}
	if searchStatsTruthy(r.Stars) {
		stats = append(stats, fmt.Sprintf("Stars: %s", searchRawVal(r.Stars)))
	}
	if searchStatsTruthy(r.Pulls) {
		stats = append(stats, fmt.Sprintf("Pulls: %s", searchRawVal(r.Pulls)))
	}
	if len(stats) > 0 {
		lines = append(lines, fmt.Sprintf("Stats:    %s", strings.Join(stats, ", ")))
	}
	if r.Official {
		lines = append(lines, "Official: Yes")
	}
	if r.URL != "" {
		lines = append(lines, fmt.Sprintf("URL:      %s", r.URL))
	}
	lines = append(lines, "", "Description:", strings.Repeat("─", 20))
	for _, para := range strings.Split(fullDesc, "\n") {
		wrapped := searchWrap(para, w-6)
		if len(wrapped) == 0 {
			lines = append(lines, "")
		} else {
			lines = append(lines, wrapped...)
		}
	}

	scrollIdx := 0
	for {
		for row := panelTop; row < fullH; row++ {
			t.sc.addStr(row, 0, strings.Repeat(" ", w-1))
		}
		t.sc.addNStr(panelTop, 0, w-1, searchCenter(" Image Details ", w-1), searchColor(7, true))
		for i := 0; i < panelH-2; i++ {
			currIdx := scrollIdx + i
			if currIdx < len(lines) {
				ln := lines[currIdx]
				lr := []rune(ln)
				if len(lr) > w-4 {
					lr = lr[:w-4]
				}
				t.sc.addNStr(panelTop+1+i, 2, w-4, string(lr), searchColor(8, false))
			}
		}
		t.sc.addNStr(fullH-1, 0, w-1,
			searchLjust(" [↑/↓] scroll  [q/ESC] close ", w-1), searchColor(7, false))
		t.sc.refresh()
		k := t.sc.getKey()
		switch {
		case k == skUp || k == 'k':
			scrollIdx = searchMax(0, scrollIdx-1)
		case k == skDown || k == 'j':
			scrollIdx = searchMin(searchMax(0, len(lines)-(panelH-2)), scrollIdx+1)
		case k == 'q' || k == 27 || k == 'i':
			return
		}
	}
}

func (t *searchTUI) filterMenu() {
	// opts = ["ALL"] + sorted(set(source for r in results))
	set := map[string]bool{}
	for _, r := range t.results {
		set[r.Source] = true
	}
	uniq := []string{}
	for s := range set {
		uniq = append(uniq, s)
	}
	sort.Strings(uniq)
	opts := append([]string{"ALL"}, uniq...)

	fullH, _ := t.sc.size()
	panelTop, panelH, w := t.getPanel()
	_ = panelH
	sel := 0
	for {
		for row := panelTop; row < fullH; row++ {
			t.sc.addStr(row, 0, strings.Repeat(" ", w-1))
		}
		t.sc.addNStr(panelTop, 0, w-1, searchCenter(" Filter by Registry ", w-1), searchColor(7, true))
		for i, o := range opts {
			y := panelTop + 2 + i
			if y >= fullH-2 {
				break
			}
			if i == sel {
				t.sc.addNStr(y, 2, w-4, searchLjust(fmt.Sprintf("▶ %s", o), w-4), searchColor(7, true))
			} else {
				t.sc.addNStr(y, 2, w-4, fmt.Sprintf("  %s", o), searchColor(1, false))
			}
		}
		t.sc.addNStr(fullH-1, 0, w-1,
			searchLjust(" [↑/↓] navigate  [Enter] select  [q] cancel ", w-1), searchColor(7, false))
		t.sc.refresh()
		k := t.sc.getKey()
		switch {
		case k == skUp || k == 'k':
			sel = (sel - 1 + len(opts)) % len(opts)
		case k == skDown || k == 'j':
			sel = (sel + 1) % len(opts)
		case k == 10 || k == 13:
			t.registryFilter = opts[sel]
			t.applyFilter()
			t.cursor = 0
			t.scroll = 0
			return
		case k == 'q' || k == 27:
			return
		}
	}
}

func (t *searchTUI) run() {
	for {
		t.draw()
		t.sc.refresh()
		k := t.sc.getKey()
		switch {
		case k == 'q' || k == 27:
			return
		case k == '/':
			kw := t.prompt(" Search: ", "")
			if kw != "" {
				t.keyword = kw
				t.page = 1
				t.registryFilter = "ALL"
				t.doSearch()
			}
		case k == 'n' || k == skRight:
			if t.keyword != "" {
				t.page++
				t.doSearch()
			}
		case k == 'p' || k == skLeft:
			if t.keyword != "" && t.page > 1 {
				t.page--
				t.doSearch()
			}
		case k >= '0' && k <= '9':
			pNum := t.prompt(" Go to Page Number: ", string(rune(k)))
			if pNum != "" && searchIsDigits(pNum) {
				targetP, _ := strconv.Atoi(pNum)
				if targetP > 0 {
					t.page = targetP
					t.doSearch()
				}
			}
		case k == 'f':
			if len(t.results) > 0 {
				t.filterMenu()
			}
		case k == 'i':
			if len(t.filtered) > 0 && t.cursor >= 0 && t.cursor < len(t.filtered) {
				t.showInfo(t.filtered[t.cursor])
			}
		case k == skUp || k == 'k':
			if len(t.filtered) > 0 {
				t.cursor = searchMax(0, t.cursor-1)
			}
		case k == skDown || k == 'j':
			if len(t.filtered) > 0 {
				t.cursor = searchMin(len(t.filtered)-1, t.cursor+1)
			}
		case k == skPgUp:
			if len(t.filtered) > 0 {
				t.cursor = searchMax(0, t.cursor-10)
			}
		case k == skPgDn:
			if len(t.filtered) > 0 {
				t.cursor = searchMin(len(t.filtered)-1, t.cursor+10)
			}
		case k == 10 || k == 13:
			// ENTER = docker pull OR select mode.
			if len(t.filtered) > 0 && t.cursor >= 0 && t.cursor < len(t.filtered) {
				r := t.filtered[t.cursor]
				pullCmd := r.Pull
				if pullCmd != "" && !strings.Contains(pullCmd, "helm install") {
					if t.selectMode {
						os.WriteFile("/tmp/stacks_build_selected", []byte(pullCmd), 0o644)
						return
					}
					_, panelH, w := t.getPanel()
					fullH, _ := t.sc.size()
					searchDockerPullInline(
						"docker pull "+pullCmd, t.sc,
						fullH-panelH, panelH, w)
					t.statusMsg = fmt.Sprintf("✔ Pulled: %s  │  [/]search [↑↓]nav [q]quit", pullCmd)
				} else {
					t.statusMsg = fmt.Sprintf("Helm: %s", pullCmd)
				}
			}
		case k == 'g':
			t.cursor = 0
		case k == 'G':
			if len(t.filtered) > 0 {
				t.cursor = len(t.filtered) - 1
			}
		}
	}
}

// ── small numeric helpers (module-unique names) ─────────────────────────────

func searchMax(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func searchMin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func searchIsDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ── entry point (mirrors main() / argparse) ─────────────────────────────────

// cmdRegsearch is the "stacks regsearch [keyword] [--select]" command.
func cmdRegsearch(args []string) {
	keyword := ""
	selectMode := false
	for _, a := range args {
		if a == "--select" {
			selectMode = true
		} else if !strings.HasPrefix(a, "-") && keyword == "" {
			keyword = a
		}
	}

	sc, err := searchNewScreen()
	if err != nil {
		fmt.Fprintln(os.Stderr, "regsearch: cannot init terminal:", err)
		os.Exit(1)
	}
	// curses.wrapper guarantees cleanup even on panic.
	defer sc.close()
	defer func() {
		if r := recover(); r != nil {
			sc.close()
			fmt.Fprintln(os.Stderr, "regsearch:", r)
			os.Exit(1)
		}
	}()
	// Clear the whole screen on start (curses init shows a blank canvas).
	sc.write("\x1b[2J")

	t := newSearchTUI(sc, keyword, selectMode)
	t.run()
}
