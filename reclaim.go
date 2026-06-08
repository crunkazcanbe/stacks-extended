package main

// reclaim.go — faithful Go port of stacks_reclaim.py.
//
// Reclaim disk by removing UNUSED tagged images, by size. Lists every local
// image largest-first, classifies each as in-use / unused / dangling. Most
// stacks here are on-demand (Sablier) and sit DOWN most of the time, so "no
// running container" does NOT mean unused — an image referenced by any compose
// file is protected (RECLAIM_PROTECT_STACK_IMAGES=1, default on). Removal uses
// `docker rmi` WITHOUT --force so an image still wired to a container can never
// be pulled out from under it; Docker refuses and we skip it.
//
// CLI:
//   stacks reclaim report  [--json] [--all] [--min-size MB]
//   stacks reclaim clean   [--auto] [--dangling] [--dry-run] [--min-size MB]
//                          [--force]            # allow rmi --force (untag only)

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ───────────────────────── config ─────────────────────────

// reclaimLoadConf mirrors load_conf(): legacy stacks.conf overlaid with the
// YAML-derived config. (configLoad already prefers stacks.yaml, falling back to
// stacks.conf, which subsumes both halves of the Python load_conf.)
func reclaimLoadConf() map[string]string {
	cfg := map[string]string{}
	conf := filepath.Join(configDir(), "stacks.conf")
	if f, err := os.Open(conf); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line != "" && !strings.HasPrefix(line, "#") && strings.Contains(line, "=") {
				k, v, _ := strings.Cut(line, "=")
				cfg[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
		f.Close()
	}
	for k, v := range configLoad() {
		cfg[k] = v
	}
	return cfg
}

// reclaimBool mirrors _bool().
func reclaimBool(cfg map[string]string, key, def string) bool {
	v, ok := cfg[key]
	if !ok {
		v = def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "", "false", "no", "off":
		return false
	}
	return true
}

// ───────────────────────── helpers ─────────────────────────

// reclaimHuman mirrors _human(): bytes → human (decimal, matching docker).
// negative sentinel (-1) renders as the em-dash used for None in the Python.
func reclaimHuman(n int64) string {
	if n < 0 {
		return "—"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(n)
	for _, u := range units {
		if f < 1000 || u == "TB" {
			if u == "B" || f >= 100 {
				return fmt.Sprintf("%.0f%s", f, u)
			}
			return fmt.Sprintf("%.1f%s", f, u)
		}
		f /= 1000.0
	}
	return ""
}

var reclaimImageRe = regexp.MustCompile(`(?m)^\s*image:\s*([^\s#\n]+)`)

// stackReferencedImages mirrors stack_referenced_images(): set of image refs
// (repo:tag) named by image: in any compose file.
func stackReferencedImages() map[string]bool {
	refs := map[string]bool{}
	matches, _ := filepath.Glob(filepath.Join(stacksDir(), "*.yml"))
	for _, fpath := range matches {
		data, err := os.ReadFile(fpath)
		if err != nil {
			continue
		}
		for _, m := range reclaimImageRe.FindAllStringSubmatch(string(data), -1) {
			img := strings.Trim(strings.TrimSpace(m[1]), `'"`)
			if img == "" {
				continue
			}
			refs[img] = true
			// bare repo (no tag in the last path segment) → :latest
			seg := img
			if i := strings.LastIndex(img, "/"); i >= 0 {
				seg = img[i+1:]
			}
			if !strings.Contains(seg, ":") {
				refs[img+":latest"] = true
			}
		}
	}
	return refs
}

// containerImageIDs mirrors container_image_ids(): full image IDs every
// container (running OR stopped) is built on, plus the image references those
// containers report (name form).
func containerImageIDs() (ids map[string]bool, names map[string]bool) {
	ids, names = map[string]bool{}, map[string]bool{}
	r := cli("ps", "-a", "--no-trunc", "--format", "{{.ID}}\t{{.Image}}")
	if r.exitCode != 0 {
		return ids, names
	}
	var cids []string
	for _, line := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		cid, img, _ := strings.Cut(line, "\t")
		cids = append(cids, strings.TrimSpace(cid))
		if strings.TrimSpace(img) != "" {
			names[strings.TrimSpace(img)] = true
		}
	}
	// resolve each container to its real image ID (handles name drift / retags)
	for _, cid := range cids {
		ri := cli("inspect", "--format", "{{.Image}}", cid)
		if ri.exitCode == 0 && strings.TrimSpace(ri.stdout) != "" {
			ids[strings.TrimSpace(ri.stdout)] = true
		}
	}
	return ids, names
}

// reclaimImage mirrors a list_images() dict entry. JSON tags reproduce the
// exact key names the Python emitted under `--json`.
type reclaimImage struct {
	ID       string `json:"id"`
	Ref      string `json:"ref"`
	Repo     string `json:"repo"`
	Tag      string `json:"tag"`
	Size     int64  `json:"size"`
	SizeH    string `json:"size_h"`
	Dangling bool   `json:"dangling"`
	Status   string `json:"status"`
	Why      string `json:"why"`
}

// listReclaimImages mirrors list_images(): every local image.
func listReclaimImages() []*reclaimImage {
	r := cli("images", "--no-trunc", "--format",
		"{{.ID}}\t{{.Repository}}\t{{.Tag}}\t{{.Size}}")
	var out []*reclaimImage
	if r.exitCode != 0 {
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 4 {
			continue
		}
		iid := strings.TrimSpace(parts[0])
		repo := strings.TrimSpace(parts[1])
		tag := strings.TrimSpace(parts[2])
		size := strings.TrimSpace(parts[3])
		dangling := repo == "<none>" || tag == "<none>"
		ref := "<none>"
		if !dangling {
			ref = repo + ":" + tag
		}
		out = append(out, &reclaimImage{
			ID: iid, Ref: ref, Repo: repo, Tag: tag,
			Size: parseReclaimSize(size), SizeH: size, Dangling: dangling,
		})
	}
	return out
}

var reclaimSizeRe = regexp.MustCompile(`(?i)^([\d.]+)\s*([KMGT]?B)$`)

// parseReclaimSize mirrors _parse_size(): '4.59GB' / '276MB' / '0B' → bytes.
func parseReclaimSize(s string) int64 {
	m := reclaimSizeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0
	}
	val, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	mult := map[string]float64{"B": 1, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12}
	mul, ok := mult[strings.ToUpper(m[2])]
	if !ok {
		mul = 1
	}
	return int64(val * mul)
}

