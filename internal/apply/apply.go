// Package apply implements the APPLY stage: it recreates the folder tree on the
// destination (preserving structure) and copies every manifest file, with a
// folder cache, bounded concurrency, retry/backoff, idempotency and resume.
package apply

import (
	"context"
	"encoding/csv"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/WH1-Cloud/migration_script/internal/auth"
	"github.com/WH1-Cloud/migration_script/internal/driveclient"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/drive/v3"
)

const migrator = "MDRV2"

// copyFields is the partial-response field mask used on files.copy calls.
const copyFields = "id,parents,name"

// Options configures an APPLY run.
type Options struct {
	FoldersCSV    string
	ManifestCSV   string
	OnlyDest      string
	OnlySrc       string
	Workers       int
	SleepMS       int
	Attempts      int
	UpdateChanged string // "" (full=skip), skip, replace, keep-both
	DryRun        bool
	StateFile     string
}

// Summary is written to summary.json at the end of a run.
type Summary struct {
	Copied   int64  `json:"copied"`
	Replaced int64  `json:"replaced"`
	Skipped  int64  `json:"skipped"`
	Failed   int64  `json:"failed"`
	Duration string `json:"duration"`
}

// Run executes the APPLY stage.
func Run(ctx context.Context, lg *slog.Logger, res *auth.Resolver, opt Options) (*Summary, error) {
	start := time.Now()
	if opt.Workers < 1 {
		opt.Workers = 1
	}

	pair2root, err := loadFolders(opt)
	if err != nil {
		return nil, err
	}
	byDest, err := loadManifest(opt)
	if err != nil {
		return nil, err
	}
	st, err := newState(opt.StateFile)
	if err != nil {
		return nil, err
	}
	defer st.close()

	r := &runner{lg: lg, res: res, opt: opt, state: st}
	sleep := time.Duration(max(opt.SleepMS, 0)) * time.Millisecond

	for ed, bySrc := range byDest {
		dSvc, err := res.GetDriveService(ctx, ed)
		if err != nil {
			lg.Error("dest drive", "dest", ed, "err", err)
			continue
		}
		client := driveclient.New(dSvc, lg, opt.Attempts)
		fc := &folderCache{m: map[string]string{}, c: client, dry: opt.DryRun}

		for eo, rows := range bySrc {
			rootID, ok := pair2root[[2]string{ed, eo}]
			if !ok {
				lg.Warn("skip pair: no root", "pair", eo+"->"+ed)
				continue
			}
			lg.Info("PAIR", "pair", eo+"->"+ed, "root", rootID, "items", len(rows))
			j := &pairJob{c: client, fc: fc, ed: ed, eo: eo, rootID: rootID, rows: rows, sleep: sleep}
			if err := r.processPair(ctx, j); err != nil {
				lg.Error("pair failed", "pair", eo+"->"+ed, "err", err)
			}
		}
	}

	sum := &Summary{
		Copied:   r.copied.Load(),
		Replaced: r.replaced.Load(),
		Skipped:  r.skipped.Load(),
		Failed:   r.failed.Load(),
		Duration: time.Since(start).Round(time.Millisecond).String(),
	}
	lg.Info("APPLY done", "copied", sum.Copied, "replaced", sum.Replaced, "skipped", sum.Skipped, "failed", sum.Failed, "took", sum.Duration)
	return sum, nil
}

type runner struct {
	lg    *slog.Logger
	res   *auth.Resolver
	opt   Options
	state *state

	copied, replaced, skipped, failed atomic.Int64
}

type row struct {
	src, name, mime, md5, mtime, path string
}

// pairJob carries everything needed to migrate one origin→dest pair.
type pairJob struct {
	c      *driveclient.Client
	fc     *folderCache
	ed, eo string
	rootID string
	rows   []row
	sleep  time.Duration
}

func (r *runner) processPair(ctx context.Context, j *pairJob) error {
	folders, files := splitRows(j.rows)
	r.ensureFolders(ctx, j, folders) // folders first, so file parents exist
	return r.copyFiles(ctx, j, files)
}

