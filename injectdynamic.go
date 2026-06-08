package main

// injectdynamic.go — faithful Go port of stacks_inject_dynamic.py.
//
// CLI usage (matches the Python):
//   stacks-inject-dynamic <action> <target> [dyn_dir]
//     action   = "inject" | "strip"
//     target   = "all" / "--all" / a path / a basename (with or without .yml)
//     dyn_dir  = optional override for the Dynamics directory
//
// Injects/strips header & footer ASCII art (from art.conf) around dynamic
// Traefik config files.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// injectDynArt holds the header/footer art loaded from art.conf.
type injectDynArt struct {
	header string
	footer string
}

// injectDynConfPath resolves the path to art.conf. The Python hardcoded
// /home/loveiznothin/.config/stacks/art.conf — replaced with configDir().
func injectDynConfPath() string {
	return filepath.Join(configDir(), "art.conf")
}

// injectDynDefaultDir resolves the default Dynamics directory. The Python
// hardcoded /home/bellzserver/MyDocker/Configs/Dynamics — replaced with a
// universal path under the stacks data root (sibling Configs/Dynamics).
func injectDynDefaultDir() string {
	if d := os.Getenv("STACKS_DYNAMICS_DIR"); d != "" {
		return d
	}
	// stacksDir() is .../MyDocker/Stacks; Dynamics lives at .../MyDocker/Configs/Dynamics
	myDocker := filepath.Dir(stacksDir())
	return filepath.Join(myDocker, "Configs", "Dynamics")
}

// injectDynLoadArt loads header/footer art from art.conf, faithfully mirroring
// the BELLZART_START_/END_ marker parsing in the Python.
func injectDynLoadArt(confPath string) injectDynArt {
	var art injectDynArt
	data, err := os.ReadFile(confPath)
	if err != nil {
		return art
	}
	conf := string(data)
	pairs := []struct {
		key string // "header" / "footer"
	}{
		{"header"},
		{"footer"},
	}
	for _, p := range pairs {
		key := p.key
		sm := "##BELLZART_START_" + strings.ToUpper(key)
		em := "##BELLZART_END_" + strings.ToUpper(key)
		if strings.Contains(conf, sm) && strings.Contains(conf, em) {
			// Faithful to Python: conf.split(sm)[1].split(em)[0].strip("\n")
			// Python's str.split(sm)[1] is the chunk between the FIRST and
			// SECOND occurrence of sm (or after the first if sm occurs once);
			// then .split(em)[0] is everything before the first em in it.
			afterStart := strings.Split(conf, sm)[1]
			between := strings.Split(afterStart, em)[0]
			between = strings.Trim(between, "\n")
			if key == "header" {
				art.header = between
			} else {
				art.footer = between
			}
		}
	}
	return art
}

// injectDynStripFile removes the leading and trailing comment blocks from a
// file (faithful port of strip_file).
func injectDynStripFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := injectDynReadlines(string(data))

	// Remove leading comment block.
	start := 0
	for i, l := range lines {
		if !strings.HasPrefix(l, "#") && strings.TrimSpace(l) != "" {
			start = i
			break
		}
	}
	// Remove trailing comment block.
	end := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.HasPrefix(lines[i], "#") && strings.TrimSpace(lines[i]) != "" {
			end = i + 1
			break
		}
	}
	if start > len(lines) {
		start = len(lines)
	}
	if end < start {
		end = start
	}
	result := lines[start:end]
	return os.WriteFile(path, []byte(strings.Join(result, "")), 0o644)
}

// injectDynInjectFile strips then prepends header art and appends footer art
// (faithful port of inject_file).
func injectDynInjectFile(path string, art injectDynArt) error {
	if err := injectDynStripFile(path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	content := string(data)
	result := ""
	if art.header != "" {
		result += art.header + "\n"
	}
	result += content
	if art.footer != "" {
		result = strings.TrimRight(result, "\n") + "\n" + art.footer + "\n"
	}
	return os.WriteFile(path, []byte(result), 0o644)
}

// injectDynReadlines mimics Python's file.readlines(): it splits keeping the
// trailing "\n" on each line, so re-joining is loss-less.
func injectDynReadlines(s string) []string {
	if s == "" {
		return nil
	}
	var lines []string
	for {
		idx := strings.IndexByte(s, '\n')
		if idx < 0 {
			lines = append(lines, s)
			break
		}
		lines = append(lines, s[:idx+1])
		s = s[idx+1:]
	}
	return lines
}

// isFile reports whether path exists and is a regular file.
func injectDynIsFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// runInjectDynamic is the entry point equivalent to the Python __main__ body.
// args are the positional arguments after the program name:
//
//	args[0] = action, args[1] = target, args[2] = dyn_dir (optional)
func runInjectDynamic(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: inject-dynamic <inject|strip> <target> [dyn_dir]")
		return 1
	}
	action := args[0] // inject or strip
	target := args[1] // all or specific file
	dynDir := injectDynDefaultDir()
	if len(args) > 2 && args[2] != "" {
		dynDir = args[2]
	}

	art := injectDynLoadArt(injectDynConfPath())

	// Build file list.
	var files []string
	switch {
	case target == "all" || target == "--all":
		entries, err := os.ReadDir(dynDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Not found: %s\n", target)
			return 1
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml") {
				files = append(files, filepath.Join(dynDir, name))
			}
		}
	case filepath.IsAbs(target) && injectDynIsFile(target):
		files = []string{target}
	case injectDynIsFile(filepath.Join(dynDir, target)):
		files = []string{filepath.Join(dynDir, target)}
	case injectDynIsFile(filepath.Join(dynDir, target+".yml")):
		files = []string{filepath.Join(dynDir, target+".yml")}
	default:
		fmt.Fprintf(os.Stderr, "Not found: %s\n", target)
		return 1
	}

	for _, f := range files {
		var err error
		if action == "strip" {
			err = injectDynStripFile(f)
		} else {
			err = injectDynInjectFile(f, art)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error processing %s: %v\n", f, err)
			return 1
		}
		fmt.Println(filepath.Base(f))
	}
	return 0
}
