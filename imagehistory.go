// imagehistory.go — faithful Go port of stacks_image_history.py.
//
// Per-image version history + rollback. Keeps a persistent history of every
// distinct image digest we've seen for each `repo:tag` referenced by the stacks,
// so you can roll a container back to an older version. Config:
//
//	IMAGE_HISTORY_ENABLED = 1     # record snapshots
//	IMAGE_HISTORY_KEEP    = 10    # versions kept per image (oldest pruned)
//
// CLI:
//
//	stacks image-history snapshot          # record current digest of every image
//	stacks image-history list <image>      # show recorded versions, newest first
//	stacks image-history rollback <image> <digest>   # pin+retag+(caller recreates)
//	stacks image-history prune             # enforce keep-count on all images
//
// NOTE ON STORAGE: the Python module persists into a SQLite database
// (image_history.db). This Go project has no SQLite driver dependency, so the
// store is reimplemented as a pure-Go JSON-backed table written to the SAME
// path (configDir()/image_history.db). All logic — upsert, last_seen ordering,
// prune-to-keep, COALESCE(image_id) — is preserved faithfully; only the on-disk
// encoding differs from SQLite. This is the one approximation in this port.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── doc string (printed for no-args / unknown subcommands) ────────────────────

const imageHistoryDoc = `
stacks_image_history.py — per-image version history + rollback.

Keeps a SQLite history of every distinct image digest we've seen for each
` + "`repo:tag`" + ` referenced by the stacks, so you can roll a container back to an
older version. Config:
    IMAGE_HISTORY_ENABLED = 1     # record snapshots
    IMAGE_HISTORY_KEEP    = 10    # versions kept per image (oldest pruned)

CLI:
    stacks_image_history.py snapshot          # record current digest of every image
    stacks_image_history.py list <image>      # show recorded versions, newest first
    stacks_image_history.py rollback <image> <digest>   # pin+retag+(caller recreates)
    stacks_image_history.py prune             # enforce keep-count on all images
`

// imageHistoryDBPath mirrors DB_PATH: configDir()/image_history.db.
func imageHistoryDBPath() string {
	return filepath.Join(configDir(), "image_history.db")
}

// ── config helpers ────────────────────────────────────────────────────────────

// imageHistoryConf mirrors load_conf(): a few keys from stacks.conf.
// (The project's confValue already resolves the per-user stacks.conf, which is
// the established Go equivalent for the conf+overlay used elsewhere.)
func imageHistoryConf(key, def string) string {
	v := confValue(key)
	if v == "" {
		return def
	}
	return v
}

// keepCount mirrors keep_count().
func imageHistoryKeepCount() int {
	n, err := strconv.Atoi(strings.TrimSpace(imageHistoryConf("IMAGE_HISTORY_KEEP", "10")))
	if err != nil {
		return 10
	}
	if n < 1 {
		return 1
	}
	return n
}

// enabled mirrors enabled().
func imageHistoryEnabled() bool {
	v := strings.TrimSpace(imageHistoryConf("IMAGE_HISTORY_ENABLED", "1"))
	switch v {
	case "0", "", "false", "no":
		return false
	}
	return true
}

// ── persistent store (faithful stand-in for the SQLite `versions` table) ──────

// imageHistoryRow mirrors a row of the `versions` table.
//
//	PRIMARY KEY (image, digest)
type imageHistoryRow struct {
	Image     string `json:"image"`
	Digest    string `json:"digest"`
	ImageID   string `json:"image_id"`
	FirstSeen int64  `json:"first_seen"`
	LastSeen  int64  `json:"last_seen"`
}

// imageHistoryDB is the in-memory view of the store, loaded/saved as a whole.
type imageHistoryDB struct {
	rows []imageHistoryRow
}

// imageHistoryOpenDB mirrors _db(): ensure dir exists, load (or create) the store.
func imageHistoryOpenDB() *imageHistoryDB {
	_ = os.MkdirAll(configDir(), 0o755)
	db := &imageHistoryDB{}
	data, err := os.ReadFile(imageHistoryDBPath())
	if err == nil && len(data) > 0 {
		var rows []imageHistoryRow
		if json.Unmarshal(data, &rows) == nil {
			db.rows = rows
		}
	}
	return db
}

