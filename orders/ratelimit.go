package main

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// A rateLimiter throttles state-changing requests per client IP.
//
// It exists for the hosted demo, which accepts writes from anyone on the
// internet with no authentication. Reads are never limited — a burst of GETs
// is what a popular demo looks like — but every write costs an event that
// lives in the stream until the next reset, so those are metered.
type rateLimiter struct {
	limit rate.Limit
	burst int

	// trustProxy makes the limiter read the client address from
	// X-Forwarded-For. Only enable it behind a proxy that overwrites that
	// header (Railway, Fly, a load balancer); otherwise any client can forge
	// a new identity per request and the limiter becomes decorative.
	trustProxy bool

	mu       sync.Mutex
	visitors map[string]*visitor
}

type visitor struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// newRateLimiter returns a limiter allowing perMinute writes per IP, with a
// burst of a quarter of that (minimum 5) so ordinary interactive use — a few
// quick card moves in a row — never trips it.
func newRateLimiter(perMinute int, trustProxy bool) *rateLimiter {
	burst := max(perMinute/4, 5)
	return &rateLimiter{
		limit:      rate.Limit(float64(perMinute) / 60.0),
		burst:      burst,
		trustProxy: trustProxy,
		visitors:   make(map[string]*visitor),
	}
}

// middleware wraps h, rejecting state-changing requests that exceed the limit.
func (rl *rateLimiter) middleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			h.ServeHTTP(w, r)
			return
		}

		if !rl.allow(rl.clientIP(r)) {
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusTooManyRequests, "too many changes too quickly — slow down a moment")
			return
		}

		h.ServeHTTP(w, r)
	})
}

func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	v, ok := rl.visitors[ip]
	if !ok {
		v = &visitor{limiter: rate.NewLimiter(rl.limit, rl.burst)}
		rl.visitors[ip] = v
	}
	v.lastSeen = time.Now()

	return v.limiter.Allow()
}

// clientIP identifies the caller. Behind a trusted proxy that is the first
// entry in X-Forwarded-For (the original client, before each hop appended
// itself); otherwise it is the socket's remote address.
func (rl *rateLimiter) clientIP(r *http.Request) string {
	if rl.trustProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			if first, _, found := strings.Cut(fwd, ","); found {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(fwd)
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// sweep drops visitors idle for longer than idleFor, so a long-running demo
// doesn't accumulate a limiter for every IP that ever visited.
func (rl *rateLimiter) sweep(idleFor time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	cutoff := time.Now().Add(-idleFor)
	for ip, v := range rl.visitors {
		if v.lastSeen.Before(cutoff) {
			delete(rl.visitors, ip)
		}
	}
}

// runSweeper sweeps idle visitors every five minutes until ctx is done.
func (rl *rateLimiter) runSweeper(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rl.sweep(15 * time.Minute)
		}
	}
}
