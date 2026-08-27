// Command orders is an order-fulfillment service backed by estoria.
//
// Every order is an aggregate with its own event stream in Postgres, moving
// through a fulfillment state machine (placed -> paid -> picked -> shipped ->
// delivered, with cancellation allowed any time before shipping). The app
// demonstrates, end to end:
//
//   - aggregate modeling with pure ApplyTo transitions, the fulfillment
//     state machine validated in command handlers before events are appended
//   - the Postgres event store with the transactional outbox: events and
//     outbox rows commit in ONE transaction, so deliveries are never lost
//     and never phantom
//   - CQRS: commands write event streams; the order list reads a Postgres
//     read-model table (order_summaries) maintained ONLY by the outbox
//     processor
//   - eventual consistency made visible: the outbox monitor shows each
//     delivery as it lands
//   - optimistic concurrency surfaced as HTTP 409s
//   - raw stream reads powering each order's event timeline
//
// Run `make up` to start Postgres, then `make run` and open
// http://localhost:8082.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-estoria/estoria"
	pgeventstore "github.com/go-estoria/estoria-contrib/postgres/eventstore"
	pgstrategy "github.com/go-estoria/estoria-contrib/postgres/eventstore/strategy"
	pgoutbox "github.com/go-estoria/estoria-contrib/postgres/outbox"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The storage strategy's table names. They match its defaults, but are named
// here because the demo reset truncates them directly (see demo.go), and that
// coupling should be visible rather than implied. The other two tables the
// reset clears are named in readmodel.go.
const (
	eventsTable  = "event"
	streamsTable = "stream"
)

// demoConfig holds the settings that only matter when this example is hosted
// publicly. Every one of them is inert by default: run the example locally and
// it behaves exactly as it did before any of this existed.
type demoConfig struct {
	// hourlyReset deletes every order at the top of every hour.
	hourlyReset bool

	// writesPerMinute caps state-changing requests per client IP (0 disables).
	writesPerMinute int

	// trustProxy reads the client IP from X-Forwarded-For. Only safe behind a
	// proxy that overwrites that header.
	trustProxy bool

	// maxClients caps concurrent SSE connections (0 disables).
	maxClients int
}

func main() {
	addr := flag.String("addr", defaultAddr(":8082"), "HTTP listen address")
	dsn := flag.String("dsn", defaultDSN("postgres://estoria:estoria@localhost:5433/estoria?sslmode=disable"),
		"Postgres DSN (the default matches docker-compose.yml, or $DATABASE_URL when set)")

	var demo demoConfig
	flag.BoolVar(&demo.hourlyReset, "hourly-reset", false,
		"delete every order at the top of every hour (for public demos)")
	flag.IntVar(&demo.writesPerMinute, "writes-per-minute", 0,
		"per-IP limit on state-changing requests (0 disables)")
	flag.BoolVar(&demo.trustProxy, "trust-proxy", false,
		"read the client IP from X-Forwarded-For (only behind a trusted proxy)")
	flag.IntVar(&demo.maxClients, "max-clients", 0,
		"maximum concurrent live (SSE) connections (0 disables)")

	flag.Parse()

	if os.Getenv("DEBUG") != "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})))
	}

	estoria.SetLogger(estoria.DefaultLogger())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *addr, *dsn, demo); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// defaultAddr honors the PORT environment variable, which is how container
// platforms (Railway, Fly, Cloud Run, ...) tell a process where to listen.
// An explicit -addr still wins, since flag parsing overrides this default.
func defaultAddr(fallback string) string {
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return fallback
}

// defaultDSN honors DATABASE_URL, which is how those same platforms hand an
// app its managed Postgres. An explicit -dsn still wins.
func defaultDSN(fallback string) string {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}
	return fallback
}

