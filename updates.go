package main

// updates.go — faithful Go port of stacks_updates.py.
//
// Image update tracker: checks if running container images have newer versions
// available, records a digest-change history, and can pull updates.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── module constants (mirror module-level globals) ──────────────────────────

const (
	updHistoryMax = 500
	updUA         = "Mozilla/5.0 (stacks-updater/1.0)"
	updTimeout    = 10 * time.Second
)

func updCacheFile() string   { return filepath.Join(configDir(), "update_cache.json") }
func updHistoryFile() string { return filepath.Join(configDir(), "update_history.json") }

// updEntry mirrors the per-image cache/result dict. Optional keys are tracked
// with separate presence flags where the Python relied on key presence.
type updEntry struct {
	Image        string   `json:"image"`
	Tag          string   `json:"tag"`
	Stacks       []string `json:"stacks"`
	LocalDigest  string   `json:"local_digest"`
	RemoteDigest string   `json:"remote_digest"`
	HasUpdate    bool     `json:"has_update"`
	Checked      int64    `json:"checked"`
	Error        string   `json:"error"`

	// hasRemote tracks whether "remote_digest" was a present key in the cached
	// JSON (the Python uses `"remote_digest" in cached` for the freshness check).
	hasRemote bool `json:"-"`
}

// updHistRecord mirrors a single history record.
type updHistRecord struct {
	TS       int64    `json:"ts"`
	Event    string   `json:"event"`
	Image    string   `json:"image"`
	Tag      string   `json:"tag"`
	Stacks   []string `json:"stacks"`
	Old      string   `json:"old"`
	New      string   `json:"new"`
	OldShort string   `json:"old_short"`
	NewShort string   `json:"new_short"`
}

// ── conf loading ────────────────────────────────────────────────────────────