// reclaimStatusSummary mirrors a per-status summary entry.
type reclaimStatusSummary struct {
	Count int   `json:"count"`
	Bytes int64 `json:"bytes"`
}

// reclaimSummary mirrors the summary dict.
type reclaimSummary struct {
	Total            int                  `json:"total"`
	InUse            reclaimStatusSummary `json:"in-use"`
	Unused           reclaimStatusSummary `json:"unused"`
	Dangling         reclaimStatusSummary `json:"dangling"`
	ReclaimableBytes int64                `json:"reclaimable_bytes"`
}

// classifyReclaim mirrors classify(): rows sorted largest-first, each tagged
// with 'status' in {in-use, unused, dangling}, plus the summary.
func classifyReclaim(minSize int64) ([]*reclaimImage, reclaimSummary) {
	cfg := reclaimLoadConf()
	protectStacks := reclaimBool(cfg, "RECLAIM_PROTECT_STACK_IMAGES", "1")
	stackRefs := map[string]bool{}
	if protectStacks {
		stackRefs = stackReferencedImages()
	}
	usedIDs, usedNames := containerImageIDs()

	rows := []*reclaimImage{}
	for _, img := range listReclaimImages() {
		if img.Size < minSize {
			continue
		}
		switch {
		case img.Dangling:
			img.Status, img.Why = "dangling", "untagged leftover"
		case usedIDs[img.ID]:
			img.Status, img.Why = "in-use", "container"
		case usedNames[img.Ref] || usedNames[img.Repo]:
			img.Status, img.Why = "in-use", "container"
		case protectStacks && (stackRefs[img.Ref] || stackRefs[img.Repo]):
			img.Status, img.Why = "in-use", "stack file"
		default:
			img.Status, img.Why = "unused", "no container, no stack"
		}
		rows = append(rows, img)
	}

	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Size > rows[j].Size })

	summary := reclaimSummary{Total: len(rows)}
	for _, st := range []string{"in-use", "unused", "dangling"} {
		var cnt int
		var by int64
		for _, r := range rows {
			if r.Status == st {
				cnt++
				by += r.Size
			}
		}
		ss := reclaimStatusSummary{Count: cnt, Bytes: by}
		switch st {
		case "in-use":
			summary.InUse = ss
		case "unused":
			summary.Unused = ss
		case "dangling":
			summary.Dangling = ss
		}
	}
	summary.ReclaimableBytes = summary.Unused.Bytes + summary.Dangling.Bytes
	return rows, summary
}

