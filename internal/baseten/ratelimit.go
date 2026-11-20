package baseten

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type rateLimitKey struct{}

// rateLimitCapture is stored on the request context so the HTTP transport can
// record Retry-After. The generated management client only returns status and
// body, so the header would otherwise be dropped.
type rateLimitCapture struct {
	mu            sync.Mutex
	hasRetryAfter bool
	retryAfter    time.Duration
}

func captureRateLimit(ctx context.Context) context.Context {
	if ctx.Value(rateLimitKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, rateLimitKey{}, &rateLimitCapture{})
}

func retryAfterFrom(ctx context.Context) (time.Duration, bool) {
	cap, _ := ctx.Value(rateLimitKey{}).(*rateLimitCapture)
	if cap == nil {
		return 0, false
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	return cap.retryAfter, cap.hasRetryAfter
}

// IsRateLimited reports whether err is an HTTP 429 from the Baseten API.
// Wrapped errors are unwrapped.
func IsRateLimited(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusTooManyRequests
}

// RetryAfter returns the Retry-After duration when err is a 429 that carried
// the header. The boolean is false when the header was absent or err is not a 429.
func RetryAfter(err error) (time.Duration, bool) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests || !apiErr.HasRetryAfter {
		return 0, false
	}
	return apiErr.RetryAfter, true
}

type rateLimitTransport struct {
	base http.RoundTripper
}

func (t *rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		return resp, err
	}
	cap, _ := req.Context().Value(rateLimitKey{}).(*rateLimitCapture)
	if cap == nil {
		return resp, err
	}
	d, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	if !ok {
		return resp, err
	}
	cap.mu.Lock()
	cap.hasRetryAfter = true
	cap.retryAfter = d
	cap.mu.Unlock()
	return resp, err
}

func withRateLimitTransport(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if _, ok := transport.(*rateLimitTransport); ok {
		return base
	}
	clone := *base
	clone.Transport = &rateLimitTransport{base: transport}
	return &clone
}

// MaxRetryAfter is the longest Retry-After the operator will wait.
// Larger headers, and dates far enough to overflow a duration, clamp here.
const MaxRetryAfter = 15 * time.Minute

// parseRetryAfter accepts the two forms from RFC 9110: delta-seconds or an HTTP-date.
// A date in the past yields 0, true so callers still know the header was present.
// Values outside [0, MaxRetryAfter] clamp into that range. A date is compared
// before subtraction so a far-future HTTP-date cannot overflow to a negative duration.
func parseRetryAfter(header string, now time.Time) (time.Duration, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false
	}
	secs, err := strconv.ParseInt(header, 10, 64)
	if err == nil {
		return clampRetryAfterSeconds(secs), true
	}
	var numErr *strconv.NumError
	if errors.As(err, &numErr) && errors.Is(numErr.Err, strconv.ErrRange) {
		return MaxRetryAfter, true
	}
	t, err := http.ParseTime(header)
	if err != nil {
		return 0, false
	}
	limit := now.Add(MaxRetryAfter)
	if !t.Before(limit) {
		return MaxRetryAfter, true
	}
	d := t.Sub(now)
	if d < 0 {
		return 0, true
	}
	return d, true
}

func clampRetryAfterSeconds(secs int64) time.Duration {
	if secs < 0 {
		return 0
	}
	maxSecs := int64(MaxRetryAfter / time.Second)
	if secs > maxSecs {
		return MaxRetryAfter
	}
	return time.Duration(secs) * time.Second
}