// commit persists the store back to disk (mirrors con.commit()).
func (db *imageHistoryDB) commit() {
	data, err := json.Marshal(db.rows)
	if err != nil {
		return
	}
	_ = os.WriteFile(imageHistoryDBPath(), data, 0o644)
}

// find returns the index of (image,digest) or -1.
func (db *imageHistoryDB) find(image, digest string) int {
	for i, r := range db.rows {
		if r.Image == image && r.Digest == digest {
			return i
		}
	}
	return -1
}

// ── digest helpers ────────────────────────────────────────────────────────────

// short mirrors short(): short form of a sha256:... digest for display.
func imageHistoryShort(d string) string {
	if d == "" {
		return "—"
	}
	parts := strings.Split(d, ":")
	d = parts[len(parts)-1]
	if len(d) > 12 {
		d = d[:12]
	}
	return d
}

// _inspect mirrors _inspect(): return (digest, image_id) for a locally-present
// image, or ("", "").
func imageHistoryInspect(image string) (string, string) {
	r := cli("inspect", "--format", "{{index .RepoDigests 0}}|{{.Id}}", image)
	if r.exitCode == 0 && strings.TrimSpace(r.stdout) != "" {
		rd, iid, _ := strings.Cut(strings.TrimSpace(r.stdout), "|")
		digest := ""
		if i := strings.Index(rd, "@"); i >= 0 {
			digest = rd[i+1:]
		}
		return strings.TrimSpace(digest), strings.TrimSpace(iid)
	}
	return "", ""
}

// ── record / prune ────────────────────────────────────────────────────────────

// record mirrors record(): upsert the current (or supplied) version for `image`,
// then prune to keep-count. Returns the digest recorded, or "" if nothing.
func imageHistoryRecord(image, digest, imageID string) string {
	if digest == "" {
		digest, imageID = imageHistoryInspect(image)
	}
	if digest == "" {
		return ""
	}
	now := time.Now().Unix()
	db := imageHistoryOpenDB()
	if idx := db.find(image, digest); idx >= 0 {
		db.rows[idx].LastSeen = now
		if imageID != "" { // COALESCE(?, image_id)
			db.rows[idx].ImageID = imageID
		}
	} else {
		db.rows = append(db.rows, imageHistoryRow{
			Image: image, Digest: digest, ImageID: imageID,
			FirstSeen: now, LastSeen: now,
		})
	}
	db.commit()
	imageHistoryPrune(db, image, imageHistoryKeepCount())
	return digest
}

// _prune mirrors _prune(): keep the `keep` newest (by last_seen DESC) per image.
func imageHistoryPrune(db *imageHistoryDB, image string, keep int) {
	// Gather indices for this image, ordered by last_seen DESC.
	var idxs []int
	for i, r := range db.rows {
		if r.Image == image {
			idxs = append(idxs, i)
		}
	}
	sort.SliceStable(idxs, func(a, b int) bool {
		return db.rows[idxs[a]].LastSeen > db.rows[idxs[b]].LastSeen
	})
	if len(idxs) <= keep {
		return
	}
	// Digests of the extras (rows[keep:]).
	extra := map[string]bool{}
	for _, i := range idxs[keep:] {
		extra[db.rows[i].Digest] = true
	}
	kept := db.rows[:0]
	for _, r := range db.rows {
		if r.Image == image && extra[r.Digest] {
			continue
		}
		kept = append(kept, r)
	}
	db.rows = kept
	db.commit()
}

// ── history (list) ────────────────────────────────────────────────────────────

// imageHistoryEntry mirrors a dict returned by history().
type imageHistoryEntry struct {
	Image     string
	Digest    string
	ImageID   string
	FirstSeen int64
	LastSeen  int64
	Short     string
	Current   bool
}

