package apply

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/WH1-Cloud/migration_script/internal/auth"
	"github.com/WH1-Cloud/migration_script/internal/driveclient"
	"google.golang.org/api/drive/v3"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeDrive is a programmable driveAPI for tests.
type fakeDrive struct {
	listFn   func(query string) (*drive.FileList, error)
	createFn func(f *drive.File) (*drive.File, error)
	copyFn   func(srcID string, f *drive.File) (*drive.File, error)
	updateFn func(id string, f *drive.File) (*drive.File, error)

	lists, creates, copies, updates int
}

func (f *fakeDrive) List(_ context.Context, query, _, _ string) (*drive.FileList, error) {
	f.lists++
	if f.listFn != nil {
		return f.listFn(query)
	}
	return &drive.FileList{}, nil
}

func (f *fakeDrive) Create(_ context.Context, _ *drive.File, _ string) (*drive.File, error) {
	f.creates++
	if f.createFn != nil {
		return f.createFn(nil)
	}
	return &drive.File{Id: "created-" + itoa(f.creates)}, nil
}

func (f *fakeDrive) Copy(_ context.Context, srcID string, file *drive.File, _ string) (*drive.File, error) {
	f.copies++
	if f.copyFn != nil {
		return f.copyFn(srcID, file)
	}
	return &drive.File{Id: "copy-" + srcID}, nil
}

func (f *fakeDrive) Update(_ context.Context, id string, _ *drive.File, _ string, _ ...string) (*drive.File, error) {
	f.updates++
	if f.updateFn != nil {
		return f.updateFn(id, nil)
	}
	return &drive.File{Id: id}, nil
}

func itoa(n int) string { return strconv.Itoa(n) }

func newRunner(t *testing.T, opt Options) *runner {
	t.Helper()
	st, err := newState("")
	if err != nil {
		t.Fatal(err)
	}
	return &runner{lg: discard(), opt: opt, state: st}
}

func TestCopyFileFirstCopy(t *testing.T) {
	fd := &fakeDrive{}
	r := newRunner(t, Options{})
	j := &pairJob{c: fd, fc: &folderCache{m: map[string]string{}, c: fd}, ed: "d@x", eo: "o@gmail.com", rootID: "root"}

	r.copyFile(context.Background(), j, row{src: "s1", name: "a.txt"})

	if fd.copies != 1 || r.copied.Load() != 1 {
		t.Fatalf("copies=%d copied=%d", fd.copies, r.copied.Load())
	}
	if !r.state.done("s1") {
		t.Fatal("expected s1 marked done")
	}
}

func TestCopyFileSkipUnchanged(t *testing.T) {
	fd := &fakeDrive{listFn: func(q string) (*drive.FileList, error) {
		if strings.Contains(q, "src_id") {
			return &drive.FileList{Files: []*drive.File{{Id: "ex", AppProperties: map[string]string{"src_md5": "m1"}}}}, nil
		}
		return &drive.FileList{}, nil
	}}
	r := newRunner(t, Options{}) // UpdateChanged "" => skip
	j := &pairJob{c: fd, fc: &folderCache{m: map[string]string{}, c: fd}, rootID: "root"}

	r.copyFile(context.Background(), j, row{src: "s1", name: "a.txt", md5: "m1"})

	if fd.copies != 0 || r.skipped.Load() != 1 {
		t.Fatalf("copies=%d skipped=%d", fd.copies, r.skipped.Load())
	}
}

func TestCopyFileReplace(t *testing.T) {
	fd := &fakeDrive{listFn: func(q string) (*drive.FileList, error) {
		if strings.Contains(q, "src_id") {
			return &drive.FileList{Files: []*drive.File{{Id: "ex", AppProperties: map[string]string{"src_md5": "old"}}}}, nil
		}
		return &drive.FileList{}, nil
	}}
	r := newRunner(t, Options{UpdateChanged: "replace"})
	j := &pairJob{c: fd, fc: &folderCache{m: map[string]string{}, c: fd}, rootID: "root"}

	r.copyFile(context.Background(), j, row{src: "s1", name: "a.txt", md5: "new"})

	if fd.copies != 1 || fd.updates != 1 || r.replaced.Load() != 1 {
		t.Fatalf("copies=%d updates=%d replaced=%d", fd.copies, fd.updates, r.replaced.Load())
	}
}

func TestCopyFileKeepBoth(t *testing.T) {
	fd := &fakeDrive{listFn: func(q string) (*drive.FileList, error) {
		if strings.Contains(q, "src_id") {
			return &drive.FileList{Files: []*drive.File{{Id: "ex", AppProperties: map[string]string{"src_md5": "old"}}}}, nil
		}
		return &drive.FileList{}, nil
	}}
	r := newRunner(t, Options{UpdateChanged: "keep-both"})
	j := &pairJob{c: fd, fc: &folderCache{m: map[string]string{}, c: fd}, rootID: "root"}

	r.copyFile(context.Background(), j, row{src: "s1", name: "a.txt", md5: "new"})

	if fd.copies != 1 || fd.updates != 0 || r.copied.Load() != 1 {
		t.Fatalf("copies=%d updates=%d copied=%d", fd.copies, fd.updates, r.copied.Load())
	}
}

