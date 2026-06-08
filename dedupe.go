package main

// dedupe.go — `stacks dedupe` (report): find container_names declared in more than
// one stack, and recommend which copy to keep. Keeper rule (matches the Python
// version): the stack running the live container wins; else category priority
// core>db>net>ai>data>srvs>dev (dev/scratch loses), tie-broken by lower number.
// *-ext (VPS) stacks are skipped.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

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