// history mirrors history(): recorded versions for an image, newest-first.
func imageHistoryList(image string) []imageHistoryEntry {
	db := imageHistoryOpenDB()
	var rows []imageHistoryRow
	for _, r := range db.rows {
		if r.Image == image {
			rows = append(rows, r)
		}
	}
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].LastSeen > rows[b].LastSeen })

	curDigest, _ := imageHistoryInspect(image)
	out := make([]imageHistoryEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, imageHistoryEntry{
			Image: image, Digest: r.Digest, ImageID: r.ImageID,
			FirstSeen: r.FirstSeen, LastSeen: r.LastSeen,
			Short: imageHistoryShort(r.Digest), Current: r.Digest == curDigest,
		})
	}
	return out
}

// _repo_no_tag mirrors _repo_no_tag(): strip a trailing :tag from the last path
// segment so we can pin @digest.
func imageHistoryRepoNoTag(image string) string {
	i := strings.LastIndex(image, ":")
	if i < 0 {
		return image
	}
	last := image[i+1:]
	if !strings.Contains(last, "/") {
		return image[:i]
	}
	return image
}

// ── image discovery (faithful port of stacks_updates.get_all_images) ──────────

var imageHistoryImageRe = regexp.MustCompile(`image:\s*([^\s\n]+)`)

// imageHistoryAllImages mirrors stacks_updates.get_all_images(): scan the
// stacks' *.yml files for `image:` references → {image: [stacks...]}.
func imageHistoryAllImages() map[string][]string {
	images := map[string][]string{}
	entries, err := os.ReadDir(stacksDir())
	if err != nil {
		return images
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yml") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	for _, name := range files {
		stack := strings.TrimSuffix(name, ".yml")
		content, err := os.ReadFile(filepath.Join(stacksDir(), name))
		if err != nil {
			continue
		}
		for _, m := range imageHistoryImageRe.FindAllStringSubmatch(string(content), -1) {
			img := strings.Trim(strings.TrimSpace(m[1]), "'\"")
			if img != "" && !strings.HasPrefix(img, "#") {
				images[img] = append(images[img], stack)
			}
		}
	}
	return images
}

// ── snapshot (record_all / record_from_docker_images) ─────────────────────────

// record_all mirrors record_all(): snapshot every referenced image present
// locally (per-image inspect, thorough). Returns (recorded, total).
func imageHistoryRecordAll() (int, int) {
	images := imageHistoryAllImages()
	rec := 0
	for image := range images {
		if imageHistoryRecord(image, "", "") != "" {
			rec++
		}
	}
	return rec, len(images)
}

// record_from_docker_images mirrors record_from_docker_images(): fast snapshot
// via one `docker images --digests` call. Returns (recorded, scanned).
func imageHistoryRecordFromDockerImages(onlyStackImages bool) (int, int) {
	r := cli("images", "--digests", "--format",
		"{{.Repository}}:{{.Tag}}\t{{.Digest}}\t{{.ID}}")
	if r.exitCode != 0 {
		return 0, 0
	}
	var wanted map[string]bool
	if onlyStackImages {
		wanted = map[string]bool{}
		for im := range imageHistoryAllImages() {
			wanted[im] = true
		}
	}
	now := time.Now().Unix()
	rec, scanned := 0, 0
	touched := map[string]bool{}
	db := imageHistoryOpenDB()
	for _, line := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
		if strings.TrimSpace(line) == "" || !strings.Contains(line, "\t") {
			continue
		}
		parts := strings.Split(line, "\t")
		image := strings.TrimSpace(parts[0])
		digest := ""
		if len(parts) > 1 {
			digest = strings.TrimSpace(parts[1])
		}
		iid := ""
		if len(parts) > 2 {
			iid = strings.TrimSpace(parts[2])
		}
		if strings.HasSuffix(image, ":<none>") || digest == "" || digest == "<none>" {
			continue
		}
		if i := strings.Index(digest, "@"); i >= 0 {
			digest = digest[i+1:]
		}
		if wanted != nil && !wanted[image] {
			continue
		}
		scanned++
		if idx := db.find(image, digest); idx >= 0 {
			db.rows[idx].LastSeen = now
			if iid != "" { // COALESCE
				db.rows[idx].ImageID = iid
			}
		} else {
			db.rows = append(db.rows, imageHistoryRow{
				Image: image, Digest: digest, ImageID: iid,
				FirstSeen: now, LastSeen: now,
			})
		}
		touched[image] = true
		rec++
	}
	db.commit()
	keep := imageHistoryKeepCount()
	for im := range touched {
		imageHistoryPrune(db, im, keep)
	}
	return rec, scanned
}

