package main

// repairdynamic.go — faithful Go port of stacks_repair_dynamic.py.
//
// Repair Traefik dynamic config files. Learned from ai_0.yml dynamic
// (perfect reference).

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// STANDARD_MIDDLEWARES — every router should have these middlewares.
var repairStandardMiddlewares = []string{
	"https-header",
	"crowdsec-bouncer",
	"global-retry",
	"compress",
	"inflight",
	"buffering",
	"rate-limit",
}

const (
	repairSablierURL   = "http://sablier:10000"
	repairEntryPoints  = "[web]"
)

// repairDynamicsDir resolves the default Dynamics directory generically.
// The Python hardcodes /home/bellzserver/MyDocker/Configs/Dynamics; here we
// derive it from the MyDocker root (the parent of stacksDir(), which is
// .../MyDocker/Stacks) so it honours STACKS_DIR / STACKS_DATA_DIR overrides,
// falling back to ~/MyDocker/Configs/Dynamics.
func repairDynamicsDir() string {
	myDocker := filepath.Dir(stacksDir())
	if myDocker == "" || myDocker == "." || myDocker == string(filepath.Separator) {
		myDocker = filepath.Join(home(), "MyDocker")
	}
	return filepath.Join(myDocker, "Configs", "Dynamics")
}

// repairDynamic repairs a single dynamic config file. Returns the list of fixes.
func repairDynamic(path string, dryRun bool) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	content := string(raw)
	original := content
	var fixes []string

	var f []string
	content, f = repairFixSablierURL(content)
	fixes = append(fixes, f...)

	content, f = repairFixEntryPoints(content)
	fixes = append(fixes, f...)

	content, f = repairFixIndentation(content)
	fixes = append(fixes, f...)

	content, f = repairFixMissingMiddlewares(content)
	fixes = append(fixes, f...)

	if !dryRun && content != original {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return fixes, err
		}
	}

	return fixes, nil
}

var repairSablierRe = regexp.MustCompile(`sablierUrl:\s*"([^"]+)"`)

// repairFixSablierURL fixes wrong sablierUrl values.
func repairFixSablierURL(content string) (string, []string) {
	var fixes []string
	out := repairSablierRe.ReplaceAllStringFunc(content, func(m string) string {
		sub := repairSablierRe.FindStringSubmatch(m)
		if sub[1] != repairSablierURL {
			fixes = append(fixes, fmt.Sprintf("sablier_url: fixed to %s", repairSablierURL))
			return fmt.Sprintf("sablierUrl: \"%s\"", repairSablierURL)
		}
		return m
	})
	return out, fixes
}

var repairEntryPointsRe = regexp.MustCompile(`entryPoints:\s*(\[.*?\])`)

// repairFixEntryPoints fixes entryPoints format.
func repairFixEntryPoints(content string) (string, []string) {
	var fixes []string
	out := repairEntryPointsRe.ReplaceAllStringFunc(content, func(m string) string {
		sub := repairEntryPointsRe.FindStringSubmatch(m)
		val := strings.TrimSpace(sub[1])
		if val != repairEntryPoints {
			fixes = append(fixes, fmt.Sprintf("entryPoints: fixed to %s", repairEntryPoints))
			return fmt.Sprintf("entryPoints: %s", repairEntryPoints)
		}
		return m
	})
	return out, fixes
}

// repairFixIndentation fixes common indentation issues - ensure 2-space indent.
// (Mirrors the Python: conservative — it computes candidate fixes but never
// applies them, so it returns the content unchanged with no fixes.)
func repairFixIndentation(content string) (string, []string) {
	var fixes []string
	lines := strings.Split(content, "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		// Fix 4-space indent to 2-space (only for non-art lines)
		if !strings.HasPrefix(line, "#") && !strings.Contains(line, "🌸") {
			stripped := strings.TrimLeft(line, " ")
			spaces := len(line) - len(stripped)
			if spaces > 0 && spaces%4 == 0 && spaces%2 == 0 {
				// Check if this looks like 4-space indented YAML
				newSpaces := spaces / 2
				newLine := strings.Repeat(" ", newSpaces) + stripped
				if newLine != line {
					// Only fix if it makes the file more consistent
					// Conservative - don't auto-fix indentation blindly
					_ = newLine
				}
			}
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n"), fixes
}

var repairRouterRe = regexp.MustCompile(`(?s)(\w+-router):\s*\n.*?middlewares:\s*\[([^\]]+)\]`)

// repairFixMissingMiddlewares checks routers are missing standard middlewares and warns.
func repairFixMissingMiddlewares(content string) (string, []string) {
	var fixes []string
	matches := repairRouterRe.FindAllStringSubmatch(content, -1)
	for _, mm := range matches {
		routerName := mm[1]
		mwStr := mm[2]
		var middlewares []string
		for _, m := range strings.Split(mwStr, ",") {
			middlewares = append(middlewares, strings.TrimSpace(m))
		}
		for _, std := range repairStandardMiddlewares {
			if !inList(middlewares, std) {
				fixes = append(fixes, fmt.Sprintf("missing_middleware: %s missing %s", routerName, std))
			}
		}
	}
	return content, fixes
}

// repairScanAll scans every dynamic file in a directory and repairs each.
func repairScanAll(dynamicsDir string, dryRun bool) error {
	entries, err := os.ReadDir(dynamicsDir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	total := 0
	for _, fname := range names {
		if !strings.HasSuffix(fname, ".yml") && !strings.HasSuffix(fname, ".yaml") {
			continue
		}
		path := filepath.Join(dynamicsDir, fname)
		fixes, err := repairDynamic(path, dryRun)
		if err != nil {
			continue
		}
		if len(fixes) > 0 {
			prefix := ""
			if dryRun {
				prefix = "[dry-run] "
			}
			fmt.Printf("%sFixed %s:\n", prefix, fname)
			for _, f := range fixes {
				fmt.Printf("  - %s\n", f)
			}
			total += len(fixes)
		}
	}
	fmt.Printf("\nTotal fixes: %d\n", total)
	return nil
}

// repairDynamicMain is the entry point mirroring the Python __main__ block.
func repairDynamicMain(args []string) {
	target := repairDynamicsDir()
	if len(args) > 0 {
		target = args[0]
	}
	dryRun := inList(args, "--dry-run")

	info, err := os.Stat(target)
	if err == nil && !info.IsDir() {
		fixes, ferr := repairDynamic(target, dryRun)
		if ferr != nil {
			fmt.Println(ferr)
			return
		}
		for _, f := range fixes {
			fmt.Printf("  - %s\n", f)
		}
		fmt.Printf("Total: %d\n", len(fixes))
	} else {
		if err := repairScanAll(target, dryRun); err != nil {
			fmt.Println(err)
		}
	}
}