var reclaimDFRe = regexp.MustCompile(`([\d.]+\s*[KMGT]?B)`)

// dockerDFReclaimable mirrors docker_df_reclaimable(): Docker's authoritative
// image reclaimable bytes (accounts for shared layers). Returns -1 for None.
func dockerDFReclaimable() int64 {
	r := cli("system", "df", "--format", "{{.Type}}\t{{.Reclaimable}}")
	if r.exitCode != 0 {
		return -1
	}
	for _, line := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
		if strings.HasPrefix(strings.ToLower(line), "images") {
			parts := strings.Split(line, "\t")
			m := reclaimDFRe.FindString(parts[len(parts)-1])
			if m != "" {
				return parseReclaimSize(m)
			}
		}
	}
	return -1
}

// reclaimError mirrors an (ref, msg) error tuple.
type reclaimError struct {
	Ref string
	Msg string
}

// removeReclaim mirrors remove(): rmi each row; returns (removed, freed, errors).
func removeReclaim(rows []*reclaimImage, force, dryRun bool) (int, int64, []reclaimError) {
	removed := 0
	var freed int64
	var errors []reclaimError
	for _, r := range rows {
		target := r.ID
		if !r.Dangling {
			if r.Ref != "<none>" {
				target = r.Ref
			}
		}
		if dryRun {
			removed++
			freed += r.Size
			continue
		}
		args := []string{"rmi"}
		if force {
			args = append(args, "--force")
		}
		args = append(args, target)
		res := cli(args...)
		if res.exitCode == 0 {
			removed++
			freed += r.Size
		} else {
			msg := res.stderr
			if msg == "" {
				msg = res.stdout
			}
			lines := strings.Split(strings.TrimSpace(msg), "\n")
			last := lines[len(lines)-1]
			if len(last) > 120 {
				last = last[:120]
			}
			errors = append(errors, reclaimError{r.Ref, last})
		}
	}
	return removed, freed, errors
}

// ────────────────────────────── CLI ──────────────────────────────

// reclaimOpts mirrors the dict produced by _parse_flags().
type reclaimOpts struct {
	minSize int64
	flags   map[string]bool
}

// parseReclaimFlags mirrors _parse_flags().
func parseReclaimFlags(args []string) reclaimOpts {
	opts := reclaimOpts{minSize: 0, flags: map[string]bool{}}
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--min-size" && i+1 < len(args) {
			if v, err := strconv.ParseFloat(args[i+1], 64); err == nil {
				opts.minSize = int64(v * 1e6)
			}
			i += 2
			continue
		}
		if strings.HasPrefix(a, "--") {
			opts.flags[a] = true
		}
		i++
	}
	return opts
}

