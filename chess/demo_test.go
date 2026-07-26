package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sqlstore "github.com/go-estoria/estoria-contrib/sqlite/eventstore"
	sqlstrategy "github.com/go-estoria/estoria-contrib/sqlite/eventstore/strategy"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/gofrs/uuid/v5"
	_ "modernc.org/sqlite"
)

// newTestServer builds the same store stack main.go does, over a temporary
// SQLite database.
func newTestServer(t *testing.T) *server {
	t.Helper()
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "chess.db")
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	strat, err := sqlstrategy.NewDefaultStrategy(
		sqlstrategy.WithEventsTableName(eventsTable),
		sqlstrategy.WithStreamsTableName(streamsTable),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, strat.Schema()); err != nil {
		t.Fatal(err)
	}

	eventStore, err := sqlstore.New(db, sqlstore.WithStrategy(strat))
	if err != nil {
		t.Fatal(err)
	}

	eventSourced, err := aggregatestore.New(eventStore, NewGame,
		aggregatestore.WithEventTypes(gameEventPrototypes()...))
	if err != nil {
		t.Fatal(err)
	}

	hookable, err := aggregatestore.NewHookableStore[Game](eventSourced)
	if err != nil {
		t.Fatal(err)
	}

	return &server{
		live:    hookable,
		history: eventSourced,
		events:  eventStore,
		db:      db,
		hub:     newHub(0),
	}
}

// TestResetDemo covers the hosted-demo reset: every game is gone afterwards,
// the storage is actually cleared, and watching browsers are told to reload.
func TestResetDemo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	srv := newTestServer(t)

	gameID := uuid.Must(uuid.NewV7())
	agg := srv.live.New(gameID)
	if err := agg.Append(
		GameCreated{White: "Alice", Black: "Bob"},
		MoveMade{UCI: "e2e4"},
	); err != nil {
		t.Fatal(err)
	}
	if err := srv.live.Save(ctx, agg, nil); err != nil {
		t.Fatal(err)
	}

	streams, err := srv.events.ListStreams(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) == 0 {
		t.Fatal("expected the game stream to exist before the reset")
	}

	// a browser watching the lobby
	watcher, ok := srv.hub.subscribe()
	if !ok {
		t.Fatal("subscribing to the hub failed")
	}

	if err := srv.resetDemo(ctx); err != nil {
		t.Fatalf("resetting demo: %v", err)
	}

	streams, err = srv.events.ListStreams(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) != 0 {
		t.Errorf("streams after reset = %d, want 0", len(streams))
	}

	if _, err := srv.live.Load(ctx, gameID, nil); err == nil {
		t.Error("the game still loaded after the reset")
	}

	select {
	case msg := <-watcher:
		var got resetMessage
		if err := json.Unmarshal(msg, &got); err != nil {
			t.Fatalf("unmarshaling broadcast: %v", err)
		}
		if !got.Reset {
			t.Errorf("broadcast = %s, want a reset message", msg)
		}
	case <-time.After(time.Second):
		t.Error("connected clients were not told about the reset")
	}
}

func TestNextHour(t *testing.T) {
	t.Parallel()

	for _, at := range []string{
		"2026-07-26T04:00:00Z",
		"2026-07-26T04:00:01Z",
		"2026-07-26T04:37:12Z",
		"2026-07-26T23:59:59Z",
	} {
		now, err := time.Parse(time.RFC3339, at)
		if err != nil {
			t.Fatal(err)
		}

		next := nextHour(now)
		if next.Minute() != 0 || next.Second() != 0 || next.Nanosecond() != 0 {
			t.Errorf("nextHour(%s) = %s, want an exact hour boundary", at, next)
		}
		if d := next.Sub(now); d <= 0 || d > time.Hour {
			t.Errorf("nextHour(%s) is %s away, want within (0, 1h]", at, d)
		}
	}
}

