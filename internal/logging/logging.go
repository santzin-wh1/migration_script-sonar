// Package logging sets up a per-run slog logger that writes to both stderr and
// a run.log file, and records a summary.json at the end of a run.
package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Run holds the per-run logging context.
type Run struct {
	Dir    string
	Logger *slog.Logger
	closer io.Closer
}

// Setup creates logs/<timestamp>/ under logDir, wires a logger to stderr +
// run.log, and returns the Run handle. level is one of debug/info/warn/error.
func Setup(logDir, level string) (*Run, error) {
	runID := time.Now().Format("20060102-150405")
	dir := filepath.Join(logDir, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir log dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "run.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open run.log: %w", err)
	}
	w := io.MultiWriter(os.Stderr, f)
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: parseLevel(level)})
	return &Run{Dir: dir, Logger: slog.New(h), closer: f}, nil
}

// Close flushes and closes the run.log file.
func (r *Run) Close() error {
	if r.closer != nil {
		return r.closer.Close()
	}
	return nil
}

// WriteSummary marshals v to summary.json in the run directory.
func (r *Run) WriteSummary(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.Dir, "summary.json"), data, 0o644)
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug", "DEBUG":
		return slog.LevelDebug
	case "warn", "WARN", "WARNING":
		return slog.LevelWarn
	case "error", "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
