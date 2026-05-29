// Package manifest implements the PREPARE stage: it maps the origin Drive,
// shares its items with the destination, and writes the folders/manifest CSVs
// later consumed by the apply stage.
package manifest

import (
	"context"
	"encoding/csv"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/WH1-Cloud/migration_script/internal/auth"
	"github.com/WH1-Cloud/migration_script/internal/driveclient"
	"google.golang.org/api/drive/v3"
)

const migrator = "MDRV2"

// Options configures a PREPARE run.
type Options struct {
	PairsCSV      string // input: email_origem,email_destino
	OutFolders    string // output: email_destino,email_origem,pasta_id_correta
	OutManifest   string // output: per-item manifest
	ShareFailures string // optional output CSV
	ReuseFolders  string // optional prior folders CSV for root reuse
	FolderMode    string // create_or_reuse | must_exist
	Role          string // reader | commenter | writer
	Notify        bool
	SleepMS       int
	DryRun        bool
	Attempts      int
}

type runner struct {
	lg    *slog.Logger
	res   *auth.Resolver
	opt   Options
	sleep time.Duration

	fw, mw, sf *csv.Writer
	reuse      map[[2]string]string
	usedRoots  map[string]bool
}

// Run executes the PREPARE stage for every pair in the input CSV.
func Run(ctx context.Context, lg *slog.Logger, res *auth.Resolver, opt Options) error {
	r := &runner{
		lg:        lg,
		res:       res,
		opt:       opt,
		sleep:     time.Duration(max(opt.SleepMS, 0)) * time.Millisecond,
		reuse:     map[[2]string]string{},
		usedRoots: map[string]bool{},
	}
	if err := r.loadReuse(); err != nil {
		return err
	}

	outF, err := os.Create(opt.OutFolders)
	if err != nil {
		return err
	}
	defer func() { _ = outF.Close() }()
	outM, err := os.Create(opt.OutManifest)
	if err != nil {
		return err
	}
	defer func() { _ = outM.Close() }()

	r.fw = csv.NewWriter(outF)
	r.mw = csv.NewWriter(outM)
	defer r.fw.Flush()
	defer r.mw.Flush()
	_ = r.fw.Write([]string{"email_destino", "email_origem", "pasta_id_correta"})
	_ = r.mw.Write([]string{"email_origem", "email_destino", "orig_file_id", "name", "mime", "md5", "mtime", "path"})

	if opt.ShareFailures != "" {
		sfFile, err := os.Create(opt.ShareFailures)
		if err != nil {
			return err
		}
		defer func() { _ = sfFile.Close() }()
		r.sf = csv.NewWriter(sfFile)
		defer r.sf.Flush()
		_ = r.sf.Write([]string{"email_origem", "email_destino", "item_id", "item_name", "is_folder", "where", "reason"})
	}

	pairs, err := readPairs(opt.PairsCSV)
	if err != nil {
		return err
	}
	lg.Info("PREPARE start", "pairs", len(pairs), "dry", opt.DryRun)
	for _, p := range pairs {
		if err := r.processPair(ctx, p.origin, p.dest); err != nil {
			lg.Error("pair failed", "origin", p.origin, "dest", p.dest, "err", err)
		}
	}
	lg.Info("PREPARE done", "folders", opt.OutFolders, "manifest", opt.OutManifest)
	return nil
}