// reclaimTrunc returns s truncated to at most n runes-as-bytes (Python slices on
// characters; refs/repos here are ASCII so byte slicing matches).
func reclaimTrunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// cmdReclaimReport mirrors cmd_report().
func cmdReclaimReport(args []string) {
	opts := parseReclaimFlags(args)
	rows, summ := classifyReclaim(opts.minSize)
	if opts.flags["--json"] {
		// Preserve Python's dict insertion order: summary first, then images.
		payload := struct {
			Summary reclaimSummary  `json:"summary"`
			Images  []*reclaimImage `json:"images"`
		}{Summary: summ, Images: rows}
		b, _ := json.MarshalIndent(payload, "", "  ")
		fmt.Println(string(b))
		return
	}
	showAll := opts.flags["--all"]

	fmt.Printf("\n\033[1;35m🧹 Image disk reclaim\033[0m   (%d images scanned)\n\n", summ.Total)
	fmt.Printf("  \033[1;32min-use\033[0m    %3d   %9s\n", summ.InUse.Count, reclaimHuman(summ.InUse.Bytes))
	fmt.Printf("  \033[1;33munused\033[0m    %3d   %9s\n", summ.Unused.Count, reclaimHuman(summ.Unused.Bytes))
	fmt.Printf("  \033[1;31mdangling\033[0m  %3d   %9s\n", summ.Dangling.Count, reclaimHuman(summ.Dangling.Bytes))
	df := dockerDFReclaimable()
	var stackOnly int64
	for _, r := range rows {
		if r.Status == "in-use" && r.Why == "stack file" {
			stackOnly += r.Size
		}
	}
	fmt.Printf("\n  \033[1mReclaimable now (safe): ~%s\033[0m — unused + dangling, protects stacks\n",
		reclaimHuman(summ.ReclaimableBytes))
	fmt.Printf("  \033[1maggressive: ~%s\033[0m — also drops %s of idle stack images (re-pull on next up)\n",
		reclaimHuman(summ.ReclaimableBytes+stackOnly), reclaimHuman(stackOnly))
	fmt.Printf("  \033[2mdocker reports %s 'reclaimable' overall (counts every stopped-stack image)\033[0m\n\n",
		reclaimHuman(df))

	var cand []*reclaimImage
	for _, r := range rows {
		if r.Status == "unused" || r.Status == "dangling" {
			cand = append(cand, r)
		}
	}
	shown := cand
	if !showAll && len(cand) > 25 {
		shown = cand[:25]
	}
	if len(cand) == 0 {
		fmt.Print("  ✓ Nothing to reclaim — every image is in use.\n")
		return
	}
	fmt.Println("  \033[2mLargest reclaimable images:\033[0m")
	for _, r := range shown {
		col := "\033[1;33m"
		tag := "unused"
		if r.Dangling {
			col = "\033[1;31m"
			tag = "dangling"
		}
		fmt.Printf("    %s%9s\033[0m  %-8s %s\n", col, reclaimHuman(r.Size), tag, reclaimTrunc(r.Ref, 54))
	}
	if !showAll && len(cand) > len(shown) {
		fmt.Printf("    \033[2m… +%d more (use --all)\033[0m\n", len(cand)-len(shown))
	}
	fmt.Println("\n  Reclaim:  stacks reclaim clean            (interactive)")
	fmt.Println("            stacks reclaim clean --auto     (remove all unused+dangling)")
	fmt.Print("            stacks reclaim clean --dangling (only untagged leftovers)\n")
}