func run(ctx context.Context, addr, dsn string, demo demoConfig) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("creating connection pool: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("pinging database (is Postgres up? try `make up`): %w", err)
	}

	// the default strategy stores all streams in a single table
	strat, err := pgstrategy.NewDefaultStrategy(
		pgstrategy.WithEventsTableName(eventsTable),
		pgstrategy.WithStreamsTableName(streamsTable),
	)
	if err != nil {
		return fmt.Errorf("creating strategy: %w", err)
	}
	if _, err := pool.Exec(ctx, strat.Schema()); err != nil {
		return fmt.Errorf("creating event store schema: %w", err)
	}

	// the read model lives in the same database as the event store, but only
	// the outbox processor ever writes to it
	rm := newReadModel(pool)
	if _, err := pool.Exec(ctx, rm.schema()); err != nil {
		return fmt.Errorf("creating read model schema: %w", err)
	}

	broadcasts := newHub(demo.maxClients)
	webhookLog := newDeliveryLog(64)

	// The outbox handler is the sole writer of the read model. The processor
	// calls it once per event, in strict per-stream FIFO order, at least once.
	// After projecting the event it records a "webhook delivery" and notifies
	// SSE clients that the read model advanced.
	ob, err := pgoutbox.New(pool, func(ctx context.Context, item *pgoutbox.Item) error {
		if err := rm.apply(ctx, item); err != nil {
			return err // the item is retried; its stream halts until it succeeds
		}

		d := delivery{
			EventType:     item.EventID.Type,
			OrderID:       item.StreamID.String(),
			StreamVersion: item.StreamVersion,
			DeliveredAt:   time.Now().UTC(),
		}
		webhookLog.add(d)
		broadcasts.broadcast(map[string]any{"type": "delivery", "delivery": d})
		return nil
	}, pgoutbox.WithPollInterval(250*time.Millisecond))
	if err != nil {
		return fmt.Errorf("creating outbox: %w", err)
	}
	if _, err := pool.Exec(ctx, ob.Schema()); err != nil {
		return fmt.Errorf("creating outbox schema: %w", err)
	}

	// The outbox is registered as an append transaction hook: every event
	// append inserts matching outbox rows in the SAME database transaction.
	// If either write fails, both roll back — no lost deliveries (an event
	// without an outbox row) and no phantom deliveries (an outbox row for an
	// event that was never committed).
	eventStore, err := pgeventstore.New(pool,
		pgeventstore.WithStrategy(strat),
		pgeventstore.WithAppendTransactionHooks(ob),
	)
	if err != nil {
		return fmt.Errorf("creating event store: %w", err)
	}

	// The aggregate store stack, innermost first. Order streams are short —
	// seven events at most — so there is no snapshotting layer here; replaying
	// from scratch is already optimal. (See the kanban example for snapshots.)

	// 1. EventSourcedStore: hydrates by replaying events, saves with
	//    optimistic concurrency (ExpectVersion). The aggregate type name
	//    becomes part of every aggregate's stream address.
	eventSourced, err := aggregatestore.New(eventStore, "order", NewOrder,
		aggregatestore.WithEventTypes(orderEventPrototypes()...))
	if err != nil {
		return fmt.Errorf("creating aggregate store: %w", err)
	}

	// 2. broadcastingStore: an app-local decorator that tells SSE clients a
	//    command was accepted — the write side of the CQRS split. The read
	//    model catches up moments later via the outbox (the "delivery"
	//    broadcasts above).
	orderStore := broadcastingStore{Store: eventSourced, hub: broadcasts}

	// the outbox processor polls for undelivered items until shutdown
	go func() {
		if err := ob.Run(ctx); err != nil {
			estoria.GetLogger().Error("outbox processor", "error", err)
		}
	}()

	srv := &server{
		orders:    orderStore,
		events:    eventStore,
		readModel: rm,
		pool:      pool,
		hub:       broadcasts,
		log:       webhookLog,
	}

	// Hosted-demo behavior, all off by default (see demoConfig).
	handler := srv.routes()
	if demo.writesPerMinute > 0 {
		limiter := newRateLimiter(demo.writesPerMinute, demo.trustProxy)
		go limiter.runSweeper(ctx)
		handler = limiter.middleware(handler)
	}
	if demo.hourlyReset {
		go srv.runHourlyReset(ctx)
	}

	httpServer := &http.Server{Addr: addr, Handler: handler}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	displayAddr := addr
	if displayAddr[0] == ':' {
		displayAddr = "localhost" + displayAddr
	}
	fmt.Printf("order service running at http://%s (db: %s)\n", displayAddr, dsn)

	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

// broadcastingStore decorates an aggregate store, pushing every successfully
// saved order to all SSE clients.
type broadcastingStore struct {
	aggregatestore.Store[Order]
	hub *hub
}

func (s broadcastingStore) Save(ctx context.Context, aggregate *aggregatestore.Aggregate[Order], opts *aggregatestore.SaveOptions) error {
	if err := s.Store.Save(ctx, aggregate, opts); err != nil {
		return err
	}

	s.hub.broadcast(orderMessage{Type: "order", Version: aggregate.Version(), Order: aggregate.State()})

	return nil
}
