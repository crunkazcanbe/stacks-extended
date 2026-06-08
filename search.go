package main

// search.go — faithful Go port of stacks_search.py.
//
// "stacks regsearch" — a half-screen registry-search TUI.
//   * Concurrently queries ~12 container registries / package indexes.
//   * Enter = `docker pull` the selected image directly (or, with --select,
//     write the chosen image to /tmp/stacks_build_selected and exit).
//
// The Python uses the `curses` library; there is no curses dependency in this
// Go project, so the terminal UI is reproduced with raw ANSI escape sequences
// plus raw-mode keyboard input via golang.org/x/sys/unix (already a dep).
// Behaviour (keys, layout, colors, flow) is kept as close to the original as
// the medium allows.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// ── constants (UA / TIMEOUT / PAGE_SIZE_PER_REGISTRY) ───────────────────────

const searchUA = "Mozilla/5.0 (regsearch/2.0)"
const searchTimeout = 15 * time.Second
const searchPageSizePerRegistry = 100

// searchResult mirrors the dict the Python builds for each repository.
// "_error" is carried as a distinct field rather than a magic dict key.
type searchResult struct {
	Name      string
	Namespace string
	Registry  string
	Pull      string
	Stars     interface{} // may be int/float/string/nil (kept loose like Python)
	Pulls     interface{}
	Desc      string
	URL       string
	Official  bool
	Source    string // "_source": which registry produced this row
	Err       string // "_error": non-empty means this is an error entry
}

// ── http_get_json ───────────────────────────────────────────────────────────

// searchHTTPGetJSON mirrors http_get_json(url, headers, timeout).
// Returns a parsed JSON value (map / slice / scalar) and a non-empty error
// string on failure (the Python returns {"_error": ...}).
func searchHTTPGetJSON(rawurl string, headers map[string]string, timeout time.Duration) (interface{}, string) {
	if timeout == 0 {
		timeout = searchTimeout
	}
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		return nil, err.Error()
	}
	req.Header.Set("User-Agent", searchUA)
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	cl := &http.Client{Timeout: timeout}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err.Error()
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err.Error()
	}
	if strings.TrimSpace(string(body)) == "" {
		return nil, "empty response"
	}
	var out interface{}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Sprintf("invalid JSON: %v", err)
	}
	return out, ""
}

// ── tiny JSON accessors (Python dict .get equivalents) ──────────────────────

func searchAsMap(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return nil
}

func searchAsSlice(v interface{}) []interface{} {
	if s, ok := v.([]interface{}); ok {
		return s
	}
	return nil
}

