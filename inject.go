package main

// inject.go — faithful Go port of stacks_inject.py.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// injectArtKey holds one art section's content keyed by section name.
type injectState struct {
	art       map[string]string
	stacksDir string
	confPath  string
	urlConf   string
	mode      string
}

var injectReName = regexp.MustCompile(`^name:`)
var injectReServices = regexp.MustCompile(`^services:`)
var injectReXcaps = regexp.MustCompile(`^x-`)
var injectReNetworks = regexp.MustCompile(`^networks:`)
var injectReVolumes = regexp.MustCompile(`^volumes:`)
var injectReDefaultDir = regexp.MustCompile(`(?m)^DEFAULT_STACKS_DIR=["'](.*)["']`)

// cmdInject is the entry point: argv = [action, target, mode?]
// action = "inject" or "strip"; target = "all"/"--all" or a file path/name;
// mode = "art"/"urls"/"all" (default "all").
func cmdInject(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: inject <inject|strip> <all|file> [art|urls|all]")
		os.Exit(1)
	}
	action := args[0]
	target := args[1]
	mode := "all"
	if len(args) > 2 {
		mode = args[2]
	}

	st := &injectState{
		art:       map[string]string{"header": "", "footer": "", "xcaps": "", "networks": "", "volumes": "", "services": ""},
		stacksDir: stacksDir(),
		confPath:  filepath.Join(configDir(), "art.conf"),
		urlConf:   filepath.Join(configDir(), "stack_urls.conf"),
		mode:      mode,
	}

	if data, err := os.ReadFile(st.confPath); err == nil {
		confContent := string(data)
		if m := injectReDefaultDir.FindStringSubmatch(confContent); m != nil {
			st.stacksDir = m[1]
		}

		for _, pair := range [][2]string{
			{"_ba_header", "header"},
			{"_ba_footer", "footer"},
			{"_ba_xcaps", "xcaps"},
			{"_ba_networks", "networks"},
			{"_ba_volumes", "volumes"},
			{"_ba_services", "services"},
		} {
			key := pair[1]
			startMarker := "##BELLZART_START_" + strings.ToUpper(key)
			endMarker := "##BELLZART_END_" + strings.ToUpper(key)
			if strings.Contains(confContent, startMarker) && strings.Contains(confContent, endMarker) {
				afterStart := strings.SplitN(confContent, startMarker, 2)[1]
				body := strings.SplitN(afterStart, endMarker, 2)[0]
				st.art[key] = strings.Trim(body, "\n")
			}
		}
	}

	// Resolve target file list.
	var files []string
	if target == "--all" || target == "all" {
		if entries, err := os.ReadDir(st.stacksDir); err == nil {
			for _, e := range entries {
				name := e.Name()
				if strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml") {
					files = append(files, filepath.Join(st.stacksDir, name))
				}
			}
		}
	} else if filepath.IsAbs(target) && injectIsFile(target) {
		files = []string{target}
	} else if injectIsFile(filepath.Join(st.stacksDir, target)) {
		files = []string{filepath.Join(st.stacksDir, target)}
	} else if injectIsFile(filepath.Join(st.stacksDir, target+".yml")) {
		files = []string{filepath.Join(st.stacksDir, target+".yml")}
	}

	for _, f := range files {
		if action == "strip" {
			st.stripFile(f)
		} else {
			st.injectFile(f)
		}
	}
}

func injectIsFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// getCustomStackDirectory faithfully ports get_custom_stack_directory.
func (s *injectState) getCustomStackDirectory(filePath string) string {
	if !injectFileExists(s.urlConf) {
		return ""
	}
	base := filepath.Base(filePath)
	stackName := strings.TrimSuffix(base, filepath.Ext(base))

	data, err := os.ReadFile(s.urlConf)
	if err != nil {
		return ""
	}
	// Mirror Python str.splitlines(): split on line boundaries, drop the
	// separators, and produce no trailing empty element. This also strips
	// any '\r' from CRLF endings (Python splitlines does), so lines kept
	// in dir_lines match what Python would append.
	lines := injectSplitLines(string(data))

	targetSection := "[" + stackName + "]"
	inSection := false
	var dirLines []string

	for _, line := range lines {
		sLine := strings.TrimSpace(line)
		if strings.HasPrefix(sLine, "[") && strings.HasSuffix(sLine, "]") {
			if sLine == targetSection {
				inSection = true
				continue
			} else if inSection {
				break
			} else {
				inSection = false
				continue
			}
		}
		if inSection {
			dirLines = append(dirLines, line)
		}
	}

	if len(dirLines) > 0 {
		return strings.Trim(strings.Join(dirLines, "\n"), "\n")
	}
	return ""
}

