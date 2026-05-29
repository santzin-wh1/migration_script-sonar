package manifest

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/WH1-Cloud/migration_script/internal/auth"
	"github.com/WH1-Cloud/migration_script/internal/driveclient"
	"google.golang.org/api/drive/v3"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeDrive is a programmable driveAPI for tests.
type fakeDrive struct {
	listAllFn func(query string, yield func(*drive.File) error) error
	listFn    func(query string) (*drive.FileList, error)
	getFn     func(id string) (*drive.File, error)
	createFn  func() (*drive.File, error)
	updateFn  func(id string) (*drive.File, error)
	shareFn   func(id string) (*drive.Permission, error)

	creates, updates, shares int
}

func (f *fakeDrive) List(_ context.Context, query, _, _ string) (*drive.FileList, error) {
	if f.listFn != nil {
		return f.listFn(query)
	}
	return &drive.FileList{}, nil
}

func (f *fakeDrive) ListAll(_ context.Context, query, _ string, yield func(*drive.File) error) error {
	if f.listAllFn != nil {
		return f.listAllFn(query, yield)
	}
	return nil
}

func (f *fakeDrive) Get(_ context.Context, id, _ string) (*drive.File, error) {
	if f.getFn != nil {
		return f.getFn(id)
	}
	return &drive.File{Id: id, MimeType: driveclient.FolderMime}, nil
}

func (f *fakeDrive) Create(_ context.Context, _ *drive.File, _ string) (*drive.File, error) {
	f.creates++
	if f.createFn != nil {
		return f.createFn()
	}
	return &drive.File{Id: "created"}, nil
}

func (f *fakeDrive) Update(_ context.Context, id string, _ *drive.File, _ string, _ ...string) (*drive.File, error) {
	f.updates++
	if f.updateFn != nil {
		return f.updateFn(id)
	}
	return &drive.File{Id: id}, nil
}

func (f *fakeDrive) Share(_ context.Context, id, _, _ string, _ bool) (*drive.Permission, error) {
	f.shares++
	if f.shareFn != nil {
		return f.shareFn(id)
	}
	return &drive.Permission{Id: "p"}, nil
}

func newRunner(opt Options) (*runner, *bytes.Buffer) {
	var mb bytes.Buffer
	r := &runner{lg: discard(), opt: opt, reuse: map[[2]string]string{}, usedRoots: map[string]bool{}}
	r.fw = csv.NewWriter(io.Discard)
	r.mw = csv.NewWriter(&mb)
	return r, &mb
}

func TestGetOrCreateRootReuse(t *testing.T) {
	fd := &fakeDrive{getFn: func(id string) (*drive.File, error) {
		return &drive.File{Id: id, MimeType: driveclient.FolderMime}, nil
	}}
	r, _ := newRunner(Options{})
	r.reuse[[2]string{"d@x", "o@x"}] = "REUSED"
	id, err := r.getOrCreateRoot(context.Background(), fd, "o@x", "d@x")
	if err != nil || id != "REUSED" {
		t.Fatalf("id=%s err=%v", id, err)
	}
}

func TestGetOrCreateRootAdoptByMark(t *testing.T) {
	fd := &fakeDrive{listFn: func(q string) (*drive.FileList, error) {
		if strings.Contains(q, "appProperties has") {
			return &drive.FileList{Files: []*drive.File{{Id: "MARKED"}}}, nil
		}
		return &drive.FileList{}, nil
	}}
	r, _ := newRunner(Options{})
	id, err := r.getOrCreateRoot(context.Background(), fd, "o@x", "d@x")
	if err != nil || id != "MARKED" {
		t.Fatalf("id=%s err=%v", id, err)
	}
}

func TestGetOrCreateRootCreate(t *testing.T) {
	fd := &fakeDrive{createFn: func() (*drive.File, error) { return &drive.File{Id: "NEW"}, nil }}
	r, _ := newRunner(Options{})
	id, err := r.getOrCreateRoot(context.Background(), fd, "o@x", "d@x")
	if err != nil || id != "NEW" || fd.creates != 1 {
		t.Fatalf("id=%s err=%v creates=%d", id, err, fd.creates)
	}
}

func TestGetOrCreateRootMustExist(t *testing.T) {
	fd := &fakeDrive{}
	r, _ := newRunner(Options{FolderMode: "must_exist"})
	if _, err := r.getOrCreateRoot(context.Background(), fd, "o@x", "d@x"); err == nil {
		t.Fatal("expected must_exist error")
	}
}

func TestGetOrCreateRootDry(t *testing.T) {
	fd := &fakeDrive{}
	r, _ := newRunner(Options{DryRun: true})
	id, err := r.getOrCreateRoot(context.Background(), fd, "o@x", "d@x")
	if err != nil || !strings.HasPrefix(id, "DRY_ROOT") || fd.creates != 0 {
		t.Fatalf("id=%s err=%v creates=%d", id, err, fd.creates)
	}
}

