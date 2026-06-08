package main

// proxyscale.go — faithful Go port of stacks_proxy_file.py + stacks_scale_file.py.
// Toggle a label across a compose file: proxy = traefik.enable=<val>;
// scale = sablier.enable=<val> + sablier.group=<prefix>. Either one service
// (container_name == svc) or "__all__" (every service, honoring a skip list).

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	reCnameWS = regexp.MustCompile(`\s+container_name:\s+(\S+)`)
	reLabels  = regexp.MustCompile(`\s+labels:\s*$`)
	rePrefix  = regexp.MustCompile(`([a-zA-Z]+)`)
)

// toggleSpec captures the differences between proxy and scale.
type toggleSpec struct {
	enableKey   string             // "traefik.enable" / "sablier.enable"
	allInsert   func() []string    // lines to insert in the __all__ path
	singleInsert string            // string spliced after labels: in the single-svc path
}

// proxyFile mirrors stacks_proxy_file.py.
func proxyFile(path, svc, val, skipArg string) {
	applyToggle(path, svc, val, skipArg, toggleSpec{
		enableKey:    "traefik.enable",
		allInsert:    func() []string { return []string{`      - "traefik.enable=` + val + `"`} },
		singleInsert: "\n      - \"traefik.enable=" + val + "\"",
	})
}

// scaleFile mirrors stacks_scale_file.py.
func scaleFile(path, svc, val, skipArg string) {
	prefix := ""
	if m := rePrefix.FindStringSubmatch(filepath.Base(path)); m != nil {
		prefix = m[1]
	}
	applyToggle(path, svc, val, skipArg, toggleSpec{
		enableKey: "sablier.enable",
		allInsert: func() []string {
			return []string{`      - "sablier.enable=` + val + `"`, `      - "sablier.group=` + prefix + `"`}
		},
		singleInsert: "\n      - \"sablier.enable=" + val + "\"\n      - \"sablier.group=" + prefix + "\"",
	})
}

func applyToggle(path, svc, val, skipArg string, sp toggleSpec) {
	var skip []string
	if val == "true" {
		skip = strings.Fields(skipArg)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	content := string(raw)
	subRe := regexp.MustCompile(regexp.QuoteMeta(sp.enableKey) + `=(true|false)`)
	repl := sp.enableKey + "=" + val

	if svc == "__all__" {
		lines := splitLines(content)
		var result []string
		skipCurrent, inLabels, hasX := false, false, false
		labelInsertIdx := -1
		for _, line := range lines {
			if m := reCnameWS.FindStringSubmatch(line); m != nil {
				if !skipCurrent && !hasX && labelInsertIdx != -1 {
					result = insertAt(result, labelInsertIdx, sp.allInsert()...)
				}
				skipCurrent = inList(skip, m[1])
				inLabels, hasX, labelInsertIdx = false, false, -1
			}
			if !skipCurrent {
				switch {
				case reLabels.MatchString(line):
					inLabels, hasX = true, false
				case inLabels && strings.HasPrefix(strings.TrimSpace(line), "- "):
					if strings.Contains(line, sp.enableKey) {
						hasX = true
						line = subRe.ReplaceAllString(line, repl)
					} else {
						labelInsertIdx = len(result) + 1
					}
				case inLabels && strings.TrimSpace(line) != "" && !strings.HasPrefix(strings.TrimSpace(line), "-"):
					if !hasX && labelInsertIdx != -1 {
						result = insertAt(result, labelInsertIdx, sp.allInsert()...)
						hasX = true
					}
					inLabels = false
				}
			}
			result = append(result, line)
		}
		newContent := strings.Join(result, "\n")
		if newContent != content {
			os.WriteFile(path, []byte(newContent), 0644)
		}
		return
	}

	// single service
	if inList(skip, svc) {
		return
	}
	idx := strings.Index(content, "container_name: "+svc)
	if idx < 0 {
		return
	}
	end := len(content)
	if rel := strings.Index(content[idx:], "\n  #"); rel >= 0 {
		end = idx + rel
	}
	block := content[idx:end]
	var newBlock string
	if strings.Contains(block, sp.enableKey) {
		newBlock = subRe.ReplaceAllString(block, repl)
	} else if labelIdx := strings.Index(block, "labels:"); labelIdx >= 0 {
		insertPos := len(block)
		if rel := strings.Index(block[labelIdx+len("labels:"):], "\n"); rel >= 0 {
			insertPos = labelIdx + len("labels:") + rel
		}
		newBlock = block[:insertPos] + sp.singleInsert + block[insertPos:]
	} else {
		newBlock = block
	}
	if newBlock != block {
		os.WriteFile(path, []byte(content[:idx]+newBlock+content[end:]), 0644)
	}
}

// splitLines mirrors Python str.splitlines() for \n (drops a single trailing empty).
func splitLines(s string) []string {
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// insertAt mirrors list.insert for one-or-more items (clamps idx to bounds).
func insertAt(s []string, idx int, items ...string) []string {
	if idx > len(s) {
		idx = len(s)
	}
	if idx < 0 {
		idx = 0
	}
	out := make([]string, 0, len(s)+len(items))
	out = append(out, s[:idx]...)
	out = append(out, items...)
	out = append(out, s[idx:]...)
	return out
}

func inList(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}
