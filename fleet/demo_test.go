package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/allegro/bigcache/v3"
	sqlstore "github.com/go-estoria/estoria-contrib/sqlite/eventstore"
	sqlstrategy "github.com/go-estoria/estoria-contrib/sqlite/eventstore/strategy"
	"github.com/go-estoria/estoria/aggregatestore"
	_ "modernc.org/sqlite"
)

// newTestServer builds a fleet server over a temporary SQLite database, with
// the same store stack main.go assembles.
func newTestServer(t *testing.T, devices int) *server {
	t.Helper()
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "fleet.db")
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

	live, err := aggregatestore.New(eventStore, "device", NewDevice,
		aggregatestore.WithEventTypes(deviceEventPrototypes()...))
	if err != nil {
		t.Fatal(err)
	}

	cache, err := bigcache.New(ctx, bigcache.DefaultConfig(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	reg := &registry{}
	if err := registerDevices(ctx, live, reg, devices); err != nil {
		t.Fatal(err)
	}

	return &server{
		live:        live,
		history:     live,
		events:      eventStore,
		cache:       cache,
		db:          db,
		deviceCount: devices,
		reg:         reg,
		sim:         newSimulator(ctx, live, reg),
		hub:         newHub(0),
	}
}

func (s *server) rowCount(t *testing.T, table string) int {
	t.Helper()

	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestIdleSupervisor covers the behavior that makes this example affordable to
// host: the simulator runs only while somebody is watching.
func TestIdleSupervisor(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The supervisor ticks every 5s internally, so drive it with a grace
	// period short enough that one tick past an idle hub trips it.
	go srv.runIdleSupervisor(ctx, time.Nanosecond)

	if srv.sim.isRunning() {
		t.Fatal("simulator was running before any client connected")
	}

	watcher, ok := srv.hub.subscribe()
	if !ok {
		t.Fatal("subscribing failed")
	}

	if !waitFor(10*time.Second, srv.sim.isRunning) {
		t.Fatal("simulator did not start while a client was watching")
	}

	srv.hub.unsubscribe(watcher)

	if !waitFor(20*time.Second, func() bool { return !srv.sim.isRunning() }) {
		t.Error("simulator did not stop after the last client disconnected")
	}

	// and it comes back for the next visitor
	if _, ok := srv.hub.subscribe(); !ok {
		t.Fatal("re-subscribing failed")
	}
	if !waitFor(10*time.Second, srv.sim.isRunning) {
		t.Error("simulator did not resume when a client reconnected")
	}
}

// TestIdleSupervisorToleratesReloads checks the grace period: a client that
// disconnects and reconnects within it must not stop the fleet.
func TestIdleSupervisorToleratesReloads(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.runIdleSupervisor(ctx, time.Hour) // long grace period

	first, ok := srv.hub.subscribe()
	if !ok {
		t.Fatal("subscribing failed")
	}
	if !waitFor(10*time.Second, srv.sim.isRunning) {
		t.Fatal("simulator did not start")
	}

	// a page reload: disconnect, then reconnect a moment later
	srv.hub.unsubscribe(first)
	time.Sleep(50 * time.Millisecond)
	if _, ok := srv.hub.subscribe(); !ok {
		t.Fatal("re-subscribing failed")
	}

	time.Sleep(200 * time.Millisecond)
	if !srv.sim.isRunning() {
		t.Error("simulator stopped during a reload, inside the grace period")
	}
}

func TestResetDemo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	srv := newTestServer(t, 3)

	// give the devices some history to throw away
	for _, id := range srv.reg.all() {
		agg, err := srv.live.Load(ctx, id, nil)
		if err != nil {
			t.Fatal(err)
		}
		agg.Append(reading(1, 21.5, 40), reading(2, 21.6, 41))
		if err := srv.live.Save(ctx, agg, nil); err != nil {
			t.Fatal(err)
		}
	}

	before := srv.rowCount(t, eventsTable)
	if before <= 3 {
		t.Fatalf("expected history before the reset, got %d events", before)
	}

	if err := srv.resetDemo(ctx); err != nil {
		t.Fatalf("resetting demo: %v", err)
	}

	// A fresh fleet of the same size: the streams are the registry, so the
	// device count must survive even though every stream is new.
	if got := srv.reg.count(); got != 3 {
		t.Errorf("fleet size after reset = %d, want 3", got)
	}
	after := srv.rowCount(t, eventsTable)
	if after != 3 {
		t.Errorf("events after reset = %d, want 3 (one registration per device)", after)
	}
	if after >= before {
		t.Errorf("reset did not shrink the store: %d -> %d events", before, after)
	}
}

func TestNextHour(t *testing.T) {
	t.Parallel()

	for _, at := range []string{
		"2026-08-27T04:00:00Z",
		"2026-08-27T04:00:01Z",
		"2026-08-27T04:37:12Z",
		"2026-08-27T23:59:59Z",
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

	t.Run("does not limit reads", func(t *testing.T) {
		t.Parallel()

		rl := newRateLimiter(1, false)
		handler := rl.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		for i := range 50 {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/fleet", nil))
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
			req := httptest.NewRequest(http.MethodPost, "/api/sim/stop", nil)
			req.RemoteAddr = "10.0.0.3:1234"
			handler.ServeHTTP(rec, req)
			lastCode = rec.Code
		}

		if lastCode != http.StatusTooManyRequests {
			t.Errorf("code after exhausting the burst = %d, want %d", lastCode, http.StatusTooManyRequests)
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

// waitFor polls cond until it holds or the timeout elapses.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}
