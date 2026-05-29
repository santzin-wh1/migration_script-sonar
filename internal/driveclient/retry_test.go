package driveclient

import (
	"errors"
	"testing"

	"google.golang.org/api/googleapi"
)

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

func TestReason(t *testing.T) {
	if r := Reason(apiErr(403, "rateLimitExceeded")); r != "rateLimitExceeded" {
		t.Fatalf("Reason = %q", r)
	}
	if r := Reason(errors.New("x")); r != "" {
		t.Fatalf("Reason(non-api) = %q, want empty", r)
	}
}
