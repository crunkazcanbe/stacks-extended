// sync.go — faithful Go port of stacks_sync.py.
//
// Sync descriptions and all_services.txt from compose files.
// Run automatically after stacks up/down/build or manually.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// syncDescDir is the per-user descriptions directory (CONF_DIR/descriptions).
func syncDescDir() string { return filepath.Join(configDir(), "descriptions") }

// syncSvcFile is the all_services.txt path (CONF_DIR/all_services.txt).
func syncSvcFile() string { return filepath.Join(configDir(), "all_services.txt") }

// syncSvc is one (service, image) pair parsed from a compose file.
type syncSvc struct {
	svc string
	img string
}

// getDefaultDesc mirrors get_default_desc(): prefer stacks_config.load()
// (YAML master, falling back to stacks.conf), then a direct stacks.conf read,
// then a hardcoded default.
func getDefaultDesc() string {
	// First try stacks_config.load().get("BUILD_DEFAULT_DESC").
	if v := configLoad()["BUILD_DEFAULT_DESC"]; v != "" {
		return v
	}
	// Then a direct stacks.conf read (Python's fallback loop).
	if v := confValue("BUILD_DEFAULT_DESC"); v != "" {
		return v
	}
	return "A powerful service running on BellzServer. Edit this description."
}

var (
	syncReSection = regexp.MustCompile(`^(networks|volumes|configs|secrets):`)
	syncReSvcKey  = regexp.MustCompile(`^  [a-zA-Z0-9_-]+:\s*$`)
)

// parseStack mirrors parse_stack(): get all services and images from a compose file.
func parseStack(fpath string) []syncSvc {
	var services []syncSvc
	data, err := os.ReadFile(fpath)
	if err != nil {
		return services
	}
	content := string(data)
	inServices := false
	currentSvc := ""
	currentImg := ""
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == "services:" {
			inServices = true
			continue
		}
		if inServices && syncReSection.MatchString(line) {
			inServices = false
			continue
		}
		if inServices && syncReSvcKey.MatchString(line) {
			if currentSvc != "" {
				services = append(services, syncSvc{currentSvc, currentImg})
			}
			currentSvc = strings.TrimSuffix(strings.TrimSpace(line), ":")
			currentImg = ""
		}
		if inServices && currentSvc != "" && strings.Contains(line, "image:") {
			parts := strings.SplitN(line, "image:", 2)
			currentImg = strings.Trim(strings.TrimSpace(parts[1]), "'\"")
		}
		if inServices && currentSvc != "" && strings.Contains(line, "container_name:") {
			parts := strings.SplitN(line, "container_name:", 2)
			currentSvc = strings.TrimSpace(parts[1])
		}
	}
	if currentSvc != "" {
		services = append(services, syncSvc{currentSvc, currentImg})
	}
	return services
}

// syncDescriptions mirrors sync_descriptions(): add missing services, remove
// deleted ones from the descriptions file. Returns (added, removed).
func syncDescriptions(stackName string, services []syncSvc, defaultDesc string) (int, int) {
	os.MkdirAll(syncDescDir(), 0o755)
	descFile := filepath.Join(syncDescDir(), stackName+".conf")
	existing := ""
	if b, err := os.ReadFile(descFile); err == nil {
		existing = string(b)
	} else {
		existing = fmt.Sprintf("# %s — Service Descriptions\n# Edit the description under each service name.\n#\n", stackName)
	}

	// Build set of valid service names (normalize dash/underscore).
	valid := map[string]bool{}
	for _, s := range services {
		valid[s.svc] = true
		valid[strings.ReplaceAll(s.svc, "-", "_")] = true
		valid[strings.ReplaceAll(s.svc, "_", "-")] = true
	}

	// Parse existing file into blocks.
	// Header = lines before first service entry.
	// Each block = service name line + following lines.
	var headerLines []string
	blocks := map[string][]string{}      // {svc_name: [lines]}
	var blockOrder []string              // preserve insertion order
	currentSvc := ""
	currentSet := false
	inHeader := true

	for _, line := range strings.Split(existing, "\n") {
		stripped := strings.TrimSpace(line)
		// Bare service name: non-empty, no "#", no ":", not "-".
		if stripped != "" && !strings.HasPrefix(stripped, "#") && !strings.Contains(stripped, ":") && !strings.HasPrefix(stripped, "-") {
			inHeader = false
			currentSvc = stripped
			currentSet = true
			if _, ok := blocks[currentSvc]; !ok {
				blockOrder = append(blockOrder, currentSvc)
			}
			blocks[currentSvc] = []string{}
		} else if inHeader {
			headerLines = append(headerLines, line)
		} else if currentSet {
			blocks[currentSvc] = append(blocks[currentSvc], line)
		}
	}

	// Rebuild: keep header, keep valid services, add missing ones.
	added, removed := 0, 0
	result := strings.TrimRight(strings.Join(headerLines, "\n"), "\n")

	for _, svcName := range blockOrder {
		svcLines := blocks[svcName]
		if valid[svcName] {
			result += "\n\n" + svcName + "\n" + strings.Trim(strings.Join(svcLines, "\n"), "\n")
		} else {
			removed++
		}
	}

	// Add missing services.
	for _, s := range services {
		svc := s.svc
		svcNorm := strings.ReplaceAll(svc, "-", "_")
		svcDash := strings.ReplaceAll(svc, "_", "-")
		_, ok1 := blocks[svc]
		_, ok2 := blocks[svcNorm]
		_, ok3 := blocks[svcDash]
		if !ok1 && !ok2 && !ok3 {
			result += "\n\n" + svc + "\n# " + defaultDesc
			added++
		}
	}

	result = strings.Trim(result, "\n") + "\n"

	if added != 0 || removed != 0 {
		os.WriteFile(descFile, []byte(result), 0o644)
	}

	return added, removed
}

