package main

// gensrvs.go — faithful Go port of stacks_gen_srvs.py.
// Writes <configDir>/all_services.txt: "stack | service | image" for every
// service in every *.yml, grouped by stack. Universal paths (stacksDir/configDir).

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	reSrvServices = regexp.MustCompile(`^services:`)
	reSrvFirstLet = regexp.MustCompile(`^[a-zA-Z]`)
	reSrvKey      = regexp.MustCompile(`^  ([a-zA-Z0-9_.-]+):\s*$`)
	reSrvImage    = regexp.MustCompile(`\s+image:\s+(.+)`)
)

// genServices mirrors stacks_gen_srvs.py: build all_services.txt.
func genServices() {
	stacksDirP := stacksDir()
	out := filepath.Join(configDir(), "all_services.txt")

	var lines []string
	lines = append(lines, "# ALL SERVICES — BellzServer\n# Format: stack | service | image\n# "+
		strings.Repeat("=", 41)+"\n\n")

	entries, _ := os.ReadDir(stacksDirP)
	var ymls []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yml") {
			ymls = append(ymls, e.Name())
		}
	}
	sort.Strings(ymls)

	total := 0
	for _, yml := range ymls {
		stack := strings.ReplaceAll(yml, ".yml", "")
		lines = append(lines, "# ── "+strings.ToUpper(stack)+" "+strings.Repeat("─", 38)+"\n")
		raw, err := os.ReadFile(filepath.Join(stacksDirP, yml))
		if err != nil {
			lines = append(lines, "\n")
			continue
		}
		inServices := false
		current := ""
		image := ""
		for _, line := range strings.Split(string(raw), "\n") {
			s := strings.TrimRight(line, " \t\r\n\v\f")
			if reSrvServices.MatchString(s) {
				inServices = true
				continue
			}
			if reSrvFirstLet.MatchString(s) && !strings.HasPrefix(s, " ") && inServices {
				inServices = false
				continue
			}
			if !inServices {
				continue
			}
			if m := reSrvKey.FindStringSubmatch(s); m != nil {
				if current != "" {
					lines = append(lines, fmt.Sprintf("%-12s | %-35s | %s\n", stack, current, image))
					total++
				}
				current = m[1]
				image = ""
				continue
			}
			if current != "" {
				if im := reSrvImage.FindStringSubmatch(s); im != nil {
					image = strings.TrimSpace(im[1])
				}
			}
		}
		if current != "" {
			lines = append(lines, fmt.Sprintf("%-12s | %-35s | %s\n", stack, current, image))
			total++
		}
		lines = append(lines, "\n")
	}

	os.WriteFile(out, []byte(strings.Join(lines, "")), 0644)
	fmt.Printf("  \033[1;32m✔ %d services written to:\033[0m %s\n", total, out)
}
