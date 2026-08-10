package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	pgeventstore "github.com/go-estoria/estoria-contrib/postgres/eventstore"
	pgstrategy "github.com/go-estoria/estoria-contrib/postgres/eventstore/strategy"
	pgoutbox "github.com/go-estoria/estoria-contrib/postgres/outbox"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestResetDemo needs a real Postgres, because the reset is a TRUNCATE across
// four tables in one transaction. It is skipped unless ORDERS_TEST_DSN is set,
// which keeps `go test ./...` dependency-free:
//
//	make up
//	ORDERS_TEST_DSN="postgres://estoria:estoria@localhost:5433/estoria?sslmode=disable" go test -run TestResetDemo ./...
func TestResetDemo(t *testing.T) {
	dsn := os.Getenv("ORDERS_TEST_DSN")
	if dsn == "" {
		t.Skip("set ORDERS_TEST_DSN to run the reset test against a live Postgres")
	}

	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	strat, err := pgstrategy.NewDefaultStrategy(
		pgstrategy.WithEventsTableName(eventsTable),
		pgstrategy.WithStreamsTableName(streamsTable),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, strat.Schema()); err != nil {
		t.Fatal(err)
	}

	rm := newReadModel(pool)
	if _, err := pool.Exec(ctx, rm.schema()); err != nil {
		t.Fatal(err)
	}

	applied := make(chan struct{}, 16)
	ob, err := pgoutbox.New(pool, func(ctx context.Context, item *pgoutbox.Item) error {
		if err := rm.apply(ctx, item); err != nil {
			return err
		}
		applied <- struct{}{}
		return nil
	}, pgoutbox.WithPollInterval(50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, ob.Schema()); err != nil {
		t.Fatal(err)
	}

	eventStore, err := pgeventstore.New(pool,
		pgeventstore.WithStrategy(strat),
		pgeventstore.WithAppendTransactionHooks(ob),
	)
	if err != nil {
		t.Fatal(err)
	}

	orders, err := aggregatestore.New(eventStore, "order", NewOrder,
		aggregatestore.WithEventTypes(orderEventPrototypes()...))
	if err != nil {
		t.Fatal(err)
	}

	srv := &server{
		orders:    orders,
		events:    eventStore,
		readModel: rm,
		pool:      pool,
		hub:       newHub(0),
		log:       newDeliveryLog(8),
	}

	// start from a clean slate regardless of what the local demo left behind
	if err := srv.resetDemo(ctx); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = ob.Run(runCtx) }()

	agg := orders.New(uuid.Must(uuid.NewV7()))
	agg.Append(OrderPlaced{Customer: "Reset Test", Items: testItems})
	if err := orders.Save(ctx, agg, nil); err != nil {
		t.Fatal(err)
	}

	// wait for the outbox to project it into the read model
	select {
	case <-applied:
	case <-time.After(10 * time.Second):
		t.Fatal("the outbox never delivered the placed order")
	}

	summaries, err := rm.list(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("read model rows before reset = %d, want 1", len(summaries))
	}

	watcher, ok := srv.hub.subscribe()
	if !ok {
		t.Fatal("subscribing to the hub failed")
	}
	srv.log.add(delivery{EventType: "orderplaced", OrderID: "order_deadbeef"})

	if err := srv.resetDemo(ctx); err != nil {
		t.Fatalf("resetting demo: %v", err)
	}

	// The write side is gone...
	if _, err := orders.Load(ctx, agg.ID().UUID, nil); err == nil {
		t.Error("the order still loaded after the reset")
	}

	// ...and so is the read model built from it.
	summaries, err = rm.list(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 0 {
		t.Errorf("read model rows after reset = %d, want 0", len(summaries))
	}

	if pending, err := rm.pendingOutbox(ctx); err != nil {
		t.Fatal(err)
	} else if pending != 0 {
		t.Errorf("pending outbox rows after reset = %d, want 0", pending)
	}

	if got := srv.log.recent(); len(got) != 0 {
		t.Errorf("delivery log after reset = %d entries, want 0", len(got))
	}

	select {
	case <-watcher:
	case <-time.After(time.Second):
		t.Error("connected clients were not told about the reset")
	}

	// Drive the scheduler loop itself, at 50ms instead of an hour: it is the
	// one piece of demo-only code that runs unattended and would fail silently.
	loopCtx, stopLoop := context.WithCancel(ctx)
	defer stopLoop()

	go srv.runResets(loopCtx, func(now time.Time) time.Time { return now.Add(50 * time.Millisecond) })

	for i := range 2 {
		select {
		case <-watcher:
		case <-time.After(5 * time.Second):
			t.Fatalf("scheduler stopped after %d resets", i)
		}
	}

	stopLoop()

	time.Sleep(200 * time.Millisecond)
	for len(watcher) > 0 {
		<-watcher
	}
	select {
	case <-watcher:
		t.Error("the scheduler kept resetting after its context was cancelled")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestDeliveryLogReset(t *testing.T) {
	t.Parallel()

	log := newDeliveryLog(4)
	for i := range 6 { // deliberately wrap the ring buffer
		log.add(delivery{EventType: "orderpaid", StreamVersion: int64(i)})
	}
	if len(log.recent()) != 4 {
		t.Fatalf("log holds %d entries, want 4", len(log.recent()))
	}

	log.reset()

	if got := log.recent(); len(got) != 0 {
		t.Errorf("log after reset = %d entries, want 0", len(got))
	}

	// the buffer is still usable afterwards
	log.add(delivery{EventType: "ordershipped"})
	if got := log.recent(); len(got) != 1 {
		t.Errorf("log after adding post-reset = %d entries, want 1", len(got))
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
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/orders", nil))
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
			req := httptest.NewRequest(http.MethodPost, "/api/orders", nil)
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

		req := httptest.NewRequest(http.MethodPost, "/api/orders", nil)
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

// TestPathOrderID covers the {id} segment without needing a database: every
// case is rejected before the handler touches a store, which is the point —
// the nil UUID used to reach estoria and come back as a 500.
func TestPathOrderID(t *testing.T) {
	t.Parallel()

	handler := (&server{}).routes()

	for _, tc := range []struct {
		name string
		id   string
		want int
	}{
		{"malformed", "not-a-uuid", http.StatusBadRequest},
		{"nil UUID", "00000000-0000-0000-0000-000000000000", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			for _, target := range []string{
				"GET /api/orders/" + tc.id,
				"POST /api/orders/" + tc.id + "/pay",
			} {
				method, path, _ := strings.Cut(target, " ")
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(method, path, strings.NewReader(`{"baseVersion":1}`))
				req.Header.Set("Content-Type", "application/json")
				handler.ServeHTTP(rec, req)

				if rec.Code != tc.want {
					t.Errorf("%s = %d, want %d (body: %s)", target, rec.Code, tc.want, rec.Body.String())
				}
			}
		})
	}
}
