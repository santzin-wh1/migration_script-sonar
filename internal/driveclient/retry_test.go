package driveclient

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"google.golang.org/api/googleapi"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func apiErr(code int, reason string) error {
	e := &googleapi.Error{Code: code}
	if reason != "" {
		e.Errors = []googleapi.ErrorItem{{Reason: reason}}
	}
	return e
}

func TestShouldRetry(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		want    bool
		wantHTT int
	}{
		{"429", apiErr(429, ""), true, 429},
		{"500", apiErr(500, ""), true, 500},
		{"503", apiErr(503, ""), true, 503},
		{"404", apiErr(404, "notFound"), false, 404},
		{"403 transient", apiErr(403, "userRateLimitExceeded"), true, 403},
		{"403 permanent", apiErr(403, "insufficientFilePermissions"), false, 403},
		{"403 unknown", apiErr(403, "somethingElse"), false, 403},
		{"network error", errors.New("connection reset"), true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, code := shouldRetry(c.err)
			if got != c.want || code != c.wantHTT {
				t.Fatalf("shouldRetry(%v) = (%v,%d), want (%v,%d)", c.err, got, code, c.want, c.wantHTT)
			}
		})
	}
}

func TestDoSuccessFirstTry(t *testing.T) {
	n := 0
	v, err := Do(context.Background(), discard(), "op", 3, func() (int, error) {
		n++
		return 42, nil
	})
	if err != nil || v != 42 || n != 1 {
		t.Fatalf("v=%d err=%v n=%d", v, err, n)
	}
}

func TestDoRetryThenSuccess(t *testing.T) {
	n := 0
	v, err := Do(context.Background(), discard(), "op", 3, func() (int, error) {
		n++
		if n < 2 {
			return 0, apiErr(503, "")
		}
		return 7, nil
	})
	if err != nil || v != 7 || n != 2 {
		t.Fatalf("v=%d err=%v n=%d", v, err, n)
	}
}

func TestDoNonRetryableStops(t *testing.T) {
	n := 0
	_, err := Do(context.Background(), discard(), "op", 5, func() (int, error) {
		n++
		return 0, apiErr(404, "notFound")
	})
	if err == nil || n != 1 {
		t.Fatalf("err=%v n=%d (expected 1 attempt)", err, n)
	}
}

func TestDoContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled; the wait after first failure aborts
	n := 0
	_, err := Do(ctx, discard(), "op", 5, func() (int, error) {
		n++
		return 0, apiErr(503, "")
	})
	if err == nil {
		t.Fatal("expected context error")
	}
}

func TestJitterRange(t *testing.T) {
	for i := 0; i < 1000; i++ {
		j := jitter()
		if j < 0 || j >= 0.5 {
			t.Fatalf("jitter out of range: %f", j)
		}
	}
}

func TestEscapeQuery(t *testing.T) {
	if got := EscapeQuery("a'b\\c\nd"); got != `a\'b\\c d` {
		t.Fatalf("EscapeQuery=%q", got)
	}
}

func TestReason(t *testing.T) {
	if r := Reason(apiErr(403, "rateLimitExceeded")); r != "rateLimitExceeded" {
		t.Fatalf("Reason = %q", r)
	}
	if r := Reason(errors.New("x")); r != "" {
		t.Fatalf("Reason(non-api) = %q, want empty", r)
	}
}
