package logging

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestSetupAndSummary(t *testing.T) {
	dir := t.TempDir()
	run, err := Setup(dir, "info")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()

	run.Logger.Info("hello", "k", "v")

	if _, err := os.Stat(filepath.Join(run.Dir, "run.log")); err != nil {
		t.Fatalf("run.log not created: %v", err)
	}

	if err := run.WriteSummary(map[string]int{"copied": 3}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(run.Dir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		t.Fatal("empty summary.json")
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"WARNING": slog.LevelWarn,
		"error":   slog.LevelError,
		"weird":   slog.LevelInfo, // unknown -> info
	}
	for s, want := range cases {
		if got := parseLevel(s); got != want {
			t.Fatalf("parseLevel(%q)=%v want %v", s, got, want)
		}
	}
}
