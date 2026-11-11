package baseten

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsRateLimited(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"429", &APIError{StatusCode: http.StatusTooManyRequests, Message: "slow down"}, true},
		{"wrapped 429", fmt.Errorf("listing deployments: %w", &APIError{StatusCode: http.StatusTooManyRequests, Message: "slow down"}), true},
		{"500", &APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}, false},
		{"404", &APIError{StatusCode: http.StatusNotFound, Message: "missing"}, false},
		{"plain error", errors.New("connection refused"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRateLimited(tt.err); got != tt.want {
				t.Errorf("IsRateLimited(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestRetryAfter(t *testing.T) {
	withHeader := &APIError{
		StatusCode:    http.StatusTooManyRequests,
		Message:       "slow down",
		RetryAfter:    45 * time.Second,
		HasRetryAfter: true,
	}
	d, ok := RetryAfter(withHeader)
	if !ok || d != 45*time.Second {
		t.Fatalf("RetryAfter(with header) = %s, %v; want 45s, true", d, ok)
	}

	bare := &APIError{StatusCode: http.StatusTooManyRequests, Message: "slow down"}
	if _, ok := RetryAfter(bare); ok {
		t.Fatal("RetryAfter without a header should report the header absent")
	}
	if _, ok := RetryAfter(fmt.Errorf("wrap: %w", &APIError{StatusCode: http.StatusInternalServerError})); ok {
		t.Fatal("RetryAfter on a non-429 error should report the header absent")
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		header string
		want   time.Duration
		ok     bool
	}{
		{header: "12", want: 12 * time.Second, ok: true},
		{header: "0", want: 0, ok: true},
		{header: "  30  ", want: 30 * time.Second, ok: true},
		{header: now.Add(45 * time.Second).Format(time.RFC1123), want: 45 * time.Second, ok: true},
		{header: now.Add(-time.Minute).Format(time.RFC1123), want: 0, ok: true},
		{header: "", ok: false},
		{header: "soon", ok: false},
		{header: "-5", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.header, func(t *testing.T) {
			got, ok := parseRetryAfter(tt.header, now)
			if ok != tt.ok || got != tt.want {
				t.Errorf("parseRetryAfter(%q) = %s, %v; want %s, %v", tt.header, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestClientCapturesRetryAfterOn429(t *testing.T) {
	t.Run("delta seconds", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "12")
			writeError(t, w, http.StatusTooManyRequests, "slow down")
		}))
		defer srv.Close()

		_, err := newTestClient(srv.URL).GetEnvironment(context.Background(), "model1", "dev")
		assertRetryAfter(t, err, 12*time.Second, true)
	})

	t.Run("http date", func(t *testing.T) {
		when := time.Now().Add(30 * time.Second).UTC().Truncate(time.Second)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", when.Format(time.RFC1123))
			writeError(t, w, http.StatusTooManyRequests, "slow down")
		}))
		defer srv.Close()

		err := newTestClient(srv.URL).DeleteModel(context.Background(), "model1")
		var apiErr *APIError
		if !errors.As(err, &apiErr) || !apiErr.HasRetryAfter {
			t.Fatalf("expected 429 with Retry-After, got %v", err)
		}
		// The date is second-resolution and the request takes a moment, so allow slack
		// but require a positive wait that tracks the header.
		if apiErr.RetryAfter < 20*time.Second || apiErr.RetryAfter > 30*time.Second {
			t.Fatalf("RetryAfter = %s, want about 30s", apiErr.RetryAfter)
		}
	})

	t.Run("missing header", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(t, w, http.StatusTooManyRequests, "slow down")
		}))
		defer srv.Close()

		err := newTestClient(srv.URL).ActivateDeployment(context.Background(), "model1", "dep1")
		assertRetryAfter(t, err, 0, false)
	})

	t.Run("non-429 ignores Retry-After", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "12")
			writeError(t, w, http.StatusInternalServerError, "boom")
		}))
		defer srv.Close()

		_, err := newTestClient(srv.URL).GetEnvironment(context.Background(), "model1", "dev")
		if IsRateLimited(err) {
			t.Fatalf("500 must not be treated as rate limited: %v", err)
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("expected APIError, got %v", err)
		}
		if apiErr.HasRetryAfter {
			t.Fatal("Retry-After on a non-429 response must not be attached")
		}
	})
}

func assertRetryAfter(t *testing.T, err error, want time.Duration, has bool) {
	t.Helper()
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %v", err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("StatusCode = %d, want 429", apiErr.StatusCode)
	}
	if apiErr.HasRetryAfter != has || apiErr.RetryAfter != want {
		t.Fatalf("RetryAfter = %s (has=%v), want %s (has=%v)", apiErr.RetryAfter, apiErr.HasRetryAfter, want, has)
	}
	if !IsRateLimited(err) {
		t.Fatal("expected IsRateLimited")
	}
}