func TestRateLimiter(t *testing.T) {
	t.Parallel()

	t.Run("allows a burst then throttles", func(t *testing.T) {
		t.Parallel()

		rl := newRateLimiter(60, false)

		for i := range rl.burst {
			if !rl.allow("10.0.0.1") {
				t.Fatalf("request %d of the burst was denied", i+1)
			}
		}
		if rl.allow("10.0.0.1") {
			t.Error("a request past the burst was allowed")
		}
		if !rl.allow("10.0.0.2") {
			t.Error("a second IP was throttled by the first IP's traffic")
		}
	})

	t.Run("does not limit reads", func(t *testing.T) {
		t.Parallel()

		rl := newRateLimiter(1, false)
		handler := rl.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		for i := range 50 {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/games", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %d = %d, want 200: reads must never be rate limited", i+1, rec.Code)
			}
		}
	})

	t.Run("throttles writes with 429", func(t *testing.T) {
		t.Parallel()

		rl := newRateLimiter(4, false)
		handler := rl.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		var lastCode int
		for range rl.burst + 1 {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/games", nil)
			req.RemoteAddr = "10.0.0.3:1234"
			handler.ServeHTTP(rec, req)
			lastCode = rec.Code
		}

		if lastCode != http.StatusTooManyRequests {
			t.Errorf("code after exhausting the burst = %d, want %d", lastCode, http.StatusTooManyRequests)
		}
	})

	t.Run("reads the client IP from the proxy header only when trusted", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest(http.MethodPost, "/api/games", nil)
		req.RemoteAddr = "10.0.0.9:5555"
		req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.1.1.1")

		if got := newRateLimiter(60, true).clientIP(req); got != "203.0.113.7" {
			t.Errorf("trusted proxy client IP = %q, want the first forwarded address", got)
		}
		if got := newRateLimiter(60, false).clientIP(req); got != "10.0.0.9" {
			t.Errorf("untrusted client IP = %q, want the socket address", got)
		}
	})
}

func TestHubCapacity(t *testing.T) {
	t.Parallel()

	h := newHub(2)

	first, ok := h.subscribe()
	if !ok {
		t.Fatal("first subscriber was rejected")
	}
	if _, ok := h.subscribe(); !ok {
		t.Fatal("second subscriber was rejected")
	}
	if _, ok := h.subscribe(); ok {
		t.Error("a third subscriber was accepted past the cap of 2")
	}

	h.unsubscribe(first)
	if _, ok := h.subscribe(); !ok {
		t.Error("a slot was not freed when a client disconnected")
	}
}

// TestRunResets drives the scheduler loop itself — the one piece of demo-only
// code that otherwise runs unattended, on a timer, and would fail silently.
func TestRunResets(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	watcher, ok := srv.hub.subscribe()
	if !ok {
		t.Fatal("subscribing to the hub failed")
	}

	// fire every 20ms instead of on the hour
	go srv.runResets(ctx, func(now time.Time) time.Time { return now.Add(20 * time.Millisecond) })

	// the loop must keep going, not reset once and stop
	for i := range 3 {
		select {
		case <-watcher:
		case <-time.After(2 * time.Second):
			t.Fatalf("scheduler stopped after %d resets", i)
		}
	}

	cancel()

	// drain anything already queued, then confirm it stays quiet
	time.Sleep(100 * time.Millisecond)
	for len(watcher) > 0 {
		<-watcher
	}
	select {
	case <-watcher:
		t.Error("the scheduler kept resetting after its context was cancelled")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestPathGameID covers the three shapes a client can put in the {id} segment.
// The nil UUID is the interesting one: it parses, so it reaches estoria, which
// rejects it — and that surfaced as a 500 for what is plainly bad input.
func TestPathGameID(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	handler := srv.routes()

	for _, tc := range []struct {
		name string
		id   string
		want int
	}{
		{"malformed", "not-a-uuid", http.StatusBadRequest},
		{"nil UUID", "00000000-0000-0000-0000-000000000000", http.StatusBadRequest},
		{"well-formed but unknown", "83f07308-a33c-41ec-85f5-72d7afd3d73b", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			for _, target := range []string{
				"GET /api/games/" + tc.id,
				"POST /api/games/" + tc.id + "/move",
			} {
				method, path, _ := strings.Cut(target, " ")
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(method, path, strings.NewReader(`{"baseVersion":1,"uci":"e2e4"}`))
				req.Header.Set("Content-Type", "application/json")
				handler.ServeHTTP(rec, req)

				if rec.Code != tc.want {
					t.Errorf("%s = %d, want %d (body: %s)", target, rec.Code, tc.want, rec.Body.String())
				}
			}
		})
	}
}
