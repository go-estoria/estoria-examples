// Command chess is a live two-player chess server backed by estoria.
//
// Every game is an event stream of moves in a local SQLite database — which
// means time travel is native to the domain: replaying the stream to version
// N reproduces the exact position after N-1 moves. The app demonstrates,
// end to end:
//
//   - aggregate modeling where the rules engine lives in pure ApplyTo
//     transitions (an illegal move is an event the domain rejects)
//   - one aggregate per game: many short streams in one event store
//   - live play via an AfterSave hook broadcasting over SSE
//   - full game replay with LoadOptions.ToVersion
//   - optimistic concurrency as turn-race protection, surfaced as HTTP 409s
//   - deriving artifacts (SAN move lists, PGN exports) from the stream
//
// Run it with no arguments and open http://localhost:8084. No Docker required.
package main

import (
	"context"
	"database/sql"
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
	sqlstore "github.com/go-estoria/estoria-contrib/sqlite/eventstore"
	sqlstrategy "github.com/go-estoria/estoria-contrib/sqlite/eventstore/strategy"
	"github.com/go-estoria/estoria/aggregatestore"
	_ "modernc.org/sqlite"
)

// The storage strategy's table names. They match its defaults, but are named
// here because the demo reset deletes from them directly (see demo.go), and
// that coupling should be visible rather than implied.
const (
	eventsTable  = "event"
	streamsTable = "stream"
)

// demoConfig holds the settings that only matter when this example is hosted
// publicly. Every one of them is inert by default: run the example locally and
// it behaves exactly as it did before any of this existed.
type demoConfig struct {
	// hourlyReset deletes every game at the top of every hour.
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
	addr := flag.String("addr", defaultAddr(":8084"), "HTTP listen address")
	dbPath := flag.String("db", "chess.db", "path to the SQLite database file")

	var demo demoConfig
	flag.BoolVar(&demo.hourlyReset, "hourly-reset", false,
		"delete every game at the top of every hour (for public demos)")
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

	if err := run(ctx, *addr, *dbPath, demo); err != nil {
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

func run(ctx context.Context, addr, dbPath string, demo demoConfig) error {
	// SQLite via a pure-Go driver: persistent, transactional, and no server
	// to run. WAL mode lets reads proceed while a write is in flight.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("pinging database: %w", err)
	}

	// the default strategy stores all streams in a single table, so every
	// game — past and present — lives in one SQLite file
	strat, err := sqlstrategy.NewDefaultStrategy(
		sqlstrategy.WithEventsTableName(eventsTable),
		sqlstrategy.WithStreamsTableName(streamsTable),
	)
	if err != nil {
		return fmt.Errorf("creating strategy: %w", err)
	}
	if _, err := db.ExecContext(ctx, strat.Schema()); err != nil {
		return fmt.Errorf("creating schema: %w", err)
	}

	eventStore, err := sqlstore.New(db, sqlstore.WithStrategy(strat))
	if err != nil {
		return fmt.Errorf("creating event store: %w", err)
	}

	// The aggregate store stack, innermost first. Chess games are short
	// streams (a long game is a few hundred events), so there is no
	// snapshotting layer here: replaying from the start is instant, and it
	// keeps version-pinned loads (the replay slider) trivially correct.

	// 1. EventSourcedStore: hydrates by replaying events, saves with
	//    optimistic concurrency (ExpectVersion).
	eventSourced, err := aggregatestore.New(eventStore, NewGame,
		aggregatestore.WithEventTypes(gameEventPrototypes()...))
	if err != nil {
		return fmt.Errorf("creating aggregate store: %w", err)
	}

	// 2. HookableStore: lifecycle hooks. The AfterSave hook is what makes the
	//    app multiplayer: every saved move is pushed to all SSE clients.
	hookable, err := aggregatestore.NewHookableStore[Game](eventSourced)
	if err != nil {
		return fmt.Errorf("creating hookable store: %w", err)
	}

	broadcasts := newHub(demo.maxClients)
	hookable.AfterSave(func(_ context.Context, agg *aggregatestore.Aggregate[Game]) error {
		broadcasts.broadcast(newGameMessage(agg, true))
		return nil
	})

	srv := &server{
		live:    hookable,
		history: eventSourced,
		events:  eventStore,
		db:      db,
		hub:     broadcasts,
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
	fmt.Printf("chess server running at http://%s (db: %s)\n", displayAddr, dbPath)

	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}