// syncAllServices mirrors sync_all_services(): update all_services.txt - add new,
// remove deleted. Returns added.
func syncAllServices(stackName string, services []syncSvc) int {
	existing := ""
	if b, err := os.ReadFile(syncSvcFile()); err == nil {
		existing = string(b)
	} else {
		existing = "# ALL SERVICES — BellzServer\n# Format: stack | service | image\n# =========================================\n"
	}

	validNames := map[string]bool{}
	for _, s := range services {
		validNames[s.svc] = true
	}
	section := "# ── " + strings.ToUpper(stackName)
	lines := strings.Split(existing, "\n")
	var newLines []string
	added, removed := 0, 0

	for _, line := range lines {
		// Check if this is a service line for this stack.
		if strings.HasPrefix(line, stackName) && strings.Contains(line, "|") {
			rawParts := strings.Split(line, "|")
			parts := make([]string, len(rawParts))
			for i, p := range rawParts {
				parts[i] = strings.TrimSpace(p)
			}
			if len(parts) >= 2 {
				svc := strings.TrimSpace(parts[1])
				if validNames[svc] {
					newLines = append(newLines, line)
				} else {
					removed++
					continue
				}
			} else {
				newLines = append(newLines, line)
			}
		} else {
			newLines = append(newLines, line)
		}
	}
	_ = removed // matches Python: counted but not returned

	existing = strings.Join(newLines, "\n")

	// Add missing.
	for _, s := range services {
		svc, img := s.svc, s.img
		if !strings.Contains(existing, "| "+svc+" ") &&
			!strings.Contains(existing, "| "+svc+"\n") &&
			!strings.Contains(existing, "| "+svc) {
			entry := fmt.Sprintf("%-12s | %-35s | %s", stackName, svc, img)
			if strings.Contains(existing, section) {
				lines2 := strings.Split(existing, "\n")
				for i, l := range lines2 {
					if strings.HasPrefix(l, section) {
						lines2 = insertAt(lines2, i+1, entry)
						break
					}
				}
				existing = strings.Join(lines2, "\n")
			} else {
				existing += fmt.Sprintf("\n%s ──────────────────────────────────────\n%s\n", section, entry)
			}
			added++
		}
	}

	os.WriteFile(syncSvcFile(), []byte(existing), 0o644)
	return added
}

// syncMain mirrors main(): walk all *.yml in STACKS_DIR and sync.
func syncMain() {
	defaultDesc := getDefaultDesc()
	totalDesc := 0
	totalSvc := 0

	matches, _ := filepath.Glob(filepath.Join(stacksDir(), "*.yml"))
	sort.Strings(matches)
	for _, fpath := range matches {
		stackName := strings.TrimSuffix(filepath.Base(fpath), ".yml")
		services := parseStack(fpath)
		if len(services) == 0 {
			continue
		}
		addedD, removedD := syncDescriptions(stackName, services, defaultDesc)
		addedS := syncAllServices(stackName, services)
		totalDesc += addedD + removedD
		totalSvc += addedS
	}

	if totalDesc != 0 || totalSvc != 0 {
		fmt.Println("Sync complete: descriptions updated, all_services updated")
	}
}