// stripFile faithfully ports strip_file.
func (s *injectState) stripFile(path string) {
	if !injectFileExists(path) {
		return
	}
	lines := injectReadLines(path)
	var out []string
	skip := false
	for _, l := range lines {
		if strings.Contains(l, "##BELLZART_START") {
			skip = true
			continue
		}
		if strings.Contains(l, "##BELLZART_END") {
			skip = false
			continue
		}
		if !skip {
			out = append(out, l)
		}
	}
	// Also remove large comment blocks (art/URLs = 3+ consecutive # lines)
	var cleaned []string
	i := 0
	for i < len(out) {
		if strings.HasPrefix(strings.TrimSpace(out[i]), "#") {
			var block []string
			for i < len(out) && strings.HasPrefix(strings.TrimSpace(out[i]), "#") {
				block = append(block, out[i])
				i++
			}
			if len(block) < 3 {
				cleaned = append(cleaned, block...)
			}
		} else {
			cleaned = append(cleaned, out[i])
			i++
		}
	}
	injectWriteLines(path, cleaned)
}

// injectFile faithfully ports inject_file.
func (s *injectState) injectFile(path string) {
	if !injectFileExists(path) {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	content := string(data)
	if !strings.Contains(content, "services:") && !strings.Contains(content, "networks:") {
		return
	}
	s.stripFile(path)
	customDirectory := s.getCustomStackDirectory(path)
	lines := injectReadLines(path)
	var out []string
	did := map[string]bool{"header": false, "footer": false, "xcaps": false, "networks": false, "volumes": false, "services": false}

	for _, line := range lines {
		ss := strings.TrimRight(line, " \t\r\n")
		if !did["header"] && injectReName.MatchString(ss) {
			out = append(out, line)
			if s.art["header"] != "" {
				out = append(out, s.art["header"]+"\n")
			}
			if customDirectory != "" && (s.mode == "all" || s.mode == "urls") {
				out = append(out, "\n"+customDirectory+"\n")
			}
			did["header"] = true
			continue
		}
		if !did["header"] && injectReServices.MatchString(ss) {
			if s.art["header"] != "" {
				out = append(out, s.art["header"]+"\n")
			}
			if customDirectory != "" && (s.mode == "all" || s.mode == "urls") {
				out = append(out, "\n"+customDirectory+"\n")
			}
			did["header"] = true
		}
		if !did["xcaps"] && injectReXcaps.MatchString(ss) {
			if s.art["xcaps"] != "" {
				out = append(out, s.art["xcaps"]+"\n")
			}
			did["xcaps"] = true
		}
		if !did["networks"] && injectReNetworks.MatchString(ss) {
			if s.art["networks"] != "" {
				out = append(out, s.art["networks"]+"\n")
			}
			did["networks"] = true
		}
		if !did["volumes"] && injectReVolumes.MatchString(ss) {
			if s.art["volumes"] != "" {
				out = append(out, s.art["volumes"]+"\n")
			}
			did["volumes"] = true
		}
		if !did["services"] && injectReServices.MatchString(ss) {
			if s.art["services"] != "" {
				out = append(out, s.art["services"]+"\n")
			}
			did["services"] = true
		}
		out = append(out, line)
	}
	if s.art["footer"] != "" {
		out = append(out, s.art["footer"]+"\n")
	}
	injectWriteLines(path, out)
}

// injectFileExists mirrors os.path.exists for a file path.
func injectFileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// injectReadLines reads a file as Python readlines() would: each element keeps
// its trailing newline; the final line keeps no newline if the file lacks one.
func injectReadLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if len(data) == 0 {
		return nil
	}
	s := string(data)
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// injectSplitLines mirrors Python str.splitlines(): it splits on \n, \r and
// \r\n boundaries, removes the separators, and yields no trailing empty
// element for a final boundary. An empty input yields no lines.
func injectSplitLines(s string) []string {
	var lines []string
	start := 0
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '\n' {
			lines = append(lines, s[start:i])
			i++
			start = i
		} else if c == '\r' {
			lines = append(lines, s[start:i])
			i++
			if i < len(s) && s[i] == '\n' {
				i++
			}
			start = i
		} else {
			i++
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// injectWriteLines mirrors writelines(): concatenate elements verbatim.
func injectWriteLines(path string, lines []string) {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
	}
	_ = os.WriteFile(path, []byte(b.String()), 0644)
}