func TestCopyFileDryRun(t *testing.T) {
	fd := &fakeDrive{}
	r := newRunner(t, Options{DryRun: true})
	j := &pairJob{c: fd, fc: &folderCache{m: map[string]string{}, c: fd, dry: true}, rootID: "root"}

	r.copyFile(context.Background(), j, row{src: "s1", name: "a.txt"})

	if fd.copies != 0 || r.copied.Load() != 1 {
		t.Fatalf("copies=%d copied=%d", fd.copies, r.copied.Load())
	}
}

func TestCopyFileResumeSkips(t *testing.T) {
	fd := &fakeDrive{}
	r := newRunner(t, Options{})
	r.state.mark("s1")
	j := &pairJob{c: fd, fc: &folderCache{m: map[string]string{}, c: fd}, rootID: "root"}

	r.copyFile(context.Background(), j, row{src: "s1", name: "a.txt"})

	if fd.copies != 0 || r.skipped.Load() != 1 {
		t.Fatalf("copies=%d skipped=%d", fd.copies, r.skipped.Load())
	}
}

func TestProcessPair(t *testing.T) {
	fd := &fakeDrive{} // List empty => folders get created
	r := newRunner(t, Options{Workers: 2})
	j := &pairJob{
		c:      fd,
		fc:     &folderCache{m: map[string]string{}, c: fd},
		rootID: "root",
		rows: []row{
			{mime: driveclient.FolderMime, name: "dir", path: ""},
			{src: "s1", name: "a.txt", path: "dir"},
			{src: "s2", name: "b.txt", path: ""},
		},
	}
	if err := r.processPair(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if fd.copies != 2 || r.copied.Load() != 2 {
		t.Fatalf("copies=%d copied=%d", fd.copies, r.copied.Load())
	}
	if fd.creates != 1 { // only "dir"
		t.Fatalf("creates=%d", fd.creates)
	}
}

func TestRunWrapperEmptyManifest(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "folders.csv")
	mp := filepath.Join(dir, "manifest.csv")
	writeFile(t, fp, "email_destino,email_origem,pasta_id_correta\nd@x,o@x,ROOT\n")
	writeFile(t, mp, "email_origem,email_destino,orig_file_id,name,mime,md5,mtime,path\n") // header only

	sum, err := Run(context.Background(), discard(), auth.NewResolver(auth.Config{}),
		Options{FoldersCSV: fp, ManifestCSV: mp})
	if err != nil || sum.Copied != 0 {
		t.Fatalf("sum=%+v err=%v", sum, err)
	}
}

func TestRunMissingFiles(t *testing.T) {
	_, err := Run(context.Background(), discard(), auth.NewResolver(auth.Config{}),
		Options{FoldersCSV: "/no/such.csv", ManifestCSV: "/no/such.csv"})
	if err == nil {
		t.Fatal("expected error for missing files")
	}
}

func TestRunLoop(t *testing.T) {
	fd := &fakeDrive{}
	r := newRunner(t, Options{Workers: 2})
	r.getClient = func(_ context.Context, _ string) (driveAPI, error) { return fd, nil }
	pair2root := map[[2]string]string{{"d@x", "o@x"}: "root"}
	byDest := map[string]map[string][]row{"d@x": {"o@x": {{src: "s1", name: "a.txt"}}}}

	sum := r.run(context.Background(), pair2root, byDest, time.Now())

	if sum.Copied != 1 || fd.copies != 1 {
		t.Fatalf("sum=%+v copies=%d", sum, fd.copies)
	}
}

func TestRunSkipsNoRootAndClientError(t *testing.T) {
	r := newRunner(t, Options{})
	r.getClient = func(_ context.Context, email string) (driveAPI, error) {
		if email == "bad@x" {
			return nil, errors.New("boom")
		}
		return &fakeDrive{}, nil
	}
	byDest := map[string]map[string][]row{
		"bad@x":  {"o@x": {{src: "s1"}}},            // client error -> skipped
		"good@x": {"o@x": {{src: "s2", name: "a"}}}, // no root entry -> skipped
	}
	sum := r.run(context.Background(), map[[2]string]string{}, byDest, time.Now())
	if sum.Copied != 0 {
		t.Fatalf("expected 0 copied, got %+v", sum)
	}
}