// ensureFolders creates every folder serially, parents before children.
func (r *runner) ensureFolders(ctx context.Context, j *pairJob, folders []row) {
	sort.SliceStable(folders, func(a, b int) bool {
		return depthOf(folders[a].path) < depthOf(folders[b].path)
	})
	for _, f := range folders {
		full := join(f.path, f.name)
		if _, err := j.fc.ensurePath(ctx, j.rootID, full); err != nil {
			r.lg.Warn("ensure folder failed", "path", full, "err", err)
		}
		r.nap(j.sleep)
	}
}

// copyFiles copies files concurrently (bounded); per-file errors are logged.
func (r *runner) copyFiles(ctx context.Context, j *pairJob, files []row) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(r.opt.Workers)
	for _, f := range files {
		f := f
		g.Go(func() error {
			if gctx.Err() != nil {
				return gctx.Err()
			}
			r.copyFile(gctx, j, f)
			r.nap(j.sleep)
			return nil
		})
	}
	return g.Wait()
}

func (r *runner) nap(d time.Duration) {
	if d > 0 {
		time.Sleep(d)
	}
}

func (r *runner) copyFile(ctx context.Context, j *pairJob, f row) {
	if r.state.done(f.src) {
		r.skipped.Add(1)
		return
	}
	parent, err := j.fc.ensurePath(ctx, j.rootID, f.path)
	if err != nil {
		r.lg.Warn("ensure path failed", "name", f.name, "err", err)
		r.failed.Add(1)
		return
	}
	if existing := r.findExisting(ctx, j.c, parent, f.src); existing != nil && r.handleExisting(ctx, j, f, parent, existing) {
		return
	}
	r.firstCopy(ctx, j, f, parent)
}

// effUpdate returns the effective update mode ("" means full == skip).
func (r *runner) effUpdate() string {
	if eff := strings.ToLower(strings.TrimSpace(r.opt.UpdateChanged)); eff != "" {
		return eff
	}
	return "skip"
}

// handleExisting deals with an already-migrated file; returns true if it took
// final action (skip/replace/keep-both), false to fall through to a fresh copy.
func (r *runner) handleExisting(ctx context.Context, j *pairJob, f row, parent string, existing *drive.File) bool {
	eff := r.effUpdate()
	if !isChanged(f, existing) || eff == "skip" {
		r.lg.Info("SKIP", "name", f.name)
		r.skipped.Add(1)
		r.state.mark(f.src)
		return true
	}
	switch eff {
	case "replace":
		return r.replaceFile(ctx, j, f, parent, existing)
	case "keep-both":
		return r.copyInto(ctx, j, f, parent, "KEEP-BOTH", &r.copied)
	}
	return false
}

func (r *runner) replaceFile(ctx context.Context, j *pairJob, f row, parent string, existing *drive.File) bool {
	if !r.opt.DryRun {
		if _, err := j.c.Copy(ctx, f.src, copyBody(f, parent, j.ed, j.eo), copyFields); err != nil {
			r.lg.Warn("FAIL replace", "name", f.name, "reason", driveclient.Reason(err))
			r.failed.Add(1)
			return true
		}
		if _, err := j.c.Update(ctx, existing.Id, &drive.File{Trashed: true}, "id,trashed", "Trashed"); err != nil {
			r.lg.Warn("trash old failed", "name", f.name, "reason", driveclient.Reason(err))
		}
	}
	r.lg.Info("REPLACE", "name", f.name)
	r.replaced.Add(1)
	r.state.mark(f.src)
	return true
}

// copyInto copies f into parent under the given log label, bumping counter.
func (r *runner) copyInto(ctx context.Context, j *pairJob, f row, parent, label string, counter *atomic.Int64) bool {
	if !r.opt.DryRun {
		if _, err := j.c.Copy(ctx, f.src, copyBody(f, parent, j.ed, j.eo), copyFields); err != nil {
			r.lg.Warn("FAIL "+label, "name", f.name, "reason", driveclient.Reason(err))
			r.failed.Add(1)
			return true
		}
	}
	r.lg.Info(label, "name", f.name)
	counter.Add(1)
	r.state.mark(f.src)
	return true
}

