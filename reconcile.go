package main

// reconcile.go — faithful Go port of stacks_reconcile.py.
// Container-state reconcile for `stacks ... repair`: for one compose file, bring
// container state in line with what the compose DEFINES, healing half-up stacks:
//   1. remove orphan hash-prefixed dup containers (<12hex>_<name>) for this stack
//   2. start this stack's containers stuck in 'created'
//   3. create defined-but-missing services one at a time (compose up --no-deps key)
// Per-service so one failure never blocks the rest. Idempotent.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	reSvcKey    = regexp.MustCompile(`^  ([A-Za-z0-9_.-]+):\s*$`)
	reCName     = regexp.MustCompile(`^\s+container_name:\s*"?([A-Za-z0-9_.-]+)`)
	reOrphanDup = regexp.MustCompile(`^[0-9a-f]{12}_(.+)$`)
)

// parseServices mirrors parse_services(): {container_name: service_key}.
func parseServices(stackFile string) map[string]string {
	defn := map[string]string{}
	raw, err := os.ReadFile(stackFile)
	if err != nil {
		return defn
	}
	key := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if m := reSvcKey.FindStringSubmatch(line + "\n"); m != nil {
			key = m[1]
			continue
		}
		if cm := reCName.FindStringSubmatch(line); cm != nil && key != "" {
			defn[cm[1]] = key
		}
	}
	return defn
}

// reconcile mirrors reconcile(): heal one stack file; prints actions, returns summary.
func reconcile(stackFile string) string {
	if st, err := os.Stat(stackFile); err != nil || st.IsDir() {
		return "reconcile: no such stack file"
	}
	defn := parseServices(stackFile)
	if len(defn) == 0 {
		return "reconcile: no services defined"
	}
	names := map[string]bool{}
	for n := range defn {
		names[n] = true
	}
	states := containerStateMap()
	cwd := filepath.Dir(stackFile)
	var actions []string

	// 1. remove orphan hash-prefixed duplicates for this stack's services
	for _, n := range keysOf(states) {
		if m := reOrphanDup.FindStringSubmatch(n); m != nil && names[m[1]] {
			if removeContainer(n, true, false) {
				actions = append(actions, "removed orphan dup "+n)
				delete(states, n)
			}
		}
	}

	// 2. start this stack's 'created' (never-started) containers
	for cname := range names {
		if states[cname] == "created" {
			if startContainer(cname) {
				actions = append(actions, "started "+cname)
			} else {
				actions = append(actions, "start-FAILED "+cname)
			}
		}
	}

	// 3. create defined-but-missing services, one at a time
	for cname, key := range defn {
		if _, ok := states[cname]; !ok {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
			cmd := exec.CommandContext(ctx, "docker", "compose", "-f", stackFile,
				"up", "-d", "--no-deps", key)
			cmd.Dir = cwd
			cmd.Env = dockerEnv()
			var errb strings.Builder
			cmd.Stderr = &errb
			err := cmd.Run()
			cancel()
			if err == nil {
				actions = append(actions, "created "+cname)
			} else {
				last := lastLine(strings.TrimSpace(errb.String()))
				if len(last) > 70 {
					last = last[:70]
				}
				actions = append(actions, "create-FAILED "+cname+": "+last)
			}
		}
	}

	for _, a := range actions {
		fmt.Println("  " + a)
	}
	if len(actions) > 0 {
		return "reconcile: " + strconv.Itoa(len(actions)) + " action(s)"
	}
	return "reconcile: already consistent"
}

// keysOf returns a snapshot of a map's keys (so we can delete while iterating).
func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// lastLine returns the final non-empty line of s (mirrors splitlines()[-1]).
func lastLine(s string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	return lines[len(lines)-1]
}
