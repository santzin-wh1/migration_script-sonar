package apply

import (
	"testing"

	"google.golang.org/api/drive/v3"
)

func TestIsChanged(t *testing.T) {
	cases := []struct {
		name string
		f    row
		ex   *drive.File
		want bool
	}{
		{
			name: "same md5",
			f:    row{md5: "abc"},
			ex:   &drive.File{AppProperties: map[string]string{"src_md5": "abc"}},
			want: false,
		},
		{
			name: "different md5",
			f:    row{md5: "abc"},
			ex:   &drive.File{AppProperties: map[string]string{"src_md5": "xyz"}},
			want: true,
		},
		{
			name: "no md5, newer mtime",
			f:    row{mtime: "2025-02-02T00:00:00Z"},
			ex:   &drive.File{AppProperties: map[string]string{"src_mtime": "2025-01-01T00:00:00Z"}},
			want: true,
		},
		{
			name: "no md5, older mtime",
			f:    row{mtime: "2025-01-01T00:00:00Z"},
			ex:   &drive.File{AppProperties: map[string]string{"src_mtime": "2025-02-02T00:00:00Z"}},
			want: false,
		},
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
		t.Fatalf("depthOf mismatch")
	}
	if join("", "x") != "x" || join("a/b", "c") != "a/b/c" {
		t.Fatalf("join mismatch")
	}
}
