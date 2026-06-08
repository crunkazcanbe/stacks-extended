package main

// ls.go — `stacks ls`: list the stack .yml files (skips *-ext VPS stacks).

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

func cmdLs(args []string) {
	dir := stacksDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Println("cannot read", dir, ":", err)
		return
	}
	var stacks []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".yml") && !strings.HasSuffix(n, "-ext.yml") {
			stacks = append(stacks, strings.TrimSuffix(n, ".yml"))
		}
	}
	sort.Strings(stacks)
	fmt.Printf("📦 %d stacks in %s:\n", len(stacks), dir)
	for _, s := range stacks {
		fmt.Println("  \x1b[36m" + s + "\x1b[0m")
	}
}
