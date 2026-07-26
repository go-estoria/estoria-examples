// Command kanban is a collaborative kanban board backed by estoria.
//
// Every change to the board is an event appended to a single event stream in
// a local SQLite database. The app demonstrates, end to end:
//
//   - aggregate modeling with pure ApplyTo event transitions
//   - the aggregate store decorator stack: hooks -> snapshotting -> event-sourced
//   - live collaboration via an AfterSave hook broadcasting over SSE
//   - time travel with LoadOptions.ToVersion
//   - optimistic concurrency surfaced as HTTP 409s
//   - stream projections (the activity feed)
//   - snapshots stored as events in a parallel stream (no extra infrastructure)
//
// Run it with no arguments and open http://localhost:8080. No Docker required.
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
	"github.com/go-estoria/estoria/snapshotstore"
	streamsnapshots "github.com/go-estoria/estoria/snapshotstore/eventstream"
	"github.com/go-estoria/estoria/typeid"
	"github.com/gofrs/uuid/v5"
	_ "modernc.org/sqlite"
)

// A fixed board ID so the same board is loaded across restarts.
var boardUUID = uuid.Must(uuid.FromString("e5701a1a-b0a2-4d00-8000-000000000001"))

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
	// hourlyReset clears and reseeds the board at the top of every hour.
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
	addr := flag.String("addr", defaultAddr(":8080"), "HTTP listen address")
	dbPath := flag.String("db", "kanban.db", "path to the SQLite database file")
	snapshotEvery := flag.Int64("snapshot-every", 10, "take an aggregate snapshot every N events")

	var demo demoConfig
	flag.BoolVar(&demo.hourlyReset, "hourly-reset", false,
		"clear and reseed the board at the top of every hour (for public demos)")
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

	if err := run(ctx, *addr, *dbPath, *snapshotEvery, demo); err != nil {
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

func run(ctx context.Context, addr, dbPath string, snapshotEvery int64, demo demoConfig) error {
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

	// the default strategy stores all streams in a single table
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

	// The aggregate store stack, innermost first. Each layer implements
	// aggregatestore.Store[Board], so they compose freely.

	// 1. EventSourcedStore: hydrates by replaying events, saves with
	//    optimistic concurrency (ExpectVersion).
	eventSourced, err := aggregatestore.New(eventStore, NewBoard,
		aggregatestore.WithEventTypes(boardEventPrototypes()...))
	if err != nil {
		return fmt.Errorf("creating aggregate store: %w", err)
	}

	// 2. SnapshottingStore: after every N events, writes a snapshot; loads
	//    start from the latest snapshot instead of replaying from scratch.
	//    Snapshots are stored as events in a parallel "boardsnapshot" stream
	//    in the same SQLite database — no separate snapshot storage needed.
	snapshotting, err := aggregatestore.NewSnapshottingStore[Board](
		eventSourced,
		streamsnapshots.NewEventStreamStore(eventStore),
		snapshotstore.EventCountSnapshotPolicy{N: snapshotEvery},
	)
	if err != nil {
		return fmt.Errorf("creating snapshotting store: %w", err)
	}

	// 3. HookableStore: lifecycle hooks. The AfterSave hook is what makes the
	//    app collaborative: every saved change is pushed to all SSE clients.
	hookable, err := aggregatestore.NewHookableStore[Board](snapshotting)
	if err != nil {
		return fmt.Errorf("creating hookable store: %w", err)
	}

	broadcasts := newHub(demo.maxClients)
	hookable.AfterSave(func(_ context.Context, agg *aggregatestore.Aggregate[Board]) error {
		broadcasts.broadcast(boardMessage{Version: agg.Version(), Live: true, Board: agg.Entity()})
		return nil
	})

	// seed a demo board on first run
	if _, err := eventSourced.Load(ctx, boardUUID, nil); errors.Is(err, aggregatestore.ErrAggregateNotFound) {
		if err := seedBoard(ctx, hookable, boardUUID); err != nil {
			return fmt.Errorf("seeding board: %w", err)
		}
		estoria.GetLogger().Info("seeded demo board", "board_id", typeid.New("board", boardUUID))
	} else if err != nil {
		return fmt.Errorf("loading board: %w", err)
	}

	srv := &server{
		boardID:       boardUUID,
		live:          hookable,
		history:       eventSourced,
		events:        eventStore,
		db:            db,
		hub:           broadcasts,
		snapshotEvery: snapshotEvery,
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
	fmt.Printf("kanban board running at http://%s (db: %s)\n", displayAddr, dbPath)

	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

// seedBoard populates a fresh board with enough history to make the time
// slider interesting, using cards that double as a guided tour of the app.
func seedBoard(ctx context.Context, store aggregatestore.Store[Board], id uuid.UUID) error {
	newID := func(kind string) string { return typeid.NewV7(kind).String() }

	todo, doing, done := newID("column"), newID("column"), newID("column")
	dragCard := newID("card")
	tabsCard := newID("card")
	editCard := newID("card")
	sliderCard := newID("card")
	snapshotCard := newID("card")
	conflictCard := newID("card")

	agg := store.New(id)
	if err := agg.Append(
		BoardCreated{Name: "Estoria Kanban"},
		ColumnAdded{ColumnID: todo, Title: "To Do"},
		ColumnAdded{ColumnID: doing, Title: "In Progress"},
		ColumnAdded{ColumnID: done, Title: "Done"},
		CardAdded{CardID: dragCard, ColumnID: todo, Title: "Drag a card to another column",
			Description: "Every drop appends a CardMoved event to the board's event stream. Nothing is ever updated in place.", Color: "blue"},
		CardAdded{CardID: tabsCard, ColumnID: todo, Title: "Open this app in a second tab",
			Description: "An AfterSave hook on the aggregate store broadcasts every change over SSE, so all tabs stay in sync.", Color: "purple"},
		CardAdded{CardID: editCard, ColumnID: todo, Title: "Click a card to edit it",
			Description: "Edits append CardEdited events. The old state isn't lost — scrub the timeline to see it again.", Color: "teal"},
		CardAdded{CardID: sliderCard, ColumnID: todo, Title: "Scrub the timeline below",
			Description: "The server rebuilds the board at any version using LoadOptions.ToVersion. This is just a replay — no special storage.", Color: "amber"},
		CardAdded{CardID: snapshotCard, ColumnID: todo, Title: "Watch a snapshot happen",
			Description: "Open Under the Hood and make some changes: every 10 events, the SnapshottingStore writes a snapshot to a parallel event stream.", Color: "pink"},
		CardAdded{CardID: conflictCard, ColumnID: todo, Title: "Trigger a version conflict",
			Description: "The ⚡ button in Under the Hood sends a command based on a stale version. Estoria's optimistic concurrency rejects it with a version mismatch.", Color: "red"},
		CardMoved{CardID: sliderCard, ToColumn: doing, ToIndex: 0},
		CardMoved{CardID: snapshotCard, ToColumn: doing, ToIndex: 1},
		CardEdited{CardID: dragCard, Title: "Drag a card to another column",
			Description: "Every drop appends a CardMoved event to the board's event stream. Nothing is ever updated in place. (This description was itself a CardEdited event — check the activity feed.)", Color: "blue"},
	); err != nil {
		return err
	}

	return store.Save(ctx, agg, nil)
}
