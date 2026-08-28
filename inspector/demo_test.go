package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestScanLimitOnlyMetersTheScan checks that the hosted-demo limiter guards
// the endpoint that scans the whole store and leaves bounded reads alone.
func TestScanLimitOnlyMetersTheScan(t *testing.T) {
	t.Parallel()

	srv := &server{backend: &backend{name: "test", label: "Test"}}
	rl := newRateLimiter(4, false)
	handler := srv.routesWithScanLimit(rl)

	exhaust := func(path string) int {
		var last int
		for range rl.burst + 2 {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.RemoteAddr = "10.0.0.5:1111"
			handler.ServeHTTP(rec, req)
			last = rec.Code
		}
		return last
	}

	// The scan endpoint is metered: past the burst it must refuse.
	if got := exhaust("/api/all/tail"); got != http.StatusTooManyRequests {
		t.Errorf("/api/all/tail after the burst = %d, want %d", got, http.StatusTooManyRequests)
	}

	// Bounded reads are not, even from the same IP that just tripped it.
	// (No capabilities are configured here, so a 501 is the expected pass —
	// what matters is that it is not a 429.)
	if got := exhaust("/api/streams"); got == http.StatusTooManyRequests {
		t.Error("/api/streams was rate limited; only scan-heavy reads should be")
	}
	if got := exhaust("/api/info"); got == http.StatusTooManyRequests {
		t.Error("/api/info was rate limited; only scan-heavy reads should be")
	}
}

func TestRateLimiterProxyHeader(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/api/all/tail", nil)
	req.RemoteAddr = "10.0.0.9:5555"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.1.1.1")

	if got := newRateLimiter(60, true).clientIP(req); got != "203.0.113.7" {
		t.Errorf("trusted proxy client IP = %q, want the first forwarded address", got)
	}
	if got := newRateLimiter(60, false).clientIP(req); got != "10.0.0.9" {
		t.Errorf("untrusted client IP = %q, want the socket address", got)
	}
}