// ── rollback ──────────────────────────────────────────────────────────────────

// rollback mirrors rollback(): pull `image` by `digest` and retag repo:tag to it.
// Caller recreates the container afterward. Returns (ok, message).
func imageHistoryRollback(image, digest string) (bool, string) {
	repo := imageHistoryRepoNoTag(image)
	ref := fmt.Sprintf("%s@%s", repo, digest)
	// 1) make sure the old version is present (fast if layers are cached)
	p := cli("pull", ref)
	if p.exitCode != 0 {
		return false, fmt.Sprintf("pull %s failed: %s", ref, imageHistoryTrim140(p.stderr, p.stdout))
	}
	// 2) point repo:tag at the pinned digest
	t := cli("tag", ref, image)
	if t.exitCode != 0 {
		return false, fmt.Sprintf("tag failed: %s", imageHistoryTrim140(t.stderr, t.stdout))
	}
	imageHistoryRecord(image, digest, "")
	return true, fmt.Sprintf("%s -> %s (recreate to apply)", image, imageHistoryShort(digest))
}

// imageHistoryTrim140 mirrors `(stderr or stdout).strip()[:140]`.
func imageHistoryTrim140(stderr, stdout string) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		s = strings.TrimSpace(stdout)
	}
	if len(s) > 140 {
		s = s[:140]
	}
	return s
}

// ── CLI entry point (mirrors main()) ──────────────────────────────────────────

// imageHistoryMain mirrors main(): args are the subcommand + params (no argv[0]).
func imageHistoryMain(args []string) {
	if len(args) == 0 {
		fmt.Print(imageHistoryDoc)
		return
	}
	cmd := args[0]
	switch {
	case cmd == "snapshot":
		if !imageHistoryEnabled() {
			fmt.Println("image history disabled (IMAGE_HISTORY_ENABLED=0)")
			return
		}
		thorough := inList(args, "--thorough")
		var rec, tot int
		if thorough {
			rec, tot = imageHistoryRecordAll()
		} else {
			rec, tot = imageHistoryRecordFromDockerImages(true)
		}
		fmt.Printf("recorded %d/%d images into %s (keep %d each)\n",
			rec, tot, imageHistoryDBPath(), imageHistoryKeepCount())

	case cmd == "list" && len(args) >= 2:
		for _, v := range imageHistoryList(args[1]) {
			mark := ""
			if v.Current {
				mark = " (current)"
			}
			when := time.Unix(v.LastSeen, 0).Format("2006-01-02 15:04")
			fmt.Printf("  %s  last_seen %s%s\n", v.Short, when, mark)
		}

	case cmd == "rollback" && len(args) >= 3:
		ok, msg := imageHistoryRollback(args[1], args[2])
		prefix := "FAIL: "
		if ok {
			prefix = "OK: "
		}
		fmt.Println(prefix + msg)
		if ok {
			os.Exit(0)
		}
		os.Exit(1)

	case cmd == "prune":
		db := imageHistoryOpenDB()
		seen := map[string]bool{}
		var imgs []string
		for _, r := range db.rows {
			if !seen[r.Image] {
				seen[r.Image] = true
				imgs = append(imgs, r.Image)
			}
		}
		keep := imageHistoryKeepCount()
		for _, im := range imgs {
			imageHistoryPrune(db, im, keep)
		}
		fmt.Printf("pruned to keep %d per image (%d images)\n", keep, len(imgs))

	default:
		fmt.Print(imageHistoryDoc)
	}
}