// searchGetStr does dict.get(key, "") then strips? (no strip unless asked).
func searchGetStr(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// searchGetStrStrip does (dict.get(key) or "").strip().
func searchGetStrStrip(m map[string]interface{}, key string) string {
	return strings.TrimSpace(searchGetStr(m, key))
}

// searchGetAny returns the raw value for a key (for stars/pulls which the
// Python leaves untyped). Returns nil when absent.
func searchGetAny(m map[string]interface{}, key string) interface{} {
	if m == nil {
		return nil
	}
	if v, ok := m[key]; ok {
		return v
	}
	return nil
}

// searchGetNested does dict.get(outer, {}).get(inner, "").
func searchGetNested(m map[string]interface{}, outer, inner string) string {
	return searchGetStr(searchAsMap(searchGetAny(m, outer)), inner)
}

func searchErrResult(msg string) searchResult { return searchResult{Err: msg} }

// ── per-registry searchers ──────────────────────────────────────────────────

func searchDockerHub(keyword string, page, limit int) []searchResult {
	out := []searchResult{}
	seen := map[string]bool{}
	for p := 1; p <= 3; p++ {
		u := fmt.Sprintf("https://hub.docker.com/v2/search/repositories/?query=%s&page_size=100&page=%d",
			url.QueryEscape(keyword), p)
		data, errs := searchHTTPGetJSON(u, nil, searchTimeout)
		if errs != "" {
			break
		}
		m := searchAsMap(data)
		results := searchAsSlice(searchGetAny(m, "results"))
		if len(results) == 0 {
			break
		}
		for _, ri := range results {
			r := searchAsMap(ri)
			ns := searchGetStr(r, "repo_owner")
			if ns == "" {
				ns = "library"
			}
			name := searchGetStr(r, "repo_name")
			key := ns + "/" + name
			if seen[key] {
				continue
			}
			seen[key] = true
			pull := name
			if ns != "library" {
				pull = ns + "/" + name
			}
			out = append(out, searchResult{
				Name: name, Namespace: ns, Registry: "docker.io",
				Pull:  pull,
				Stars: searchGetAny(r, "star_count"), Pulls: searchGetAny(r, "pull_count"),
				Desc: searchGetStrStrip(r, "short_description"),
				URL:  fmt.Sprintf("https://hub.docker.com/r/%s/%s", ns, name),
			})
		}
		if len(results) < 100 {
			break // last page
		}
	}
	return out
}

func searchGhcr(keyword string, page, limit int) []searchResult {
	u := fmt.Sprintf("https://api.github.com/search/repositories?q=%s&per_page=%d&page=%d&sort=stars",
		url.QueryEscape(keyword), limit, page)
	data, errs := searchHTTPGetJSON(u, map[string]string{
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
	}, searchTimeout)
	if errs != "" {
		return []searchResult{searchErrResult(errs)}
	}
	m := searchAsMap(data)
	out := []searchResult{}
	for _, ri := range searchAsSlice(searchGetAny(m, "items")) {
		r := searchAsMap(ri)
		owner := searchGetNested(r, "owner", "login")
		name := searchGetStr(r, "name")
		if owner == "" || name == "" {
			continue
		}
		out = append(out, searchResult{
			Name: name, Namespace: owner, Registry: "ghcr.io",
			Pull:  strings.ToLower(fmt.Sprintf("ghcr.io/%s/%s", owner, name)),
			Stars: searchGetAny(r, "stargazers_count"),
			Pulls: searchGetAny(r, "watchers_count"),
			Desc:  searchGetStrStrip(r, "description"),
			URL:   searchGetStr(r, "html_url"),
		})
	}
	return out
}

func searchQuay(keyword string, page, limit int) []searchResult {
	u := fmt.Sprintf("https://quay.io/api/v1/find/repositories?query=%s&page=%d&includeUsage=true",
		url.QueryEscape(keyword), page)
	data, errs := searchHTTPGetJSON(u, nil, searchTimeout)
	if errs != "" {
		return []searchResult{searchErrResult(errs)}
	}
	m := searchAsMap(data)
	out := []searchResult{}
	results := searchAsSlice(searchGetAny(m, "results"))
	if len(results) > limit {
		results = results[:limit]
	}
	for _, ri := range results {
		r := searchAsMap(ri)
		var nsName string
		nsVal := searchGetAny(r, "namespace")
		if nsMap := searchAsMap(nsVal); nsMap != nil {
			nsName = searchGetStr(nsMap, "name")
		} else if s, ok := nsVal.(string); ok {
			nsName = s
		}
		name := searchGetStr(r, "name")
		pull := name
		urlStr := ""
		if nsName != "" {
			pull = fmt.Sprintf("quay.io/%s/%s", nsName, name)
			urlStr = fmt.Sprintf("https://quay.io/repository/%s/%s", nsName, name)
		}
		out = append(out, searchResult{
			Name: name, Namespace: nsName, Registry: "quay.io",
			Pull:  pull,
			Stars: searchGetAny(r, "popularity"), Pulls: searchGetAny(r, "usage_count"),
			Desc: searchGetStrStrip(r, "description"),
			URL:  urlStr,
		})
	}
	return out
}

func searchLscr(keyword string, page, limit int) []searchResult {
	u := fmt.Sprintf("https://hub.docker.com/v2/repositories/linuxserver/?page_size=100&page=%d", page)
	data, errs := searchHTTPGetJSON(u, nil, searchTimeout)
	if errs != "" {
		return []searchResult{searchErrResult(errs)}
	}
	m := searchAsMap(data)
	kw := strings.ToLower(keyword)
	out := []searchResult{}
	for _, ri := range searchAsSlice(searchGetAny(m, "results")) {
		r := searchAsMap(ri)
		name := searchGetStr(r, "name")
		desc := searchGetStr(r, "description")
		if kw != "" && !strings.Contains(strings.ToLower(name), kw) &&
			!strings.Contains(strings.ToLower(desc), kw) {
			continue
		}
		out = append(out, searchResult{
			Name: name, Namespace: "linuxserver", Registry: "lscr.io",
			Pull:  fmt.Sprintf("lscr.io/linuxserver/%s", name),
			Stars: searchGetAny(r, "star_count"), Pulls: searchGetAny(r, "pull_count"),
			Desc: strings.TrimSpace(desc),
			URL:  fmt.Sprintf("https://hub.docker.com/r/linuxserver/%s", name),
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

func searchBitnami(keyword string, page, limit int) []searchResult {
	u := fmt.Sprintf("https://hub.docker.com/v2/repositories/bitnami/?page_size=%d&page=%d&name=%s",
		limit, page, url.QueryEscape(keyword))
	data, errs := searchHTTPGetJSON(u, nil, searchTimeout)
	if errs != "" {
		return []searchResult{searchErrResult(errs)}
	}
	m := searchAsMap(data)
	out := []searchResult{}
	results := searchAsSlice(searchGetAny(m, "results"))
	if len(results) > limit {
		results = results[:limit]
	}
	for _, ri := range results {
		r := searchAsMap(ri)
		name := searchGetStr(r, "name")
		out = append(out, searchResult{
			Name: name, Namespace: "bitnami", Registry: "docker.io/bitnami",
			Pull:  fmt.Sprintf("bitnami/%s", name),
			Stars: searchGetAny(r, "star_count"), Pulls: searchGetAny(r, "pull_count"),
			Desc: searchGetStrStrip(r, "description"),
			URL:  fmt.Sprintf("https://hub.docker.com/r/bitnami/%s", name),
		})
	}
	return out
}

func searchGitlab(keyword string, page, limit int) []searchResult {
	u := fmt.Sprintf("https://gitlab.com/api/v4/projects?search=%s&per_page=%d&page=%d&order_by=star_count&sort=desc&with_packages=true",
		url.QueryEscape(keyword), limit, page)
	data, errs := searchHTTPGetJSON(u, map[string]string{"Accept": "application/json"}, searchTimeout)
	if errs != "" {
		return []searchResult{searchErrResult(errs)}
	}
	arr := searchAsSlice(data)
	if arr == nil { // not a list -> []
		return []searchResult{}
	}
	if len(arr) > limit {
		arr = arr[:limit]
	}
	out := []searchResult{}
	for _, ri := range arr {
		r := searchAsMap(ri)
		path := searchGetStr(r, "path_with_namespace")
		if path == "" {
			continue
		}
		out = append(out, searchResult{
			Name: searchGetStr(r, "path"), Namespace: searchGetNested(r, "namespace", "path"),
			Registry: "registry.gitlab.com",
			Pull:     fmt.Sprintf("registry.gitlab.com/%s", path),
			Stars:    searchGetAny(r, "star_count"),
			Desc:     searchGetStrStrip(r, "description"),
			URL:      searchGetStr(r, "web_url"),
		})
	}
	return out
}

func searchMcr(keyword string, page, limit int) []searchResult {
	data, errs := searchHTTPGetJSON("https://mcr.microsoft.com/v2/_catalog?n=10000", nil, searchTimeout)
	if errs != "" {
		return []searchResult{searchErrResult(errs)}
	}
	m := searchAsMap(data)
	kw := strings.ToLower(keyword)
	matches := []string{}
	for _, ri := range searchAsSlice(searchGetAny(m, "repositories")) {
		if s, ok := ri.(string); ok {
			if strings.Contains(strings.ToLower(s), kw) {
				matches = append(matches, s)
			}
		}
	}
	start := (page - 1) * limit
	out := []searchResult{}
	if start < 0 {
		start = 0
	}
	for i := start; i < start+limit && i < len(matches); i++ {
		repo := matches[i]
		parts := strings.Split(repo, "/")
		name := parts[len(parts)-1]
		ns := strings.Join(parts[:len(parts)-1], "/")
		out = append(out, searchResult{
			Name:      name,
			Namespace: ns,
			Registry:  "mcr.microsoft.com",
			Pull:      fmt.Sprintf("mcr.microsoft.com/%s", repo),
			Desc:      "Microsoft Container Registry image",
		})
	}
	return out
}

func searchArtifacthub(keyword string, page, limit int) []searchResult {
	offset := (page - 1) * limit
	// kind=12 = container images only (not helm charts)
	u := fmt.Sprintf("https://artifacthub.io/api/v1/packages/search?ts_query_web=%s&limit=%d&offset=%d&kind=12",
		url.QueryEscape(keyword), limit, offset)
	data, errs := searchHTTPGetJSON(u, nil, searchTimeout)
	if errs != "" {
		return []searchResult{searchErrResult(errs)}
	}
	m := searchAsMap(data)
	out := []searchResult{}
	pkgs := searchAsSlice(searchGetAny(m, "packages"))
	if len(pkgs) > limit {
		pkgs = pkgs[:limit]
	}
	for _, ri := range pkgs {
		r := searchAsMap(ri)
		repo := searchAsMap(searchGetAny(r, "repository"))
		kind := searchGetStr(repo, "kind_name")
		if kind == "" {
			kind = "helm"
		}
		name := searchGetStr(r, "name")
		repoName := searchGetStr(repo, "name")
		pull := name
		if repoName != "" {
			pull = fmt.Sprintf("%s/%s", repoName, name)
		}
		out = append(out, searchResult{
			Name: name, Namespace: repoName,
			Registry: fmt.Sprintf("artifacthub:%s", kind),
			Pull:     pull,
			Stars:    searchGetAny(r, "stars"),
			Desc:     searchGetStrStrip(r, "description"),
			URL:      fmt.Sprintf("https://artifacthub.io/packages/%s/%s/%s", kind, repoName, name),
		})
	}
	return out
}

// searchAwsEcr — Docker Hub verified publishers (replaces broken ECR API).
func searchAwsEcr(keyword string, page, limit int) []searchResult {
	u := fmt.Sprintf("https://hub.docker.com/v2/search/repositories/?query=%s&page_size=%d&page=%d&content_types=image&image_filter=store",
		url.QueryEscape(keyword), limit, page)
	data, errs := searchHTTPGetJSON(u, nil, searchTimeout)
	if errs != "" {
		return []searchResult{}
	}
	m := searchAsMap(data)
	out := []searchResult{}
	results := searchAsSlice(searchGetAny(m, "results"))
	if len(results) > limit {
		results = results[:limit]
	}
	for _, ri := range results {
		r := searchAsMap(ri)
		ns := searchGetStr(r, "repo_owner")
		if ns == "" {
			ns = "library"
		}
		name := searchGetStr(r, "repo_name")
		pull := name
		if ns != "library" {
			pull = ns + "/" + name
		}
		out = append(out, searchResult{
			Name: name, Namespace: ns, Registry: "docker.io (verified)",
			Pull:  pull,
			Stars: searchGetAny(r, "star_count"), Pulls: searchGetAny(r, "pull_count"),
			Desc: searchGetStrStrip(r, "short_description"),
		})
	}
	return out
}

// searchForgejo — Codeberg/Forgejo container registry.
func searchForgejo(keyword string, page, limit int) []searchResult {
	u := fmt.Sprintf("https://codeberg.org/api/v1/repos/search?q=%s&limit=%d&page=%d&topic=true",
		url.QueryEscape(keyword), limit, page)
	data, errs := searchHTTPGetJSON(u, map[string]string{"Accept": "application/json"}, searchTimeout)
	if errs != "" {
		return []searchResult{}
	}
	m := searchAsMap(data)
	out := []searchResult{}
	items := searchAsSlice(searchGetAny(m, "data"))
	if len(items) > limit {
		items = items[:limit]
	}
	for _, ri := range items {
		r := searchAsMap(ri)
		owner := searchGetNested(r, "owner", "login")
		name := searchGetStr(r, "name")
		if owner == "" {
			continue
		}
		out = append(out, searchResult{
			Name: name, Namespace: owner, Registry: "codeberg.org",
			Pull:  strings.ToLower(fmt.Sprintf("codeberg.org/%s/%s", owner, name)),
			Stars: searchGetAny(r, "stars_count"),
			Desc:  searchGetStrStrip(r, "description"),
		})
	}
	return out
}

// searchDockerhubOfficial — Docker Hub official images.
func searchDockerhubOfficial(keyword string, page, limit int) []searchResult {
	u := fmt.Sprintf("https://hub.docker.com/v2/search/repositories/?query=%s&page_size=50&page=%d&is_official=1",
		url.QueryEscape(keyword), page)
	data, errs := searchHTTPGetJSON(u, nil, searchTimeout)
	if errs != "" {
		return []searchResult{}
	}
	m := searchAsMap(data)
	out := []searchResult{}
	for _, ri := range searchAsSlice(searchGetAny(m, "results")) {
		r := searchAsMap(ri)
		ns := searchGetStr(r, "repo_owner")
		if ns == "" {
			ns = "library"
		}
		name := searchGetStr(r, "repo_name")
		pull := name
		if ns != "library" {
			pull = fmt.Sprintf("%s/%s", ns, name)
		}
		out = append(out, searchResult{
			Name: name, Namespace: ns, Registry: "docker.io (official)",
			Pull:  pull,
			Stars: searchGetAny(r, "star_count"), Pulls: searchGetAny(r, "pull_count"),
			Desc: searchGetStrStrip(r, "short_description"),
		})
	}
	return out
}

// searchSelfhostedIo — awesome-selfhosted via GitHub.
func searchSelfhostedIo(keyword string, page, limit int) []searchResult {
	u := fmt.Sprintf("https://api.github.com/search/repositories?q=%s+topic:self-hosted&per_page=%d&page=%d&sort=stars",
		url.QueryEscape(keyword), limit, page)
	data, errs := searchHTTPGetJSON(u, map[string]string{"Accept": "application/vnd.github+json"}, searchTimeout)
	if errs != "" {
		return []searchResult{}
	}
	m := searchAsMap(data)
	out := []searchResult{}
	for _, ri := range searchAsSlice(searchGetAny(m, "items")) {
		r := searchAsMap(ri)
		owner := searchGetNested(r, "owner", "login")
		name := searchGetStr(r, "name")
		if owner == "" {
			continue
		}
		out = append(out, searchResult{
			Name: name, Namespace: owner, Registry: "ghcr.io",
			Pull:  strings.ToLower(fmt.Sprintf("ghcr.io/%s/%s", owner, name)),
			Stars: searchGetAny(r, "stargazers_count"),
			Desc:  searchGetStrStrip(r, "description"),
		})
	}
	return out
}

// searchPortainerTemplates — Portainer community templates.
func searchPortainerTemplates(keyword string, page, limit int) []searchResult {
	u := "https://raw.githubusercontent.com/portainer/templates/master/templates-2.0.json"
	data, errs := searchHTTPGetJSON(u, nil, searchTimeout)
	if errs != "" {
		return []searchResult{}
	}
	m := searchAsMap(data)
	out := []searchResult{}
	kw := strings.ToLower(keyword)
	for _, ti := range searchAsSlice(searchGetAny(m, "templates")) {
		t := searchAsMap(ti)
		name := strings.ToLower(searchGetStr(t, "name"))
		title := strings.ToLower(searchGetStr(t, "title"))
		if strings.Contains(name, kw) || strings.Contains(title, kw) {
			image := searchGetStr(t, "image")
			out = append(out, searchResult{
				Name: searchGetStr(t, "name"), Namespace: "",
				Registry: "portainer-templates",
				Pull:     image,
				Desc:     searchGetStrStrip(t, "description"),
			})
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ── REGISTRIES (ordered like the Python dict) ───────────────────────────────

type searchRegistry struct {
	name string
	fn   func(keyword string, page, limit int) []searchResult
}

var searchRegistries = []searchRegistry{
	{"Docker Hub", searchDockerHub},
	{"Hub Official", searchDockerhubOfficial},
	{"GitHub (ghcr.io)", searchGhcr},
	{"Self-Hosted", searchSelfhostedIo},
	{"Quay.io", searchQuay},
	{"GitLab Registry", searchGitlab},
	{"Verified Pub", searchAwsEcr},
	{"Codeberg", searchForgejo},
	{"LinuxServer.io", searchLscr},
	{"Bitnami", searchBitnami},
	{"Microsoft MCR", searchMcr},
	{"ArtifactHub", searchArtifacthub},
}

// searchAll mirrors search_all(): query every registry concurrently.
func searchAll(keyword string, page, limitPerReg int) map[string][]searchResult {
	results := map[string][]searchResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, reg := range searchRegistries {
		wg.Add(1)
		go func(reg searchRegistry) {
			defer wg.Done()
			var res []searchResult
			func() {
				defer func() {
					if r := recover(); r != nil {
						res = []searchResult{searchErrResult(fmt.Sprintf("%v", r))}
					}
				}()
				res = reg.fn(keyword, page, limitPerReg)
			}()
			mu.Lock()
			results[reg.name] = res
			mu.Unlock()
		}(reg)
	}
	wg.Wait()
	return results
}

// ── human_num / trunc ───────────────────────────────────────────────────────

// searchToInt mimics Python int(n) on the loose stars/pulls value.
func searchToInt(n interface{}) (int64, bool) {
	switch v := n.(type) {
	case nil:
		return 0, false
	case int:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		return int64(v), true
	case json.Number:
		i, err := v.Int64()
		if err == nil {
			return i, true
		}
		f, err := v.Float64()
		if err == nil {
			return int64(f), true
		}
		return 0, false
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err == nil {
			return i, true
		}
		return 0, false
	}
	return 0, false
}

// searchPyFloat renders a float64 the way Python's str()/f-string does for a
// float: shortest round-trippable decimal, but always with at least one
// fractional digit (str(2.0) == "2.0", str(1.5) == "1.5", str(15.234) ==
// "15.234"). Go's FormatFloat 'g' drops the trailing ".0", which Python keeps.
func searchPyFloat(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	// If there's no decimal point and no exponent, Python appends ".0".
	if !strings.ContainsAny(s, ".eEnN") { // n/N guards inf/nan spellings
		s += ".0"
	}
	return s
}

// searchRawVal renders a loose stars/pulls value the way Python would print
// the raw JSON scalar (e.g. in show_info's "Stars: {r['stars']}"). Python's
// json decodes whole numbers as int, so a JSON 1500000 prints as "1500000",
// not "1.5e+06" (which is what Go's %v on a float64 would produce).
func searchRawVal(v interface{}) string {
	switch x := v.(type) {
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return searchPyFloat(x)
	case json.Number:
		return x.String()
	case string:
		return x
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

// searchHumanNum mirrors human_num(n).
//
// Python: after the first `n /= 1000`, n becomes a *float*, so `f"{n}{u}"`
// uses float repr (e.g. 1.5K, 2.0K, 15.234K) — NOT an integer. Only the very
// first iteration (when n is still the original int) prints as an int.
func searchHumanNum(n interface{}) string {
	if n == nil {
		return ""
	}
	val, ok := searchToInt(n)
	if !ok {
		// Python: except -> return str(n)
		return fmt.Sprintf("%v", n)
	}
	// First iteration: n is still an int.
	if val < 0 {
		if -val < 1000 {
			return fmt.Sprintf("%d%s", val, "")
		}
	} else if val < 1000 {
		return fmt.Sprintf("%d%s", val, "")
	}
	// Subsequent iterations: n is a float.
	f := float64(val) / 1000
	for _, u := range []string{"K", "M", "B"} {
		var a float64
		if f < 0 {
			a = -f
		} else {
			a = f
		}
		if a < 1000 {
			return searchPyFloat(f) + u
		}
		f /= 1000
	}
	return fmt.Sprintf("%.1fT", f)
}

// searchTrunc mirrors trunc(s, n): collapse whitespace, ellipsize at n.
func searchTrunc(s string, n int) string {
	if s == "" {
		return ""
	}
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n-1 < 0 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// searchStatsTruthy mirrors the Python `if r.get("stars"):` truthiness
// (nil / 0 / "" / "0" are all falsey).
func searchStatsTruthy(v interface{}) bool {
	if v == nil {
		return false
	}
	if s, ok := v.(string); ok {
		return s != "" && s != "0"
	}
	if i, ok := searchToInt(v); ok {
		return i != 0
	}
	return true
}

// ── raw-terminal screen layer (replaces curses) ─────────────────────────────

// searchScreen holds the raw-mode terminal handle and color helpers.
type searchScreen struct {
	in       *os.File
	out      *os.File
	oldState *unix.Termios
}

// color pair → ANSI sequence. Mirrors the curses init_pair() table:
//
//	1 cyan, 2 green, 3 yellow, 4 magenta, 5 red, 6 blue,
//	7 white-on-blue, 8 white-on-default.
func searchColor(pair int, bold bool) string {
	var fg, bg string
	switch pair {
	case 1:
		fg = "36"
	case 2:
		fg = "32"
	case 3:
		fg = "33"
	case 4:
		fg = "35"
	case 5:
		fg = "31"
	case 6:
		fg = "34"
	case 7:
		fg, bg = "37", "44"
	case 8:
		fg = "37"
	default:
		fg = "39"
	}
	seq := "\x1b[" + fg
	if bg != "" {
		seq += ";" + bg
	}
	if bold {
		seq += ";1"
	}
	return seq + "m"
}

const searchReset = "\x1b[0m"

func searchNewScreen() (*searchScreen, error) {
	s := &searchScreen{in: os.Stdin, out: os.Stdout}
	if err := s.enterRaw(); err != nil {
		return nil, err
	}
	s.write("\x1b[?25l") // hide cursor (curses.curs_set(0))
	return s, nil
}

func (s *searchScreen) enterRaw() error {
	fd := int(s.in.Fd())
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return err
	}
	old := *t
	s.oldState = &old
	// cbreak + noecho: disable canonical mode, echo, signals stay (curses cbreak).
	t.Lflag &^= unix.ICANON | unix.ECHO | unix.IEXTEN
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	return unix.IoctlSetTermios(fd, unix.TCSETS, t)
}

func (s *searchScreen) restore() {
	if s.oldState != nil {
		unix.IoctlSetTermios(int(s.in.Fd()), unix.TCSETS, s.oldState)
	}
}

func (s *searchScreen) close() {
	s.write("\x1b[?25h") // show cursor
	s.restore()
}

func (s *searchScreen) write(str string) { s.out.WriteString(str) }

func (s *searchScreen) size() (h, w int) {
	ws, err := unix.IoctlGetWinsize(int(s.out.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Row == 0 || ws.Col == 0 {
		return 24, 80
	}
	return int(ws.Row), int(ws.Col)
}

// addStr writes text at (row, col) — both 0-based, like curses addstr.
func (s *searchScreen) addStr(row, col int, text string) {
	s.write(fmt.Sprintf("\x1b[%d;%dH%s", row+1, col+1, text))
}

// addNStr writes text clipped to n runes at (row,col) with a color attr.
func (s *searchScreen) addNStr(row, col, n int, text, attr string) {
	if n < 0 {
		n = 0
	}
	r := []rune(text)
	if len(r) > n {
		r = r[:n]
	}
	s.addStr(row, col, attr+string(r)+searchReset)
}

func (s *searchScreen) refresh() {} // ANSI writes are unbuffered; no-op

// ── key codes (decoded from raw byte stream) ────────────────────────────────

const (
	skUp = iota + 256
	skDown
	skLeft
	skRight
	skPgUp
	skPgDn
)

// getKey reads one logical key, decoding arrow/page escape sequences.
func (s *searchScreen) getKey() int {
	buf := make([]byte, 1)
	n, err := s.in.Read(buf)
	if err != nil || n == 0 {
		return 27 // treat as ESC
	}
	b := buf[0]
	if b != 0x1b {
		return int(b)
	}
	// Possible escape sequence. Peek the next bytes.
	seq := make([]byte, 2)
	n2, _ := s.in.Read(seq[:1])
	if n2 == 0 {
		return 27 // bare ESC
	}
	if seq[0] != '[' && seq[0] != 'O' {
		return 27
	}
	n3, _ := s.in.Read(seq[1:2])
	if n3 == 0 {
		return 27
	}
	switch seq[1] {
	case 'A':
		return skUp
	case 'B':
		return skDown
	case 'C':
		return skRight
	case 'D':
		return skLeft
	case '5': // PgUp: ESC [ 5 ~
		tmp := make([]byte, 1)
		s.in.Read(tmp)
		return skPgUp
	case '6': // PgDn: ESC [ 6 ~
		tmp := make([]byte, 1)
		s.in.Read(tmp)
		return skPgDn
	}
	return 27
}

// ── ljust / center / rjust style helpers (rune-aware) ───────────────────────

func searchLjust(s string, w int) string {
	r := []rune(s)
	if len(r) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(r))
}

func searchCenter(s string, w int) string {
	r := []rune(s)
	if len(r) >= w {
		return s
	}
	total := w - len(r)
	left := total / 2
	right := total - left
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", right)
}

// searchWrap mirrors textwrap.wrap(text, width) closely enough for display.
func searchWrap(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	lines := []string{}
	cur := ""
	for _, wd := range words {
		if cur == "" {
			cur = wd
			continue
		}
		if len([]rune(cur))+1+len([]rune(wd)) <= width {
			cur += " " + wd
		} else {
			lines = append(lines, cur)
			cur = wd
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// ── docker_pull_inline ──────────────────────────────────────────────────────

// searchDockerPullInline mirrors docker_pull_inline(): stream a real
// `docker pull` in the foreground, then re-enter raw mode.
func searchDockerPullInline(pullCmd string, sc *searchScreen, panelTop, panelH, w int) {
	fullH, _ := sc.size()
	image := strings.TrimSpace(strings.ReplaceAll(pullCmd, "docker pull ", ""))

	// Clear panel.
	for row := panelTop; row < fullH; row++ {
		sc.addStr(row, 0, strings.Repeat(" ", w-1))
	}
	sc.addNStr(panelTop, 0, w-1, searchLjust(fmt.Sprintf(" ⬇  Pulling: %s ", image), w-1),
		searchColor(7, true))
	sc.refresh()

	// Leave raw mode / give the terminal back to docker.
	sc.close()

	fmt.Printf("\n\033[1;35m⬇  docker pull %s\033[0m\n", image)
	cmd := exec.Command("docker", "pull", image)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	ret := 0
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			ret = ee.ExitCode()
		} else {
			ret = 1
		}
	}
	if ret == 0 {
		fmt.Printf("\033[1;32m✔  Pull complete: %s\033[0m\n", image)
	} else {
		fmt.Printf("\033[1;31m✘  Pull failed (exit %d)\033[0m\n", ret)
	}
	fmt.Printf("\n\033[1;36mPress Enter to return to search...\033[0m\n")
	// input() — read a line from stdin (terminal is in cooked mode now).
	rd := make([]byte, 1)
	for {
		_, err := os.Stdin.Read(rd)
		if err != nil || rd[0] == '\n' {
			break
		}
	}

	// Re-init raw mode (mirrors curses.initscr()/restart).
	sc.enterRaw()
	sc.write("\x1b[?25l")
}

// ── TUI ─────────────────────────────────────────────────────────────────────

type searchTUI struct {
	sc             *searchScreen
	keyword        string
	selectMode     bool
	page           int
	results        []searchResult
	filtered       []searchResult
	cursor         int
	scroll         int
	registryFilter string
	statusMsg      string
	loading        bool
}

func newSearchTUI(sc *searchScreen, keyword string, selectMode bool) *searchTUI {
	t := &searchTUI{
		sc:             sc,
		keyword:        keyword,
		selectMode:     selectMode,
		page:           1,
		registryFilter: "ALL",
		statusMsg:      "[/]search  [↑↓]nav  [n/p]page  [f]filter  [i]info  [Enter]PULL  [q]quit",
	}
	if keyword != "" {
		t.doSearch()
	}
	return t
}

func (t *searchTUI) getPanel() (panelTop, panelH, w int) {
	fullH, fullW := t.sc.size()
	return 0, fullH, fullW
}

func (t *searchTUI) doSearch() {
	if t.keyword == "" {
		return
	}
	t.loading = true
	t.draw()
	t.sc.refresh()
	resultsByReg := searchAll(t.keyword, t.page, searchPageSizePerRegistry)
	t.results = []searchResult{}
	// Preserve a deterministic registry order (the original dict order).
	for _, reg := range searchRegistries {
		items, ok := resultsByReg[reg.name]
		if !ok {
			continue
		}
		for _, item := range items {
			if item.Err != "" {
				continue
			}
			item.Source = reg.name
			t.results = append(t.results, item)
		}
	}
	t.applyFilter()
	t.cursor = 0
	t.scroll = 0
	t.loading = false
	t.statusMsg = fmt.Sprintf(
		"[/]search  [↑↓]nav  [n/p]page  [f]filter  [i]info  [Enter]PULL  [q]quit  │  %d results p%d",
		len(t.results), t.page)
}

func (t *searchTUI) applyFilter() {
	if t.registryFilter == "ALL" {
		t.filtered = append([]searchResult(nil), t.results...)
	} else {
		t.filtered = nil
		for _, r := range t.results {
			if r.Source == t.registryFilter {
				t.filtered = append(t.filtered, r)
			}
		}
	}
}

// prompt mirrors the bottom-line text input. initial seeds the first char.
func (t *searchTUI) prompt(promptText, initial string) string {
	fullH, fullW := t.sc.size()
	t.sc.write("\x1b[?25h") // show cursor (echo on)
	t.sc.addStr(fullH-1, 0, strings.Repeat(" ", fullW-1))
	t.sc.addStr(fullH-1, 0, promptText)
	t.sc.refresh()

	// Temporarily go to a line-edit loop in raw mode (we handle echo ourselves).
	buf := []rune(initial)
	if initial != "" {
		t.sc.write(initial)
	}
	for {
		k := t.sc.getKey()
		switch {
		case k == 10 || k == 13: // Enter
			t.sc.write("\x1b[?25l")
			return strings.TrimSpace(string(buf))
		case k == 127 || k == 8: // Backspace
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				t.sc.write("\b \b")
			}
		case k == 27: // ESC -> cancel
			t.sc.write("\x1b[?25l")
			return ""
		case k >= 32 && k < 256:
			buf = append(buf, rune(k))
			t.sc.write(string(rune(k)))
		}
	}
}

func (t *searchTUI) draw() {
	fullH, _ := t.sc.size()
	panelTop, panelH, w := t.getPanel()

	// Clear panel (bottom half).
	for row := panelTop; row < fullH; row++ {
		t.sc.addStr(row, 0, strings.Repeat(" ", w-1))
	}

	// Separator.
	t.sc.addNStr(panelTop, 0, w-1,
		searchLjust(fmt.Sprintf("─── 🔍 regsearch: '%s' │ page %d │ %s ",
			t.keyword, t.page, t.registryFilter), w-1),
		searchColor(3, true))

	if t.loading {
		t.sc.addNStr(panelTop+panelH/2, searchMax(0, (w-16)/2), w-1,
			"  Searching...  ", searchColor(3, true))
		return
	}

	bodyStart := panelTop + 1
	bodyHeight := panelH - 3 // sep + desc + status

	if len(t.filtered) == 0 {
		t.sc.addNStr(bodyStart, 2, w-3, "No results. Type / to search.", searchColor(3, false))
	} else {
		if t.cursor < t.scroll {
			t.scroll = t.cursor
		} else if t.cursor >= t.scroll+bodyHeight {
			t.scroll = t.cursor - bodyHeight + 1
		}
		for i := 0; i < bodyHeight; i++ {
			idx := t.scroll + i
			if idx >= len(t.filtered) {
				break
			}
			r := t.filtered[idx]
			y := bodyStart + i
			selected := idx == t.cursor
			line := " " + r.Pull
			stats := []string{}
			if searchStatsTruthy(r.Stars) {
				stats = append(stats, "★"+searchHumanNum(r.Stars))
			}
			if searchStatsTruthy(r.Pulls) {
				stats = append(stats, "↓"+searchHumanNum(r.Pulls))
			}
			statsStr := strings.Join(stats, " ")
			source := r.Source
			padW := searchMax(20, w-len(statsStr)-len(source)-8)
			full := fmt.Sprintf("%s %s [%s]", searchLjust(line, padW), statsStr, source)
			if selected {
				t.sc.addNStr(y, 0, w-1, searchLjust(full, w-1), searchColor(7, true))
			} else {
				t.sc.addNStr(y, 0, w-1, full, searchColor(1, false))
			}
		}
	}

	// Desc line.
	descY := fullH - 2
	if len(t.filtered) > 0 && t.cursor >= 0 && t.cursor < len(t.filtered) {
		r := t.filtered[t.cursor]
		desc := searchTrunc(r.Desc, w-4)
		t.sc.addNStr(descY, 0, w-1, " "+desc, searchColor(2, false))
	}

	// Status.
	t.sc.addNStr(fullH-1, 0, w-1, searchLjust(t.statusMsg, w-1), searchColor(7, false))
}

func (t *searchTUI) showInfo(r searchResult) {
	fullH, _ := t.sc.size()
	panelTop, panelH, w := t.getPanel()

	fullDesc := r.Desc
	if strings.Contains(r.Registry, "docker.io") || r.Registry == "lscr.io" {
		ns := r.Namespace
		if ns == "" {
			ns = "library"
		}
		u := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/%s/", ns, r.Name)
		data, errs := searchHTTPGetJSON(u, nil, 3*time.Second)
		if errs == "" {
			m := searchAsMap(data)
			if fd := searchGetStr(m, "full_description"); fd != "" {
				fullDesc = fd
			} else if d := searchGetStr(m, "description"); d != "" {
				fullDesc = d
			}
		}
	}

	lines := []string{
		fmt.Sprintf("Name:     %s", r.Name),
		fmt.Sprintf("Registry: %s", r.Registry),
		fmt.Sprintf("Pull:     %s", r.Pull),
	}
	stats := []string{}
	if searchStatsTruthy(r.Stars) {
		stats = append(stats, fmt.Sprintf("Stars: %s", searchRawVal(r.Stars)))
	}
	if searchStatsTruthy(r.Pulls) {
		stats = append(stats, fmt.Sprintf("Pulls: %s", searchRawVal(r.Pulls)))
	}
	if len(stats) > 0 {
		lines = append(lines, fmt.Sprintf("Stats:    %s", strings.Join(stats, ", ")))
	}
	if r.Official {
		lines = append(lines, "Official: Yes")
	}
	if r.URL != "" {
		lines = append(lines, fmt.Sprintf("URL:      %s", r.URL))
	}
	lines = append(lines, "", "Description:", strings.Repeat("─", 20))
	for _, para := range strings.Split(fullDesc, "\n") {
		wrapped := searchWrap(para, w-6)
		if len(wrapped) == 0 {
			lines = append(lines, "")
		} else {
			lines = append(lines, wrapped...)
		}
	}

	scrollIdx := 0
	for {
		for row := panelTop; row < fullH; row++ {
			t.sc.addStr(row, 0, strings.Repeat(" ", w-1))
		}
		t.sc.addNStr(panelTop, 0, w-1, searchCenter(" Image Details ", w-1), searchColor(7, true))
		for i := 0; i < panelH-2; i++ {
			currIdx := scrollIdx + i
			if currIdx < len(lines) {
				ln := lines[currIdx]
				lr := []rune(ln)
				if len(lr) > w-4 {
					lr = lr[:w-4]
				}
				t.sc.addNStr(panelTop+1+i, 2, w-4, string(lr), searchColor(8, false))
			}
		}
		t.sc.addNStr(fullH-1, 0, w-1,
			searchLjust(" [↑/↓] scroll  [q/ESC] close ", w-1), searchColor(7, false))
		t.sc.refresh()
		k := t.sc.getKey()
		switch {
		case k == skUp || k == 'k':
			scrollIdx = searchMax(0, scrollIdx-1)
		case k == skDown || k == 'j':
			scrollIdx = searchMin(searchMax(0, len(lines)-(panelH-2)), scrollIdx+1)
		case k == 'q' || k == 27 || k == 'i':
			return
		}
	}
}

func (t *searchTUI) filterMenu() {
	// opts = ["ALL"] + sorted(set(source for r in results))
	set := map[string]bool{}
	for _, r := range t.results {
		set[r.Source] = true
	}
	uniq := []string{}
	for s := range set {
		uniq = append(uniq, s)
	}
	sort.Strings(uniq)
	opts := append([]string{"ALL"}, uniq...)

	fullH, _ := t.sc.size()
	panelTop, panelH, w := t.getPanel()
	_ = panelH
	sel := 0
	for {
		for row := panelTop; row < fullH; row++ {
			t.sc.addStr(row, 0, strings.Repeat(" ", w-1))
		}
		t.sc.addNStr(panelTop, 0, w-1, searchCenter(" Filter by Registry ", w-1), searchColor(7, true))
		for i, o := range opts {
			y := panelTop + 2 + i
			if y >= fullH-2 {
				break
			}
			if i == sel {
				t.sc.addNStr(y, 2, w-4, searchLjust(fmt.Sprintf("▶ %s", o), w-4), searchColor(7, true))
			} else {
				t.sc.addNStr(y, 2, w-4, fmt.Sprintf("  %s", o), searchColor(1, false))
			}
		}
		t.sc.addNStr(fullH-1, 0, w-1,
			searchLjust(" [↑/↓] navigate  [Enter] select  [q] cancel ", w-1), searchColor(7, false))
		t.sc.refresh()
		k := t.sc.getKey()
		switch {
		case k == skUp || k == 'k':
			sel = (sel - 1 + len(opts)) % len(opts)
		case k == skDown || k == 'j':
			sel = (sel + 1) % len(opts)
		case k == 10 || k == 13:
			t.registryFilter = opts[sel]
			t.applyFilter()
			t.cursor = 0
			t.scroll = 0
			return
		case k == 'q' || k == 27:
			return
		}
	}
}

func (t *searchTUI) run() {
	for {
		t.draw()
		t.sc.refresh()
		k := t.sc.getKey()
		switch {
		case k == 'q' || k == 27:
			return
		case k == '/':
			kw := t.prompt(" Search: ", "")
			if kw != "" {
				t.keyword = kw
				t.page = 1
				t.registryFilter = "ALL"
				t.doSearch()
			}
		case k == 'n' || k == skRight:
			if t.keyword != "" {
				t.page++
				t.doSearch()
			}
		case k == 'p' || k == skLeft:
			if t.keyword != "" && t.page > 1 {
				t.page--
				t.doSearch()
			}
		case k >= '0' && k <= '9':
			pNum := t.prompt(" Go to Page Number: ", string(rune(k)))
			if pNum != "" && searchIsDigits(pNum) {
				targetP, _ := strconv.Atoi(pNum)
				if targetP > 0 {
					t.page = targetP
					t.doSearch()
				}
			}
		case k == 'f':
			if len(t.results) > 0 {
				t.filterMenu()
			}
		case k == 'i':
			if len(t.filtered) > 0 && t.cursor >= 0 && t.cursor < len(t.filtered) {
				t.showInfo(t.filtered[t.cursor])
			}
		case k == skUp || k == 'k':
			if len(t.filtered) > 0 {
				t.cursor = searchMax(0, t.cursor-1)
			}
		case k == skDown || k == 'j':
			if len(t.filtered) > 0 {
				t.cursor = searchMin(len(t.filtered)-1, t.cursor+1)
			}
		case k == skPgUp:
			if len(t.filtered) > 0 {
				t.cursor = searchMax(0, t.cursor-10)
			}
		case k == skPgDn:
			if len(t.filtered) > 0 {
				t.cursor = searchMin(len(t.filtered)-1, t.cursor+10)
			}
		case k == 10 || k == 13:
			// ENTER = docker pull OR select mode.
			if len(t.filtered) > 0 && t.cursor >= 0 && t.cursor < len(t.filtered) {
				r := t.filtered[t.cursor]
				pullCmd := r.Pull
				if pullCmd != "" && !strings.Contains(pullCmd, "helm install") {
					if t.selectMode {
						os.WriteFile("/tmp/stacks_build_selected", []byte(pullCmd), 0o644)
						return
					}
					_, panelH, w := t.getPanel()
					fullH, _ := t.sc.size()
					searchDockerPullInline(
						"docker pull "+pullCmd, t.sc,
						fullH-panelH, panelH, w)
					t.statusMsg = fmt.Sprintf("✔ Pulled: %s  │  [/]search [↑↓]nav [q]quit", pullCmd)
				} else {
					t.statusMsg = fmt.Sprintf("Helm: %s", pullCmd)
				}
			}
		case k == 'g':
			t.cursor = 0
		case k == 'G':
			if len(t.filtered) > 0 {
				t.cursor = len(t.filtered) - 1
			}
		}
	}
}

// ── small numeric helpers (module-unique names) ─────────────────────────────

func searchMax(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func searchMin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func searchIsDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ── entry point (mirrors main() / argparse) ─────────────────────────────────

// cmdRegsearch is the "stacks regsearch [keyword] [--select]" command.
func cmdRegsearch(args []string) {
	keyword := ""
	selectMode := false
	for _, a := range args {
		if a == "--select" {
			selectMode = true
		} else if !strings.HasPrefix(a, "-") && keyword == "" {
			keyword = a
		}
	}

	sc, err := searchNewScreen()
	if err != nil {
		fmt.Fprintln(os.Stderr, "regsearch: cannot init terminal:", err)
		os.Exit(1)
	}
	// curses.wrapper guarantees cleanup even on panic.
	defer sc.close()
	defer func() {
		if r := recover(); r != nil {
			sc.close()
			fmt.Fprintln(os.Stderr, "regsearch:", r)
			os.Exit(1)
		}
	}()
	// Clear the whole screen on start (curses init shows a blank canvas).
	sc.write("\x1b[2J")

	t := newSearchTUI(sc, keyword, selectMode)
	t.run()
}