func TestFolderCacheCaches(t *testing.T) {
	fd := &fakeDrive{} // List empty => always create
	fc := &folderCache{m: map[string]string{}, c: fd}

	id1, err := fc.ensurePath(context.Background(), "root", "a/b/c")
	if err != nil {
		t.Fatal(err)
	}
	if fd.creates != 3 {
		t.Fatalf("creates=%d, want 3", fd.creates)
	}
	id2, _ := fc.ensurePath(context.Background(), "root", "a/b/c")
	if fd.creates != 3 || id1 != id2 {
		t.Fatalf("cache miss: creates=%d id1=%s id2=%s", fd.creates, id1, id2)
	}
}

func TestFolderCacheDry(t *testing.T) {
	fd := &fakeDrive{}
	fc := &folderCache{m: map[string]string{}, c: fd, dry: true}
	id, err := fc.ensurePath(context.Background(), "root", "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if fd.creates != 0 || fd.lists != 0 || !strings.HasPrefix(id, "DRY_") {
		t.Fatalf("dry violated: creates=%d lists=%d id=%s", fd.creates, fd.lists, id)
	}
}

func TestLoadFolders(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "folders.csv")
	writeFile(t, p, "email_destino,email_origem,pasta_id_correta\nd@x,o@gmail.com,ROOT1\n")
	m, err := loadFolders(Options{FoldersCSV: p})
	if err != nil {
		t.Fatal(err)
	}
	if m[[2]string{"d@x", "o@gmail.com"}] != "ROOT1" {
		t.Fatalf("got %v", m)
	}
}

func TestLoadFoldersConflict(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.csv")
	writeFile(t, p, "email_destino,email_origem,pasta_id_correta\nd@x,o@x,A\nd@x,o@x,B\n")
	if _, err := loadFolders(Options{FoldersCSV: p}); err == nil {
		t.Fatal("expected conflict error")
	}
}

func TestLoadManifestGroups(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "m.csv")
	writeFile(t, p, "email_origem,email_destino,orig_file_id,name,mime,md5,mtime,path\n"+
		"o@x,d@x,f1,a.txt,text/plain,m,t,dir\n")
	m, err := loadManifest(Options{ManifestCSV: p})
	if err != nil {
		t.Fatal(err)
	}
	rows := m["d@x"]["o@x"]
	if len(rows) != 1 || rows[0].src != "f1" || rows[0].path != "dir" {
		t.Fatalf("got %+v", rows)
	}
}

func TestStateResume(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.jsonl")
	s1, err := newState(p)
	if err != nil {
		t.Fatal(err)
	}
	s1.mark("x")
	s1.mark("x") // idempotent
	s1.close()

	s2, err := newState(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.close()
	if !s2.done("x") {
		t.Fatal("resume failed: x not done")
	}
}

func TestOptionsSkip(t *testing.T) {
	if !(Options{OnlyDest: "d@x"}).skip("other", "o") {
		t.Fatal("should skip on dest mismatch")
	}
	if (Options{OnlyDest: "d@x"}).skip("d@x", "o") {
		t.Fatal("should not skip on dest match")
	}
	if !(Options{OnlySrc: "o@x"}).skip("d", "other") {
		t.Fatal("should skip on src mismatch")
	}
}

func TestIsChanged(t *testing.T) {
	cases := []struct {
		name string
		f    row
		ex   *drive.File
		want bool
	}{
		{"same md5", row{md5: "abc"}, &drive.File{AppProperties: map[string]string{"src_md5": "abc"}}, false},
		{"diff md5", row{md5: "abc"}, &drive.File{AppProperties: map[string]string{"src_md5": "xyz"}}, true},
		{"newer mtime", row{mtime: "2025-02-02"}, &drive.File{AppProperties: map[string]string{"src_mtime": "2025-01-01"}}, true},
		{"older mtime", row{mtime: "2025-01-01"}, &drive.File{AppProperties: map[string]string{"src_mtime": "2025-02-02"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isChanged(c.f, c.ex); got != c.want {
				t.Fatalf("isChanged = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSplitAndDepth(t *testing.T) {
	if got := splitPath("/a//b/c/"); len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("splitPath = %v", got)
	}
	if depthOf("a/b/c") != 3 {
		t.Fatal("depthOf mismatch")
	}
	if join("", "x") != "x" || join("a/b", "c") != "a/b/c" {
		t.Fatal("join mismatch")
	}
	folders, files := splitRows([]row{{mime: driveclient.FolderMime}, {src: "f"}})
	if len(folders) != 1 || len(files) != 1 {
		t.Fatalf("splitRows folders=%d files=%d", len(folders), len(files))
	}
}

func TestCopyBody(t *testing.T) {
	b := copyBody(row{src: "s", md5: "m", mtime: "t"}, "parent", "d@x", "o@x")
	if b.AppProperties["src_id"] != "s" || b.Parents[0] != "parent" || b.AppProperties["dest_email"] != "d@x" {
		t.Fatalf("copyBody = %+v", b)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
