// Command fleet is an IoT sensor-fleet dashboard backed by estoria.
//
// An in-process simulator plays the role of the sensors: every device is an
// aggregate with its own event stream in a local SQLite database, receiving a
// reading every few seconds, indefinitely. That event volume is the point —
// the app demonstrates, end to end:
//
//   - long, ever-growing streams (one per device, thousands of events)
//   - the full aggregate store decorator stack:
//     hooks -> cached (bigcache) -> snapshotting -> event-sourced
//   - snapshots as events in parallel streams (no extra infrastructure)
//   - aggregate caching, so hot loads skip storage entirely
//   - a live hydration benchmark: cold replay vs snapshot vs cache hit
//   - live dashboard updates via an AfterSave hook broadcasting over SSE
//
// Run it with no arguments and open http://localhost:8083. No Docker required.
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

	"github.com/allegro/bigcache/v3"
	"github.com/go-estoria/estoria"
	"github.com/go-estoria/estoria-contrib/bigcache/aggregatecache"
	sqlstore "github.com/go-estoria/estoria-contrib/sqlite/eventstore"
	sqlstrategy "github.com/go-estoria/estoria-contrib/sqlite/eventstore/strategy"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/go-estoria/estoria/snapshotstore"
	streamsnapshots "github.com/go-estoria/estoria/snapshotstore/eventstream"
	_ "modernc.org/sqlite"
)

func main() {
	addr := flag.String("addr", ":8083", "HTTP listen address")
	dbPath := flag.String("db", "fleet.db", "path to the SQLite database file")
	devices := flag.Int("devices", 12, "number of simulated devices")
	snapshotEvery := flag.Int64("snapshot-every", 200, "take an aggregate snapshot every N events")
	flag.Parse()

	if os.Getenv("DEBUG") != "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})))
	}

	estoria.SetLogger(estoria.DefaultLogger())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *addr, *dbPath, *devices, *snapshotEvery); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, addr, dbPath string, devices int, snapshotEvery int64) error {
	if devices < 1 {
		return errors.New("-devices must be at least 1")
	}

	// SQLite via a pure-Go driver: persistent, transactional, and no server
	// to run. WAL mode lets reads proceed while a write is in flight — which
	// matters here, because the simulator is writing constantly.
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
	strat, err := sqlstrategy.NewDefaultStrategy()
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
	// aggregatestore.Store[Device], so they compose freely.

	// 1. EventSourcedStore: hydrates by replaying events, saves with
	//    optimistic concurrency (ExpectVersion). Kept un-decorated as the
	//    benchmark's "cold replay" path — the honest baseline that replays a
	//    device's entire stream from version 1.
	eventSourced, err := aggregatestore.New(eventStore, "device", NewDevice,
		aggregatestore.WithEventTypes(deviceEventPrototypes()...))
	if err != nil {
		return fmt.Errorf("creating aggregate store: %w", err)
	}

	// 2. SnapshottingStore: after every N events, writes a snapshot; loads
	//    start from the latest snapshot and replay only the events after it.
	//    Snapshots are stored as events in a parallel "devicesnapshot" stream
	//    in the same SQLite database — no separate snapshot storage needed.
	//    This layer is also the benchmark's "snapshot" path (snapshotting
	//    without caching).
	snapStore, err := streamsnapshots.New(eventStore)
	if err != nil {
		return fmt.Errorf("creating snapshot store: %w", err)
	}

	snapshotting, err := aggregatestore.NewSnapshottingStore(
		eventSourced,
		snapStore,
		snapshotstore.EventCountSnapshotPolicy{N: snapshotEvery},
	)
	if err != nil {
		return fmt.Errorf("creating snapshotting store: %w", err)
	}

	// 3. CachedStore: keeps hydrated aggregates in an in-process bigcache, so
	//    a hot load touches no storage at all. Every save re-populates the
	//    cache, and the simulator saves constantly, so serving reads are
	//    effectively always cache hits.
	cacheBackend, err := bigcache.New(ctx, cacheConfig())
	if err != nil {
		return fmt.Errorf("creating cache: %w", err)
	}
	cached, err := aggregatestore.NewCachedStore(snapshotting, aggregatecache.New[Device](cacheBackend))
	if err != nil {
		return fmt.Errorf("creating cached store: %w", err)
	}

	// 4. HookableStore: lifecycle hooks. The AfterSave hook is what makes the
	//    dashboard live: every saved change is pushed to all SSE clients.
	hookable, err := aggregatestore.NewHookableStore(cached)
	if err != nil {
		return fmt.Errorf("creating hookable store: %w", err)
	}

	broadcasts := newHub()
	hookable.AfterSave(func(_ context.Context, agg *aggregatestore.Aggregate[Device]) error {
		broadcasts.broadcast(deviceMessage{Version: agg.Version(), Device: agg.State()})
		return nil
	})

	// The registry of device IDs is derived from the event store itself:
	// every stream of type "device" is a device. Top up to the requested
	// fleet size on first run (or when -devices grows).
	reg := &registry{}
	streams, err := eventStore.ListStreams(ctx)
	if err != nil {
		return fmt.Errorf("listing streams: %w", err)
	}
	for _, stream := range streams {
		if stream.StreamID.Type == "device" {
			reg.add(stream.StreamID.UUID)
		}
	}
	if missing := devices - reg.count(); missing > 0 {
		if err := registerDevices(ctx, hookable, reg, missing); err != nil {
			return fmt.Errorf("registering devices: %w", err)
		}
		estoria.GetLogger().Info("registered devices", "count", missing, "fleet_size", reg.count())
	}

	// the simulator is the fleet: one goroutine per device, writing through
	// the full decorated stack
	sim := newSimulator(ctx, hookable, reg)
	sim.start()
	defer sim.stop()

	srv := &server{
		live:          hookable,
		snapshotting:  snapshotting,
		history:       eventSourced,
		events:        eventStore,
		snapshots:     snapStore,
		cache:         cacheBackend,
		reg:           reg,
		sim:           sim,
		hub:           broadcasts,
		snapshotEvery: snapshotEvery,
	}

	httpServer := &http.Server{Addr: addr, Handler: srv.routes()}

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
	fmt.Printf("fleet dashboard running at http://%s (db: %s, devices: %d)\n", displayAddr, dbPath, reg.count())

	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

// cacheConfig sizes bigcache for a small fleet of JSON-marshaled aggregates.
// The 10-minute life window only matters when the simulator is paused; while
// it runs, every save refreshes the cached entry.
func cacheConfig() bigcache.Config {
	config := bigcache.DefaultConfig(10 * time.Minute)
	config.Shards = 64
	config.Verbose = false
	return config
}