func (r *runner) processPair(ctx context.Context, eo, ed string) error {
	oSvc, err := r.res.GetDriveService(ctx, eo)
	if err != nil {
		return fmt.Errorf("origin drive: %w", err)
	}
	dSvc, err := r.res.GetDriveService(ctx, ed)
	if err != nil {
		return fmt.Errorf("dest drive: %w", err)
	}
	o := driveclient.New(oSvc, r.lg, r.opt.Attempts)
	d := driveclient.New(dSvc, r.lg, r.opt.Attempts)

	rootID, err := r.getOrCreateRoot(ctx, d, eo, ed)
	if err != nil {
		return err
	}
	if r.usedRoots[rootID] {
		// id collision across pairs: make a unique folder
		if r.opt.DryRun {
			rootID = fmt.Sprintf("DRY_COLLISION_%s_%s", eo, ed)
		} else {
			meta, err := d.Create(ctx, &drive.File{
				Name:          fmt.Sprintf("%s (%s-%d)", eo, migrator, time.Now().Unix()),
				MimeType:      driveclient.FolderMime,
				AppProperties: markProps(eo, ed),
			}, "id")
			if err != nil {
				return err
			}
			rootID = meta.Id
		}
	}
	r.usedRoots[rootID] = true
	_ = r.fw.Write([]string{ed, eo, rootID})

	// Share origin's My Drive root children + Computers roots with the destination.
	if err := o.ListAll(ctx, "'root' in parents and trashed=false",
		"nextPageToken, files(id,name,mimeType)", func(f *drive.File) error {
			r.shareItem(ctx, o, f, eo, ed, "mydrive_root_child")
			r.nap()
			return nil
		}); err != nil {
		return err
	}
	comps, err := listComputersRoots(ctx, o)
	if err != nil {
		return err
	}
	for _, c := range comps {
		r.shareItem(ctx, o, c, eo, ed, "computers_root")
		r.nap()
	}

	// Build the manifest: folders (with structure) + loose files + Computers.
	if err := o.ListAll(ctx, "'root' in parents and trashed=false",
		"nextPageToken, files(id,name,mimeType,md5Checksum,modifiedTime)", func(f *drive.File) error {
			if f.MimeType == driveclient.FolderMime {
				return r.walkFolder(ctx, o, f, "", eo, ed)
			}
			r.writeFile(f, "", eo, ed)
			r.nap()
			return nil
		}); err != nil {
		return err
	}
	for _, c := range comps {
		if err := r.walkFolder(ctx, o, c, "", eo, ed); err != nil {
			return err
		}
	}
	return nil
}

// walkFolder records folder at parentPath, then recurses its children.
func (r *runner) walkFolder(ctx context.Context, o *driveclient.Client, folder *drive.File, parentPath, eo, ed string) error {
	_ = r.mw.Write([]string{eo, ed, folder.Id, folder.Name, folder.MimeType, "", "", parentPath})
	full := folder.Name
	if parentPath != "" {
		full = parentPath + "/" + folder.Name
	}
	r.nap()
	return o.ListAll(ctx, fmt.Sprintf("'%s' in parents and trashed=false", folder.Id),
		"nextPageToken, files(id,name,mimeType,md5Checksum,modifiedTime)", func(ch *drive.File) error {
			if ch.MimeType == driveclient.FolderMime {
				return r.walkFolder(ctx, o, ch, full, eo, ed)
			}
			r.writeFile(ch, full, eo, ed)
			return nil
		})
}

func (r *runner) writeFile(f *drive.File, path, eo, ed string) {
	_ = r.mw.Write([]string{eo, ed, f.Id, f.Name, f.MimeType, f.Md5Checksum, f.ModifiedTime, path})
}

func (r *runner) shareItem(ctx context.Context, o *driveclient.Client, f *drive.File, eo, ed, where string) {
	if r.opt.DryRun {
		return
	}
	if _, err := o.Share(ctx, f.Id, ed, r.opt.Role, r.opt.Notify); err != nil {
		r.lg.Info("share fail", "where", where, "name", f.Name, "reason", driveclient.Reason(err))
		if r.sf != nil {
			_ = r.sf.Write([]string{eo, ed, f.Id, f.Name,
				fmt.Sprintf("%t", f.MimeType == driveclient.FolderMime), where, driveclient.Reason(err)})
		}
		return
	}
	r.lg.Info("share ok", "where", where, "name", f.Name, "dest", ed)
}