// cmdReclaimClean mirrors cmd_clean().
func cmdReclaimClean(args []string) {
	opts := parseReclaimFlags(args)
	rows, _ := classifyReclaim(opts.minSize)
	flags := opts.flags
	danglingOnly := flags["--dangling"]
	aggressive := flags["--aggressive"] || flags["--stacks-too"]
	everything := flags["--everything"] || flags["--nuke"] || flags["--all-images"]
	auto := flags["--auto"]
	dry := flags["--dry-run"]
	force := flags["--force"] || everything

	// ── pick the tier ──────────────────────────────────────
	var cand []*reclaimImage
	var mode string
	switch {
	case everything:
		// NUKE: every image, including ones a container uses (force rmi). Images
		// held by a RUNNING container can't actually be removed and are skipped.
		cand = append(cand, rows...)
		mode = "EVERYTHING (incl. in-use — force)"
	case aggressive:
		// Max space: delete everything NOT tied to a real container, including
		// idle stack images (they re-pull on next 'up').
		for _, r := range rows {
			if r.Why != "container" {
				cand = append(cand, r)
			}
		}
		mode = "aggressive (all but container-bound)"
	case danglingOnly:
		for _, r := range rows {
			if r.Status == "dangling" {
				cand = append(cand, r)
			}
		}
		mode = "dangling only"
	default:
		for _, r := range rows {
			if r.Status == "unused" || r.Status == "dangling" {
				cand = append(cand, r)
			}
		}
		mode = "safe (unused + dangling)"
	}
	if len(cand) == 0 {
		fmt.Println("✓ Nothing to reclaim.")
		return
	}

	var nominal int64
	for _, r := range cand {
		nominal += r.Size
	}
	fmt.Printf("\n\033[1mMode: %s\033[0m\n", mode)
	fmt.Printf("%d image(s) to remove — ~%s nominal.\n", len(cand), reclaimHuman(nominal))
	if everything {
		fmt.Println("\033[1;31m⚠ This removes images your running stacks use — they will re-pull on next start.\033[0m")
	} else if aggressive {
		fmt.Println("\033[1;33m⚠ Idle stack images will be deleted and re-pulled the next time those stacks start.\033[0m")
	}
	if dry {
		for _, r := range cand {
			lbl := r.Status
			if r.Status == "in-use" {
				lbl = "in-use:" + r.Why
			}
			fmt.Printf("  would remove  %9s  %-14s %s\n", reclaimHuman(r.Size), lbl, reclaimTrunc(r.Ref, 50))
		}
		fmt.Printf("\n(dry-run) would remove %d images.\n\n", len(cand))
		return
	}

	if !auto {
		limit := cand
		if len(cand) > 30 {
			limit = cand[:30]
		}
		for _, r := range limit {
			lbl := r.Status
			if r.Status == "in-use" {
				lbl = "in-use:" + r.Why
			}
			fmt.Printf("  %9s  %-14s %s\n", reclaimHuman(r.Size), lbl, reclaimTrunc(r.Ref, 50))
		}
		if len(cand) > 30 {
			fmt.Printf("  … +%d more\n", len(cand)-30)
		}
		prompt := fmt.Sprintf("\nRemove these %d images? [y/N]: ", len(cand))
		if everything {
			prompt = "Type DELETE to confirm: "
		}
		fmt.Print(prompt)
		reader := bufio.NewReader(os.Stdin)
		raw, _ := reader.ReadString('\n')
		ans := strings.TrimSpace(raw)
		if (everything && ans != "DELETE") || (!everything && strings.ToLower(ans) != "y") {
			fmt.Println("Aborted.")
			return
		}
	}

	removed, freed, errors := removeReclaim(cand, force, false)
	fmt.Printf("\n✓ Removed %d/%d images (~%s nominal).\n", removed, len(cand), reclaimHuman(freed))
	if len(errors) > 0 {
		fmt.Printf("  %d could not be removed (still referenced — skipped):\n", len(errors))
		shown := errors
		if len(errors) > 8 {
			shown = errors[:8]
		}
		for _, e := range shown {
			fmt.Printf("    • %s: %s\n", e.Ref, e.Msg)
		}
		if len(errors) > 8 {
			fmt.Printf("    … +%d more\n", len(errors)-8)
		}
	}
	df := dockerDFReclaimable()
	if df >= 0 {
		fmt.Printf("  Docker still reports %s reclaimable.\n\n", reclaimHuman(df))
	}
}

// cmdReclaim mirrors main(): route the sub-command word.
func cmdReclaim(args []string) {
	cmd := "report"
	if len(args) > 0 {
		cmd = args[0]
	}
	var rest []string
	if len(args) > 1 {
		rest = args[1:]
	}
	switch cmd {
	case "report":
		cmdReclaimReport(rest)
	case "clean":
		cmdReclaimClean(rest)
	default:
		fmt.Print(reclaimDoc)
	}
}

// reclaimDoc mirrors the module docstring printed for an unknown sub-command.
// Python's __doc__ begins with a newline (after the opening triple-quote) and
// ends with a newline (before the closing triple-quote); reproduce both so the
// output of `print(__doc__)` matches byte-for-byte.
const reclaimDoc = `
stacks_reclaim.py — reclaim disk by removing UNUSED tagged images, by size.

Lists every local image largest-first, classifies each as:
  • in-use     — a container (running OR stopped) is built on it, OR it is
                 referenced by image: in a stack compose file
  • unused     — tagged, but no container uses it and no stack references it
  • dangling   — untagged <none>:<none> leftovers (always safe to remove)

CRITICAL SAFETY: most stacks here are on-demand (Sablier) and sit DOWN most of
the time, so "no running container" does NOT mean unused. An image referenced by
any compose file is protected (config RECLAIM_PROTECT_STACK_IMAGES=1, default on).
Removal uses ` + "`docker rmi`" + ` WITHOUT --force, so an image still wired to any
container can never be pulled out from under it — Docker refuses and we skip it.

CLI:
    stacks_reclaim.py report  [--json] [--all] [--min-size MB]
    stacks_reclaim.py clean   [--auto] [--dangling] [--dry-run] [--min-size MB]
                              [--force]            # allow rmi --force (untag only)
`
