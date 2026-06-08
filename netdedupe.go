package main

// netdedupe.go — `stacks netdedupe` (report): find networks declared as a creator
// (external:false) in more than one stack — those throw the "network exists but was
// not created for project" warning. Owner = same category priority as dedupe; the
// rest should become external:true. *-ext (VPS) stacks are skipped.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

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