// firstCopy handles the first migration of a src into a parent.
func (r *runner) firstCopy(ctx context.Context, j *pairJob, f row, parent string) {
	if r.opt.DryRun {
		r.lg.Info("DRY COPY", "name", f.name)
		r.copied.Add(1)
		return
	}
	r.copyInto(ctx, j, f, parent, "COPY", &r.copied)
}

func (r *runner) findExisting(ctx context.Context, c *driveclient.Client, parent, src string) *drive.File {
	if r.opt.DryRun || strings.HasPrefix(parent, "DRY_") {
		return nil
	}
	res, err := c.List(ctx, fmt.Sprintf(
		"'%s' in parents and trashed=false and appProperties has { key='src_id' and value='%s' }",
		parent, driveclient.EscapeQuery(src)),
		"nextPageToken, files(id,name,appProperties,md5Checksum,modifiedTime)", "")
	if err != nil || len(res.Files) == 0 {
		return nil
	}
	return res.Files[0]
}

func copyBody(f row, parent, ed, eo string) *drive.File {
	return &drive.File{
		Name:    f.name,
		Parents: []string{parent},
		AppProperties: map[string]string{
			"src_id": f.src, "src_md5": f.md5, "src_mtime": f.mtime,
			"dest_email": ed, "src_email": eo, "migrator": migrator,
		},
	}
}

func isChanged(f row, existing *drive.File) bool {
	exMD5 := existing.AppProperties["src_md5"]
	if exMD5 == "" {
		exMD5 = existing.Md5Checksum
	}
	exMT := existing.AppProperties["src_mtime"]
	if exMT == "" {
		exMT = existing.ModifiedTime
	}
	if f.md5 != "" && exMD5 != "" {
		return f.md5 != exMD5
	}
	if f.md5 == "" && f.mtime != "" && exMT != "" {
		return f.mtime > exMT
	}
	return false
}

// ---------------- folder cache ----------------

type folderCache struct {
	mu  sync.Mutex
	m   map[string]string
	c   *driveclient.Client
	dry bool
}

func (fc *folderCache) ensurePath(ctx context.Context, rootID, rel string) (string, error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	parent := rootID
	acc := ""
	for _, part := range splitPath(rel) {
		if acc == "" {
			acc = part
		} else {
			acc += "/" + part
		}
		key := rootID + "\x00" + acc
		if id, ok := fc.m[key]; ok {
			parent = id
			continue
		}
		id, err := fc.ensureFolderLocked(ctx, parent, part)
		if err != nil {
			return "", err
		}
		fc.m[key] = id
		parent = id
	}
	return parent, nil
}

func (fc *folderCache) ensureFolderLocked(ctx context.Context, parent, name string) (string, error) {
	if fc.dry {
		return fmt.Sprintf("DRY_%d", hash(parent+"\x00"+name)), nil
	}
	if id := fc.find(ctx, parent, name); id != "" {
		return id, nil
	}
	meta, err := fc.c.Create(ctx, &drive.File{
		Name: name, Parents: []string{parent}, MimeType: driveclient.FolderMime,
	}, "id")
	if err != nil {
		return "", err
	}
	return meta.Id, nil
}

func (fc *folderCache) find(ctx context.Context, parent, name string) string {
	q := fmt.Sprintf("'%s' in parents and trashed=false and mimeType='%s' and name='%s'",
		parent, driveclient.FolderMime, driveclient.EscapeQuery(name))
	res, err := fc.c.List(ctx, q, "nextPageToken, files(id,name)", "")
	if err != nil {
		// Some names trip a 400; retry with a "contains" match.
		safe := name
		if len(safe) > 100 {
			safe = safe[:100]
		}
		q = fmt.Sprintf("'%s' in parents and trashed=false and mimeType='%s' and name contains '%s'",
			parent, driveclient.FolderMime, driveclient.EscapeQuery(safe))
		res, err = fc.c.List(ctx, q, "nextPageToken, files(id,name)", "")
		if err != nil {
			return ""
		}
	}
	for _, f := range res.Files {
		if f.Name == name {
			return f.Id
		}
	}
	if len(res.Files) > 0 {
		return res.Files[0].Id
	}
	return ""
}