func TestResolveRootCollision(t *testing.T) {
	n := 0
	fd := &fakeDrive{createFn: func() (*drive.File, error) {
		n++
		return &drive.File{Id: "ID" + strconv.Itoa(n)}, nil
	}}
	r, _ := newRunner(Options{})
	r.usedRoots["ID1"] = true // first create collides with a prior pair's root
	id, err := r.resolveRoot(context.Background(), fd, "o@x", "d@x")
	if err != nil || id != "ID2" {
		t.Fatalf("expected unique ID2, got %s err=%v", id, err)
	}
}

func TestBuildManifest(t *testing.T) {
	fd := &fakeDrive{listAllFn: func(query string, yield func(*drive.File) error) error {
		if strings.Contains(query, "'root' in parents") {
			_ = yield(&drive.File{Id: "dir1", Name: "Docs", MimeType: driveclient.FolderMime})
			return yield(&drive.File{Id: "f1", Name: "loose.txt", MimeType: "text/plain", Md5Checksum: "m"})
		}
		// children of Docs
		return yield(&drive.File{Id: "f2", Name: "inner.txt", MimeType: "text/plain"})
	}}
	r, mb := newRunner(Options{})
	if err := r.buildManifest(context.Background(), fd, "o@x", "d@x", nil); err != nil {
		t.Fatal(err)
	}
	r.mw.Flush()
	out := mb.String()
	for _, want := range []string{"dir1,Docs", "f1,loose.txt", "f2,inner.txt", "Docs"} {
		if !strings.Contains(out, want) {
			t.Fatalf("manifest missing %q:\n%s", want, out)
		}
	}
}

func TestShareAll(t *testing.T) {
	fd := &fakeDrive{listAllFn: func(_ string, yield func(*drive.File) error) error {
		return yield(&drive.File{Id: "i1", Name: "x"})
	}}
	r, _ := newRunner(Options{Role: "writer"})
	comps := []*drive.File{{Id: "c1", Name: "Comp"}}
	if err := r.shareAll(context.Background(), fd, "o@x", "d@x", comps); err != nil {
		t.Fatal(err)
	}
	if fd.shares != 2 { // 1 root child + 1 computers root
		t.Fatalf("shares=%d, want 2", fd.shares)
	}
}

func TestRunWrapperNoPairs(t *testing.T) {
	dir := t.TempDir()
	pairs := filepath.Join(dir, "pairs.csv")
	mustWrite(t, pairs, "email_origem,email_destino\n") // header only, 0 pairs

	err := Run(context.Background(), discard(), auth.NewResolver(auth.Config{}), Options{
		PairsCSV:      pairs,
		OutFolders:    filepath.Join(dir, "out_folders.csv"),
		OutManifest:   filepath.Join(dir, "manifest.csv"),
		ShareFailures: filepath.Join(dir, "fails.csv"),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestShareItemFailureRecorded(t *testing.T) {
	fd := &fakeDrive{shareFn: func(_ string) (*drive.Permission, error) { return nil, errors.New("nope") }}
	r, _ := newRunner(Options{Role: "writer"})
	var sb bytes.Buffer
	r.sf = csv.NewWriter(&sb)

	r.shareItem(context.Background(), fd, &drive.File{Id: "i", Name: "n"}, "o@x", "d@x", "where")
	r.sf.Flush()

	if sb.Len() == 0 {
		t.Fatal("expected a share-failure row to be recorded")
	}
}

func TestProcessPair(t *testing.T) {
	fd := &fakeDrive{listAllFn: func(query string, yield func(*drive.File) error) error {
		if strings.Contains(query, "'root' in parents") {
			return yield(&drive.File{Id: "f1", Name: "a.txt", MimeType: "text/plain"})
		}
		return nil // no computers roots, no folder children
	}}
	r, _ := newRunner(Options{})
	r.getClient = func(_ context.Context, _ string) (driveAPI, error) { return fd, nil }

	if err := r.processPair(context.Background(), "o@x", "d@x"); err != nil {
		t.Fatal(err)
	}
	if fd.creates != 1 { // created the destination root
		t.Fatalf("creates=%d, want 1", fd.creates)
	}
	if fd.shares == 0 {
		t.Fatal("expected at least one share")
	}
}

func TestReadPairs(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pairs.csv")
	mustWrite(t, p, "email_origem,email_destino\no@gmail.com,d@x\n,skipme\n")
	pairs, err := readPairs(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || pairs[0].origin != "o@gmail.com" || pairs[0].dest != "d@x" {
		t.Fatalf("pairs=%+v", pairs)
	}
}

func TestLoadReuse(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "reuse.csv")
	mustWrite(t, p, "email_destino,email_origem,pasta_id_correta\nd@x,o@x,RID\n")
	r, _ := newRunner(Options{ReuseFolders: p})
	if err := r.loadReuse(); err != nil {
		t.Fatal(err)
	}
	if r.reuse[[2]string{"d@x", "o@x"}] != "RID" {
		t.Fatalf("reuse=%v", r.reuse)
	}
}

func TestHelpers(t *testing.T) {
	idx := headerIndex([]string{" Email_Destino ", "X"})
	if idx["email_destino"] != 0 {
		t.Fatalf("headerIndex=%v", idx)
	}
	if get([]string{"a", "b"}, idx, "x") != "b" {
		t.Fatal("get mismatch")
	}
	if markProps("o", "d")["migrator"] != migrator {
		t.Fatal("markProps mismatch")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