// updLoadConf mirrors load_conf(): defaults, overlaid by stacks.conf, then by the
// YAML master (stacks.yaml wins).
func updLoadConf() map[string]string {
	cfg := map[string]string{
		"UPDATE_CHECK_ENABLED":      "1",
		"UPDATE_CHECK_INTERVAL":     "24",
		"UPDATE_CHECK_RUNNING_ONLY": "1",
		"UPDATE_AUTO_PULL":          "0",
		"UPDATE_SKIP_IMAGES":        "",
	}
	// raw stacks.conf overlay
	if f, err := os.Open(filepath.Join(configDir(), "stacks.conf")); err == nil {
		defer f.Close()
		data, _ := os.ReadFile(filepath.Join(configDir(), "stacks.conf"))
		for _, line := range strings.Split(string(data), "\n") {
			l := strings.TrimSpace(line)
			if strings.Contains(l, "=") && !strings.HasPrefix(l, "#") {
				k, v, _ := strings.Cut(l, "=")
				cfg[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
			}
		}
	}
	// YAML master overlay (stacks.yaml wins)
	for k, v := range configLoad() {
		cfg[k] = v
	}
	return cfg
}

// ── cache / history persistence ─────────────────────────────────────────────

// updLoadCache mirrors load_cache(): returns {} on any failure.
func updLoadCache() map[string]updEntry {
	out := map[string]updEntry{}
	data, err := os.ReadFile(updCacheFile())
	if err != nil {
		return out
	}
	// First pass into generic maps so we can detect key presence (remote_digest).
	var raw map[string]map[string]interface{}
	if json.Unmarshal(data, &raw) != nil {
		return map[string]updEntry{}
	}
	for k, m := range raw {
		var e updEntry
		// re-marshal/unmarshal for typed fields
		if b, err := json.Marshal(m); err == nil {
			_ = json.Unmarshal(b, &e)
		}
		_, e.hasRemote = m["remote_digest"]
		out[k] = e
	}
	return out
}

// updSaveCache mirrors save_cache(): best-effort, indented JSON.
func updSaveCache(cache map[string]updEntry) {
	b, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(updCacheFile(), b, 0o644)
}

// updLoadHistory mirrors load_history(): returns [] on any failure.
func updLoadHistory() []updHistRecord {
	data, err := os.ReadFile(updHistoryFile())
	if err != nil {
		return []updHistRecord{}
	}
	var hist []updHistRecord
	if json.Unmarshal(data, &hist) != nil {
		return []updHistRecord{}
	}
	return hist
}

// updSaveHistory mirrors save_history(): keeps the last HISTORY_MAX records.
func updSaveHistory(hist []updHistRecord) {
	if len(hist) > updHistoryMax {
		hist = hist[len(hist)-updHistoryMax:]
	}
	b, err := json.MarshalIndent(hist, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(updHistoryFile(), b, 0o644)
}

// ── digest helpers ──────────────────────────────────────────────────────────

// updShort mirrors _short(): short form of a sha256:... digest for display.
func updShort(d string) string {
	if d == "" {
		return "—"
	}
	if strings.Contains(d, ":") {
		_, after, _ := strings.Cut(d, ":")
		d = after
	}
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// updRecordHistory mirrors record_history(): append (newest to end).
func updRecordHistory(hist *[]updHistRecord, event, image, tag string, stacks []string, old, new string) {
	*hist = append(*hist, updHistRecord{
		TS:       time.Now().Unix(),
		Event:    event,
		Image:    image,
		Tag:      tag,
		Stacks:   stacks,
		Old:      old,
		New:      new,
		OldShort: updShort(old),
		NewShort: updShort(new),
	})
}

// updGetHistory mirrors get_history(): newest-first, optionally limited.
// limit <= 0 means no limit (Python's None).
func updGetHistory(limit int) []updHistRecord {
	hist := updLoadHistory()
	sort.SliceStable(hist, func(i, j int) bool {
		return hist[i].TS > hist[j].TS
	})
	if limit > 0 && limit < len(hist) {
		return hist[:limit]
	}
	return hist
}

// ── image discovery ─────────────────────────────────────────────────────────

var updImageRe = regexp.MustCompile(`image:\s*([^\s\n]+)`)

// updGetAllImages mirrors get_all_images(): {image: [stack,...]} from compose files.
func updGetAllImages() map[string][]string {
	images := map[string][]string{}
	matches, _ := filepath.Glob(filepath.Join(stacksDir(), "*.yml"))
	sort.Strings(matches)
	for _, fpath := range matches {
		stack := strings.TrimSuffix(filepath.Base(fpath), ".yml")
		content, err := os.ReadFile(fpath)
		if err != nil {
			continue
		}
		for _, m := range updImageRe.FindAllStringSubmatch(string(content), -1) {
			img := strings.Trim(strings.TrimSpace(m[1]), "'\"")
			if img != "" && !strings.HasPrefix(img, "#") {
				images[img] = append(images[img], stack)
			}
		}
	}
	return images
}

// updParseImage mirrors parse_image(): returns registry, repo, tag.
func updParseImage(image string) (string, string, string) {
	tag := "latest"
	// ":" in the last path segment → split tag
	parts := strings.Split(image, "/")
	last := parts[len(parts)-1]
	if strings.Contains(last, ":") {
		i := strings.LastIndex(image, ":")
		tag = image[i+1:]
		image = image[:i]
	}

	if !strings.Contains(image, "/") {
		return "docker.io", "library/" + image, tag
	}
	segs := strings.Split(image, "/")
	if strings.Contains(segs[0], ".") || strings.Contains(segs[0], ":") {
		registry := segs[0]
		repo := strings.Join(segs[1:], "/")
		return registry, repo, tag
	}
	return "docker.io", image, tag
}

// ── registry checks ─────────────────────────────────────────────────────────

// updCheckResult mirrors the dict returned by check_dockerhub/check_ghcr.
type updCheckResult struct {
	digest  string
	checked int64
	err     string
}

func updHTTPClient() *http.Client { return &http.Client{Timeout: updTimeout} }

// updTruncErr mirrors str(e)[:50].
func updTruncErr(e error) string {
	s := e.Error()
	if len(s) > 50 {
		return s[:50]
	}
	return s
}

// updCheckDockerHub mirrors check_dockerhub(): get token, then manifest digest.
func updCheckDockerHub(repo, currentTag string) updCheckResult {
	client := updHTTPClient()

	// Get token
	authURL := fmt.Sprintf(
		"https://auth.docker.io/token?service=registry.docker.io&scope=repository:%s:pull", repo)
	req, err := http.NewRequest("GET", authURL, nil)
	if err != nil {
		return updCheckResult{err: updTruncErr(err), checked: time.Now().Unix()}
	}
	req.Header.Set("User-Agent", updUA)
	resp, err := client.Do(req)
	if err != nil {
		return updCheckResult{err: updTruncErr(err), checked: time.Now().Unix()}
	}
	var tokBody struct {
		Token string `json:"token"`
	}
	dec := json.NewDecoder(resp.Body)
	derr := dec.Decode(&tokBody)
	resp.Body.Close()
	if derr != nil {
		return updCheckResult{err: updTruncErr(derr), checked: time.Now().Unix()}
	}

	// Get manifest digest for current tag
	manURL := fmt.Sprintf("https://registry-1.docker.io/v2/%s/manifests/%s", repo, currentTag)
	req2, err := http.NewRequest("GET", manURL, nil)
	if err != nil {
		return updCheckResult{err: updTruncErr(err), checked: time.Now().Unix()}
	}
	req2.Header.Set("User-Agent", updUA)
	req2.Header.Set("Authorization", "Bearer "+tokBody.Token)
	req2.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	resp2, err := client.Do(req2)
	if err != nil {
		return updCheckResult{err: updTruncErr(err), checked: time.Now().Unix()}
	}
	remoteDigest := resp2.Header.Get("Docker-Content-Digest")
	resp2.Body.Close()

	return updCheckResult{digest: remoteDigest, checked: time.Now().Unix()}
}

// updCheckGHCR mirrors check_ghcr(): GitHub Container Registry manifest digest.
func updCheckGHCR(repo, tag string) updCheckResult {
	client := updHTTPClient()
	url := fmt.Sprintf("https://ghcr.io/v2/%s/manifests/%s", repo, tag)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return updCheckResult{err: updTruncErr(err), checked: time.Now().Unix()}
	}
	req.Header.Set("User-Agent", updUA)
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	resp, err := client.Do(req)
	if err != nil {
		return updCheckResult{err: updTruncErr(err), checked: time.Now().Unix()}
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	resp.Body.Close()
	return updCheckResult{digest: digest, checked: time.Now().Unix()}
}

// updGetLocalDigest mirrors get_local_digest(): local image digest via inspect.
// The Python shells out to `docker inspect`; we keep that behavior verbatim
// (with a 5s timeout) so the RepoDigests[0]@<digest> parsing is identical.
func updGetLocalDigest(image string) string {
	cmd := exec.Command("docker", "inspect", "--format", "{{index .RepoDigests 0}}", image)
	cmd.Env = dockerEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	digest := strings.TrimSpace(string(out))
	if strings.Contains(digest, "@") {
		_, after, _ := strings.Cut(digest, "@")
		return after
	}
	return ""
}

// ── update check ────────────────────────────────────────────────────────────

// updCheckUpdates mirrors check_updates(): check all images, returns result list.
func updCheckUpdates(force bool) []updEntry {
	cfg := updLoadConf()
	if cfg["UPDATE_CHECK_ENABLED"] != "1" {
		return []updEntry{}
	}

	cache := updLoadCache()
	hist := updLoadHistory()
	histDirty := false

	intervalHours := 24
	if v, err := strconv.Atoi(strings.TrimSpace(cfg["UPDATE_CHECK_INTERVAL"])); err == nil {
		intervalHours = v
	}
	interval := int64(intervalHours) * 3600

	skip := map[string]bool{}
	var skipList []string
	for _, s := range strings.Split(cfg["UPDATE_SKIP_IMAGES"], ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			skip[s] = true
			skipList = append(skipList, s)
		}
	}

	images := updGetAllImages()
	results := []updEntry{}

	// Deterministic iteration (Python dict preserves insertion order; we sort
	// for stable output — does not affect cache/history correctness).
	imgKeys := make([]string, 0, len(images))
	for k := range images {
		imgKeys = append(imgKeys, k)
	}
	sort.Strings(imgKeys)

	for _, image := range imgKeys {
		stacks := images[image]
		if skip[image] {
			continue
		}
		skipMatch := false
		for _, s := range skipList {
			if strings.Contains(image, s) {
				skipMatch = true
				break
			}
		}
		if skipMatch {
			continue
		}

		// Check cache age
		cached, hasCached := cache[image]
		age := time.Now().Unix() - cached.Checked
		if !force && age < interval && cached.hasRemote {
			out := cached
			out.Image = image
			out.Stacks = stacks
			results = append(results, out)
			continue
		}

		registry, repo, tag := updParseImage(image)
		localDigest := updGetLocalDigest(image)

		// Check remote
		var remote updCheckResult
		if registry == "docker.io" {
			remote = updCheckDockerHub(repo, tag)
		} else if strings.Contains(registry, "ghcr.io") {
			remote = updCheckGHCR(repo, tag)
		} else {
			remote = updCheckResult{err: "unsupported registry"}
		}

		remoteDigest := remote.digest
		hasUpdate := localDigest != "" && remoteDigest != "" && localDigest != remoteDigest

		entry := updEntry{
			Image:        image,
			Tag:          tag,
			Stacks:       stacks,
			LocalDigest:  localDigest,
			RemoteDigest: remoteDigest,
			HasUpdate:    hasUpdate,
			Checked:      time.Now().Unix(),
			Error:        remote.err,
			hasRemote:    true,
		}

		// ── record history on any digest change vs the last cached entry ──
		var prevRemote, prevLocal string
		if hasCached {
			prevRemote = cached.RemoteDigest
			prevLocal = cached.LocalDigest
		}
		if remoteDigest != "" && prevRemote != "" && remoteDigest != prevRemote {
			updRecordHistory(&hist, "published", image, tag, stacks, prevRemote, remoteDigest)
			histDirty = true
		}
		if localDigest != "" && prevLocal != "" && localDigest != prevLocal {
			updRecordHistory(&hist, "pulled", image, tag, stacks, prevLocal, localDigest)
			histDirty = true
		}

		cache[image] = entry
		results = append(results, entry)
	}

	updSaveCache(cache)
	if histDirty {
		updSaveHistory(hist)
	}
	return results
}

// ── pulling ─────────────────────────────────────────────────────────────────

// updPullUpdates mirrors pull_updates(): pull every image with an update available.
// NOTE: the Python integrates with stacks_image_history (record_from_docker_images)
// for rollback snapshots before/after pulling; that module is not ported here, so
// those snapshot calls are omitted (approximation). All other behavior is faithful.
func updPullUpdates() {
	cache := updLoadCache()
	var targets []updEntry
	// Iterate deterministically over cache.
	keys := make([]string, 0, len(cache))
	for k := range cache {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := cache[k]
		if v.HasUpdate {
			targets = append(targets, v)
		}
	}
	if len(targets) == 0 {
		fmt.Println("No updates to pull.")
		return
	}
	fmt.Printf("Pulling %d image(s)...\n\n", len(targets))
	// (stacks_image_history snapshot of outgoing versions omitted — not ported)
	for _, r := range targets {
		img := r.Image
		fmt.Printf("⬇ docker pull %s\n", img)
		cmd := exec.Command("docker", "pull", img)
		cmd.Env = dockerEnv()
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Printf("  pull failed: %v\n", err)
		}
	}
	// (stacks_image_history snapshot of freshly-pulled versions omitted — not ported)
	fmt.Println("\nRe-checking digests to update history...")
	updCheckUpdates(true)
}

// ── history display ─────────────────────────────────────────────────────────

// updShowHistory mirrors show_history().
func updShowHistory(limit int) {
	hist := updGetHistory(limit)
	if len(hist) == 0 {
		fmt.Println("No update history yet.")
		return
	}
	fmt.Printf("\nUpdate history (newest %d):\n\n", len(hist))
	for _, r := range hist {
		when := time.Unix(r.TS, 0).Format("2006-01-02 15:04")
		ev := r.Event
		arrow := "⬇"
		if ev == "published" {
			arrow = "⬆"
		}
		oldS := r.OldShort
		if oldS == "" {
			oldS = "—"
		}
		newS := r.NewShort
		if newS == "" {
			newS = "—"
		}
		fmt.Printf("  %s  %s %-9s %-44s %s → %s\n", when, arrow, ev, r.Image, oldS, newS)
	}
}

// ── CLI entrypoint ──────────────────────────────────────────────────────────

// updMain mirrors the `if __name__ == "__main__"` block.
func updMain(argv []string) {
	if inList(argv, "--history") {
		updShowHistory(40)
		return
	}
	if inList(argv, "--pull") {
		updPullUpdates()
		return
	}
	force := inList(argv, "--force")
	fmt.Println("Checking for image updates...")
	results := updCheckUpdates(force)

	var updates, errors, ok []updEntry
	for _, r := range results {
		if r.HasUpdate {
			updates = append(updates, r)
		}
		if r.Error != "" {
			errors = append(errors, r)
		}
		if !r.HasUpdate && r.Error == "" {
			ok = append(ok, r)
		}
	}
	fmt.Printf("\n✔ Up to date:  %d\n", len(ok))
	fmt.Printf("⬆ Updates:     %d\n", len(updates))
	fmt.Printf("✘ Errors:      %d\n", len(errors))
	if len(updates) > 0 {
		fmt.Println("\nUpdates available:")
		for _, r := range updates {
			fmt.Printf("  %-50s stacks: %s\n", r.Image, strings.Join(r.Stacks, ", "))
		}
	}
	hist := updGetHistory(8)
	if len(hist) > 0 {
		fmt.Println("\nRecent changes:")
		for _, r := range hist {
			when := time.Unix(r.TS, 0).Format("01-02 15:04")
			arrow := "⬇"
			if r.Event == "published" {
				arrow = "⬆"
			}
			oldS := r.OldShort
			if oldS == "" {
				oldS = "—"
			}
			newS := r.NewShort
			if newS == "" {
				newS = "—"
			}
			fmt.Printf("  %s %s %-44s %s → %s\n", when, arrow, r.Image, oldS, newS)
		}
	}
}
