package main

// gengi.go — faithful Go port of stacks_gen_gi.py.
//
// Generates the global_inject.conf file: keys injected into every service by
// `stacks fix`. Scans the stacks dir for unique alphabetic prefixes, assigns
// CPU cores, and writes a fully-commented config template.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// gengiPrefixRe matches a leading run of ASCII letters (Python: ^([a-zA-Z]+)).
var gengiPrefixRe = regexp.MustCompile(`^([a-zA-Z]+)`)

// gengiHeavyPrefixes mirrors the Python heavy_prefixes set.
var gengiHeavyPrefixes = map[string]bool{"ai": true, "ml": true, "llm": true}

// genGlobalInject is the Go equivalent of running stacks_gen_gi.py.
//
// confPath defaults to /tmp/global_inject.conf when empty.
// stacksDirArg defaults to stacksDir() when empty (replacing the hardcoded
// /home/bellzserver/MyDocker/Stacks default in the Python).
func genGlobalInject(confPath, stacksDirArg string) error {
	if confPath == "" {
		confPath = "/tmp/global_inject.conf"
	}
	if stacksDirArg == "" {
		stacksDirArg = stacksDir()
	}

	ncores := runtime.NumCPU()
	if ncores == 0 {
		ncores = 8
	}

	// Scan stacks dir for unique prefixes.
	prefixSet := map[string]bool{}
	if fi, err := os.Stat(stacksDirArg); err == nil && fi.IsDir() {
		entries, err := os.ReadDir(stacksDirArg)
		if err == nil {
			for _, e := range entries {
				f := e.Name()
				if strings.HasSuffix(f, ".yml") || strings.HasSuffix(f, ".yaml") {
					name := strings.ReplaceAll(f, ".yml", "")
					name = strings.ReplaceAll(name, ".yaml", "")
					if m := gengiPrefixRe.FindStringSubmatch(name); m != nil {
						prefixSet[m[1]] = true
					}
				}
			}
		}
	}

	prefixes := make([]string, 0, len(prefixSet))
	for p := range prefixSet {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)

	usable := ncores / 2
	if usable < 4 {
		usable = 4
	}

	// Assign cores. Use an ordered slice of keys to preserve insertion order
	// (Python dict preserves insertion order: regular prefixes first, then
	// heavy prefixes appended).
	coreMap := map[string]string{}
	coreOrder := []string{}
	regular := []string{}
	for _, p := range prefixes {
		if !gengiHeavyPrefixes[p] {
			regular = append(regular, p)
		}
	}
	for i, p := range regular {
		if _, ok := coreMap[p]; !ok {
			coreOrder = append(coreOrder, p)
		}
		coreMap[p] = fmt.Sprintf("%d", i%usable)
	}
	for _, p := range prefixes {
		if gengiHeavyPrefixes[p] {
			if _, ok := coreMap[p]; !ok {
				coreOrder = append(coreOrder, p)
			}
			coreMap[p] = fmt.Sprintf("0-%d", ncores-1)
		}
	}

	lines := []string{
		"# ==============================================================================",
		"# global_inject.conf — Keys injected into every service by stacks fix",
		"# Auto-generated on first run. Edit freely.",
		"# Values: 0=disabled, 1=add-only, force=always override",
		"# _FORCE=1 forces individual key. FORCE_ALL=1 forces everything.",
		"# Anchor keys -> x-common-caps block. Service keys -> each service.",
		"# ==============================================================================",
		"",
		"FORCE_ALL=0",
		"",
		"# -- Stop behavior (-> anchor) ------------------------------------------------",
		"INJECT_STOP_GRACE=1",
		"INJECT_STOP_GRACE_FORCE=0",
		"STOP_GRACE_PERIOD=120s",
		"STOP_SIGNAL=SIGTERM",
		"",
		"# -- Logging (-> anchor) ------------------------------------------------------",
		"INJECT_LOGGING=1",
		"INJECT_LOGGING_FORCE=0",
		"LOGGING_DRIVER=json-file",
		"LOGGING_MAX_SIZE=50m",
		"LOGGING_MAX_FILE=5",
		"",
		"# -- Restart policy (-> anchor) -----------------------------------------------",
		"INJECT_RESTART=0",
		"INJECT_RESTART_FORCE=0",
		"RESTART_POLICY=unless-stopped",
		"",
		"# -- Resource limits (-> each service) ----------------------------------------",
		"INJECT_DEPLOY=0",
		"INJECT_DEPLOY_FORCE=0",
		"DEPLOY_MEMORY_LIMIT=2G",
		"DEPLOY_CPU_LIMIT=0.20",
		"DEPLOY_MEMORY_RESERVATION=256M",
		"",
		"# -- Block IO (-> each service) -----------------------------------------------",
		"INJECT_BLKIO=0",
		"INJECT_BLKIO_FORCE=0",
		"BLKIO_WEIGHT=500",
		"BLKIO_READ_BPS=750mb",
		"BLKIO_WRITE_BPS=750mb",
		"",
		"# -- ulimits (-> each service) ------------------------------------------------",
		"INJECT_ULIMITS=0",
		"INJECT_ULIMITS_FORCE=0",
		"ULIMIT_NOFILE_SOFT=65535",
		"ULIMIT_NOFILE_HARD=65535",
		"ULIMIT_NPROC=65535",
		"",
		"# -- CPU core pinning (-> each service) --------------------------------------",
		fmt.Sprintf("# Detected %d stack prefix(es): %s", len(prefixes), strings.Join(prefixes, ", ")),
		fmt.Sprintf("# System has %d cores. Containers use 0-%d, host keeps %d-%d", ncores, usable-1, usable, ncores-1),
		"INJECT_CPUSET=0",
		"INJECT_CPUSET_FORCE=0",
		"CPU_SHARES_default=256",
		"CPU_SHARES_heavy=4096",
		fmt.Sprintf("CPUSET_default=0-%d", usable-1),
	}

	for _, p := range coreOrder {
		lines = append(lines, fmt.Sprintf("CPUSET_%s=%s", p, coreMap[p]))
	}

	lines = append(lines,
		"# Container names that get all cores (space-separated)",
		"CPUSET_heavy_containers=",
		"",
		"# -- Custom YAML injected into x-common-caps anchor --------------------------",
		"[custom_anchor]",
		"[/custom_anchor]",
		"",
		"# -- Custom YAML injected into every service ----------------------------------",
		"[custom_service]",
		"[/custom_service]",
	)

	if dir := filepath.Dir(confPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(confPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return err
	}

	fmt.Printf("Generated %s with %d prefixes: %s\n", confPath, len(prefixes), strings.Join(prefixes, ", "))
	return nil
}