func (r *runner) getOrCreateRoot(ctx context.Context, d *driveclient.Client, eo, ed string) (string, error) {
	key := [2]string{strings.ToLower(ed), strings.ToLower(eo)}
	if fid, ok := r.reuse[key]; ok {
		if meta, err := d.Get(ctx, fid, "id,mimeType,trashed"); err == nil &&
			meta.MimeType == driveclient.FolderMime && !meta.Trashed {
			if !r.opt.DryRun {
				_ = r.adoptMark(ctx, d, fid, eo, ed)
			}
			r.lg.Info("root reuse", "pair", eo+"->"+ed, "root", fid)
			return fid, nil
		}
		r.lg.Warn("reuse id invalid; falling back", "id", fid)
	}

	if ex := r.findRoot(ctx, d, fmt.Sprintf(
		"'root' in parents and trashed=false and mimeType='%s' and appProperties has { key='src_email' and value='%s' }",
		driveclient.FolderMime, driveclient.EscapeQuery(eo))); ex != nil {
		if !r.opt.DryRun {
			_ = r.adoptMark(ctx, d, ex.Id, eo, ed)
		}
		r.lg.Info("root adopt-by-mark", "pair", eo+"->"+ed, "root", ex.Id)
		return ex.Id, nil
	}
	if ex := r.findRoot(ctx, d, fmt.Sprintf(
		"'root' in parents and trashed=false and mimeType='%s' and name='%s'",
		driveclient.FolderMime, driveclient.EscapeQuery(eo))); ex != nil {
		if !r.opt.DryRun {
			_ = r.adoptMark(ctx, d, ex.Id, eo, ed)
		}
		r.lg.Info("root adopt-by-name", "pair", eo+"->"+ed, "root", ex.Id)
		return ex.Id, nil
	}

	if r.opt.FolderMode == "must_exist" {
		return "", fmt.Errorf("root folder %q missing in dest %s (folder-mode=must_exist)", eo, ed)
	}
	if r.opt.DryRun {
		r.lg.Info("root dry-create", "pair", eo+"->"+ed)
		return "DRY_ROOT_" + eo + "_" + ed, nil
	}
	meta, err := d.Create(ctx, &drive.File{Name: eo, MimeType: driveclient.FolderMime, AppProperties: markProps(eo, ed)}, "id,name")
	if err != nil {
		return "", err
	}
	r.lg.Info("root created", "pair", eo+"->"+ed, "root", meta.Id)
	return meta.Id, nil
}

func (r *runner) findRoot(ctx context.Context, d *driveclient.Client, query string) *drive.File {
	res, err := d.List(ctx, query, "nextPageToken, files(id,name,appProperties)", "")
	if err != nil || len(res.Files) == 0 {
		return nil
	}
	return res.Files[0]
}

func (r *runner) adoptMark(ctx context.Context, d *driveclient.Client, fid, eo, ed string) error {
	_, err := d.Update(ctx, fid, &drive.File{AppProperties: markProps(eo, ed)}, "id,appProperties")
	return err
}

func (r *runner) loadReuse() error {
	if r.opt.ReuseFolders == "" {
		return nil
	}
	f, err := os.Open(r.opt.ReuseFolders)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil || len(rows) == 0 {
		return err
	}
	idx := headerIndex(rows[0])
	for _, row := range rows[1:] {
		ed := strings.ToLower(strings.TrimSpace(get(row, idx, "email_destino")))
		pid := strings.TrimSpace(get(row, idx, "pasta_id_correta"))
		eo := strings.ToLower(strings.TrimSpace(get(row, idx, "email_origem")))
		if ed == "" || pid == "" {
			continue
		}
		r.reuse[[2]string{ed, eo}] = pid
	}
	return nil
}

func (r *runner) nap() {
	if r.sleep > 0 {
		time.Sleep(r.sleep)
	}
}

func markProps(eo, ed string) map[string]string {
	return map[string]string{"src_email": eo, "dest_email": ed, "migrator": migrator}
}

func listComputersRoots(ctx context.Context, o *driveclient.Client) ([]*drive.File, error) {
	var out []*drive.File
	err := o.ListAll(ctx,
		fmt.Sprintf("trashed=false and mimeType='%s' and 'me' in owners and not 'root' in parents", driveclient.FolderMime),
		"nextPageToken, files(id,name,parents)", func(f *drive.File) error {
			if len(f.Parents) == 0 {
				out = append(out, f)
			}
			return nil
		})
	return out, err
}

type pair struct{ origin, dest string }

func readPairs(path string) ([]pair, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("empty pairs CSV")
	}
	idx := headerIndex(rows[0])
	var out []pair
	for _, row := range rows[1:] {
		eo := strings.TrimSpace(get(row, idx, "email_origem"))
		ed := strings.TrimSpace(get(row, idx, "email_destino"))
		if eo == "" || ed == "" {
			continue
		}
		out = append(out, pair{origin: eo, dest: ed})
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

func get(row []string, idx map[string]int, col string) string {
	if i, ok := idx[col]; ok && i < len(row) {
		return row[i]
	}
	return ""
}
