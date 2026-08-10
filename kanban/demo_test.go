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

	sqlstore "github.com/go-estoria/estoria-contrib/sqlite/eventstore"
	sqlstrategy "github.com/go-estoria/estoria-contrib/sqlite/eventstore/strategy"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/go-estoria/estoria/snapshotstore"
	streamsnapshots "github.com/go-estoria/estoria/snapshotstore/eventstream"
	"github.com/gofrs/uuid/v5"
	_ "modernc.org/sqlite"
)

// newTestServer builds the same store stack main.go does, over a temporary
// SQLite database.
func newTestServer(t *testing.T) *server {
	t.Helper()
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "kanban.db")
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

	eventSourced, err := aggregatestore.New(eventStore, "board", NewBoard,
		aggregatestore.WithEventTypes(boardEventPrototypes()...))
	if err != nil {
		t.Fatal(err)
	}

	snapshotting, err := aggregatestore.NewSnapshottingStore(
		eventSourced,
		streamsnapshots.New(eventStore),
		snapshotstore.EventCountSnapshotPolicy{N: 10},
	)
	if err != nil {
		t.Fatal(err)
	}

	hookable, err := aggregatestore.NewHookableStore(snapshotting)
	if err != nil {
		t.Fatal(err)
	}

	// the same AfterSave broadcast main.go registers — without it the store
	// would behave differently under test than in the app
	broadcasts := newHub(0)
	hookable.AfterSave(func(_ context.Context, agg *aggregatestore.Aggregate[Board]) error {
		broadcasts.broadcast(boardMessage{Version: agg.Version(), Live: true, Board: agg.State()})
		return nil
	})

	return &server{
		boardID:       uuid.Must(uuid.NewV4()),
		live:          hookable,
		history:       eventSourced,
		events:        eventStore,
		db:            db,
		hub:           broadcasts,
		snapshotEvery: 10,
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

// TestResetDemo covers the hosted-demo reset: the board returns to its seeded
// state, and the storage really is cleared rather than appended to.
func TestResetDemo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	srv := newTestServer(t)

	if err := seedBoard(ctx, srv.live, srv.boardID); err != nil {
		t.Fatal(err)
	}

	seeded, err := srv.live.Load(ctx, srv.boardID, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Two streams at this point: the board's, and the parallel snapshot stream
	// the snapshotting store writes into.
	seededVersion := seeded.Version()
	seededEvents, seededStreams := srv.rowCount(t, eventsTable), srv.rowCount(t, streamsTable)

	// dirty the board the way a visitor would
	agg, err := srv.live.Load(ctx, srv.boardID, nil)
	if err != nil {
		t.Fatal(err)
	}
	agg.Append(CardAdded{
		CardID:   "graffiti",
		ColumnID: agg.State().Columns[0].ID,
		Title:    "someone wrote this",
	})
	if err := srv.live.Save(ctx, agg, nil); err != nil {
		t.Fatal(err)
	}

	if err := srv.resetDemo(ctx); err != nil {
		t.Fatalf("resetting demo: %v", err)
	}

	reset, err := srv.live.Load(ctx, srv.boardID, nil)
	if err != nil {
		t.Fatalf("loading board after reset: %v", err)
	}

	if reset.Version() != seededVersion {
		t.Errorf("version after reset = %d, want %d (the seeded version)", reset.Version(), seededVersion)
	}
	for _, column := range reset.State().Columns {
		for _, card := range column.Cards {
			if card.ID == "graffiti" {
				t.Error("the card added after seeding survived the reset")
			}
		}
	}

	// The point of the reset is that storage is cleared, not appended to: a
	// reset that merely re-seeded would leave the old events (and snapshots)
	// behind and grow the database every hour.
	if got := srv.rowCount(t, eventsTable); got != seededEvents {
		t.Errorf("event rows after reset = %d, want %d (a freshly seeded store)", got, seededEvents)
	}
	if got := srv.rowCount(t, streamsTable); got != seededStreams {
		t.Errorf("stream rows after reset = %d, want %d (a freshly seeded store)", got, seededStreams)
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

		// 60/minute is one per second, with a burst of a quarter of that.
		rl := newRateLimiter(60, false)

		for i := range rl.burst {
			if !rl.allow("10.0.0.1") {
				t.Fatalf("request %d of the burst was denied", i+1)
			}
		}
		if rl.allow("10.0.0.1") {
			t.Error("a request past the burst was allowed")
		}

		// a different visitor is unaffected
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
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/board", nil))
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
			req := httptest.NewRequest(http.MethodPost, "/api/cards", nil)
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

		req := httptest.NewRequest(http.MethodPost, "/api/cards", nil)
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

	// a departing client frees its slot
	h.unsubscribe(first)
	if _, ok := h.subscribe(); !ok {
		t.Error("a slot was not freed when a client disconnected")
	}
}

// TestRunResets drives the scheduler loop itself — the one piece of demo-only
// code that otherwise runs unattended, on a timer, and would fail silently.
func TestRunResets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	srv := newTestServer(t)
	if err := seedBoard(ctx, srv.live, srv.boardID); err != nil {
		t.Fatal(err)
	}

	// each reset reseeds, and the seed's save broadcasts through the hook
	watcher, ok := srv.hub.subscribe()
	if !ok {
		t.Fatal("subscribing to the hub failed")
	}

	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// fire every 20ms instead of on the hour
	go srv.runResets(loopCtx, func(now time.Time) time.Time { return now.Add(20 * time.Millisecond) })

	// the loop must keep going, not reset once and stop
	for i := range 3 {
		select {
		case <-watcher:
		case <-time.After(2 * time.Second):
			t.Fatalf("scheduler stopped after %d resets", i)
		}
	}

	// the board is still the seeded one after all that resetting
	agg, err := srv.live.Load(ctx, srv.boardID, nil)
	if err != nil {
		t.Fatalf("loading board after repeated resets: %v", err)
	}
	if agg.Version() == 0 {
		t.Error("board is empty after a reset cycle, want the seeded board")
	}

	cancel()

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