// ---------------- resume state ----------------

type state struct {
	mu   sync.Mutex
	set  map[string]bool
	file *os.File
}

func newState(path string) (*state, error) {
	s := &state{set: map[string]bool{}}
	if path == "" {
		return s, nil
	}
	if b, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				s.set[line] = true
			}
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s.file = f
	return s, nil
}

func (s *state) done(src string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.set[src]
}

func (s *state) mark(src string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.set[src] {
		return
	}
	s.set[src] = true
	if s.file != nil {
		_, _ = s.file.WriteString(src + "\n")
	}
}

func (s *state) close() {
	if s.file != nil {
		_ = s.file.Close()
	}
}

// ---------------- loaders / helpers ----------------

func loadFolders(opt Options) (map[[2]string]string, error) {
	f, err := os.Open(opt.FoldersCSV)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("empty folders CSV")
	}
	idx := headerIndex(rows[0])
	out := map[[2]string]string{}
	for _, rw := range rows[1:] {
		ed := strings.ToLower(strings.TrimSpace(col(rw, idx, "email_destino")))
		eo := strings.ToLower(strings.TrimSpace(col(rw, idx, "email_origem")))
		pid := strings.TrimSpace(col(rw, idx, "pasta_id_correta"))
		if ed == "" || eo == "" || pid == "" || opt.skip(ed, eo) {
			continue
		}
		key := [2]string{ed, eo}
		if prev, ok := out[key]; ok && prev != pid {
			return nil, fmt.Errorf("conflict: pair %v mapped to two folder ids (%s vs %s)", key, prev, pid)
		}
		out[key] = pid
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no pairs to process (check --only-dest/--only-src)")
	}
	return out, nil
}

func loadManifest(opt Options) (map[string]map[string][]row, error) {
	f, err := os.Open(opt.ManifestCSV)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("empty manifest CSV")
	}
	idx := headerIndex(rows[0])
	out := map[string]map[string][]row{}
	for _, rw := range rows[1:] {
		ed := strings.ToLower(strings.TrimSpace(col(rw, idx, "email_destino")))
		eo := strings.ToLower(strings.TrimSpace(col(rw, idx, "email_origem")))
		if ed == "" || eo == "" || opt.skip(ed, eo) {
			continue
		}
		if out[ed] == nil {
			out[ed] = map[string][]row{}
		}
		out[ed][eo] = append(out[ed][eo], row{
			src:   col(rw, idx, "orig_file_id"),
			name:  col(rw, idx, "name"),
			mime:  col(rw, idx, "mime"),
			md5:   col(rw, idx, "md5"),
			mtime: col(rw, idx, "mtime"),
			path:  col(rw, idx, "path"),
		})
	}
	return out, nil
}

func headerIndex(header []string) map[string]int {
	idx := map[string]int{}
	for i, h := range header {
		idx[strings.ToLower(strings.TrimSpace(h))] = i
	}
	return idx
}

func col(rw []string, idx map[string]int, name string) string {
	if i, ok := idx[name]; ok && i < len(rw) {
		return rw[i]
	}
	return ""
}

// splitRows partitions manifest rows into folders and files.
func splitRows(rows []row) (folders, files []row) {
	for _, x := range rows {
		if x.mime == driveclient.FolderMime {
			folders = append(folders, x)
		} else {
			files = append(files, x)
		}
	}
	return folders, files
}

// skip reports whether a pair is filtered out by --only-dest/--only-src.
func (opt Options) skip(ed, eo string) bool {
	if opt.OnlyDest != "" && ed != strings.ToLower(opt.OnlyDest) {
		return true
	}
	if opt.OnlySrc != "" && eo != strings.ToLower(opt.OnlySrc) {
		return true
	}
	return false
}

func splitPath(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "/" + name
}

func depthOf(p string) int { return len(splitPath(p)) }

func hash(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}
