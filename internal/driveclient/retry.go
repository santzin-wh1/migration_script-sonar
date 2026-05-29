// Package driveclient wraps the Google Drive v3 API with retry/backoff and a
// few convenience helpers shared by the prepare and apply stages.
package driveclient

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"google.golang.org/api/googleapi"
)

// jitter returns a value in [0, 0.5) for backoff spreading. Not
// security-sensitive (only de-syncs retry timing), but sourced from crypto/rand
// to avoid seeding concerns and PRNG static-analysis warnings.
func jitter() float64 {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		return 0
	}
	return float64(binary.BigEndian.Uint64(b[:])>>11) / (1 << 53) * 0.5
}

// retryableStatus are HTTP status codes that always warrant a retry.
var retryableStatus = map[int]bool{429: true, 500: true, 503: true}

// retryable403 are 403 reasons that are transient and worth retrying.
var retryable403 = map[string]bool{
	"rateLimitExceeded":        true,
	"userRateLimitExceeded":    true,
	"sharingRateLimitExceeded": true,
	"domainRateLimitExceeded":  true,
	"backendError":             true,
	"internalError":            true,
	"dailyLimitExceeded":       true,
}

// nonRetry403 are 403 reasons that are permanent; retrying only wastes quota.
var nonRetry403 = map[string]bool{
	"cannotCopyFile":               true,
	"fileNeverWritable":            true,
	"copyRequiresWriterPermission": true,
	"appNotAuthorizedToFile":       true,
	"insufficientFilePermissions":  true,
	"forbidden":                    true,
	"fileNotFound":                 true,
	"teamDriveFileNotFound":        true,
	"domainPolicy":                 true,
	"invalidSharingRequest":        true,
}

// Reason extracts the first Drive API error reason from err, if any.
func Reason(err error) string {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		if len(gerr.Errors) > 0 && gerr.Errors[0].Reason != "" {
			return gerr.Errors[0].Reason
		}
		return gerr.Message
	}
	return ""
}

// shouldRetry reports whether err is transient, along with the HTTP status
// code (0 if not an API error, e.g. a network/timeout error which we retry).
func shouldRetry(err error) (bool, int) {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		switch {
		case gerr.Code == 403:
			r := ""
			if len(gerr.Errors) > 0 {
				r = gerr.Errors[0].Reason
			}
			if nonRetry403[r] {
				return false, gerr.Code
			}
			if retryable403[r] {
				return true, gerr.Code
			}
			return false, gerr.Code
		case retryableStatus[gerr.Code]:
			return true, gerr.Code
		default:
			return false, gerr.Code
		}
	}
	// Non-API errors (network, timeout, context-less transport failures): retry.
	return true, 0
}

// Do runs fn with exponential backoff + jitter, retrying only transient
// failures up to attempts times. ctx cancellation aborts the wait.
func Do[T any](ctx context.Context, lg *slog.Logger, desc string, attempts int, fn func() (T, error)) (T, error) {
	var zero T
	delay := 0.8
	for i := 1; i <= attempts; i++ {
		v, err := fn()
		if err == nil {
			return v, nil
		}
		retry, code := shouldRetry(err)
		if !retry || i == attempts {
			lg.Warn("no-retry", "op", desc, "status", code, "reason", Reason(err))
			return zero, err
		}
		sl := math.Min(delay*1.7+jitter(), 20)
		lg.Warn("backoff", "op", desc, "attempt", fmt.Sprintf("%d/%d", i, attempts), "sleep_s", fmt.Sprintf("%.2f", sl))
		select {
		case <-time.After(time.Duration(sl * float64(time.Second))):
		case <-ctx.Done():
			return zero, ctx.Err()
		}
		delay = sl
	}
	return zero, fmt.Errorf("%s: exhausted %d attempts", desc, attempts)
}
