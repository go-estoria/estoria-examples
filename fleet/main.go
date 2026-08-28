// Command fleet is an IoT sensor-fleet dashboard backed by estoria.
//
// An in-process simulator plays the role of the sensors: every device is an
// aggregate with its own event stream in a local SQLite database, receiving a
// reading every few seconds, indefinitely. That event volume is the point —
// the app demonstrates, end to end:
//
//   - long, ever-growing streams (one per device, thousands of events)
//   - the full aggregate store decorator stack:
//     broadcasting -> cached (bigcache) -> snapshotting -> event-sourced
//   - snapshots as events in parallel streams (no extra infrastructure)
//   - aggregate caching, so hot loads skip storage entirely
//   - a live hydration benchmark: cold replay vs snapshot vs cache hit
//   - live dashboard updates via an app-local save decorator broadcasting over SSE
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
	// hourlyReset clears the fleet's history at the top of every hour. Unlike
	// the other examples this matters for more than tidiness: the simulator
	// appends continuously, so the streams would otherwise grow without bound.
	hourlyReset bool

	// pauseWhenIdleAfter stops the simulator once nobody has been watching for
	// this long, and restarts it when someone connects. Zero runs it
	// continuously, which is what a local run wants.
	pauseWhenIdleAfter time.Duration

	// writesPerMinute caps state-changing requests per client IP (0 disables).
	writesPerMinute int

	// trustProxy reads the client IP from X-Forwarded-For. Only safe behind a
	// proxy that overwrites that header.
	trustProxy bool

	// maxClients caps concurrent SSE connections (0 disables).
	maxClients int
}

func main() {
	addr := flag.String("addr", defaultAddr(":8083"), "HTTP listen address")
	dbPath := flag.String("db", "fleet.db", "path to the SQLite database file")
	devices := flag.Int("devices", 12, "number of simulated devices")
	snapshotEvery := flag.Int64("snapshot-every", 200, "take an aggregate snapshot every N events")

	var demo demoConfig
	flag.BoolVar(&demo.hourlyReset, "hourly-reset", false,
		"clear the fleet's history and re-register devices at the top of every hour (for public demos)")
	flag.DurationVar(&demo.pauseWhenIdleAfter, "pause-when-idle", 0,
		"pause the simulator once nobody has been watching for this long (0 runs it continuously)")
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

	if err := run(ctx, *addr, *dbPath, *devices, *snapshotEvery, demo); err != nil {
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

func run(ctx context.Context, addr, dbPath string, devices int, snapshotEvery int64, demo demoConfig) error {
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

	// 4. broadcastingStore: an app-local decorator. Pushing every saved
	//    change to all SSE clients is what makes the dashboard live.
	broadcasts := newHub(demo.maxClients)
	live := broadcastingStore{Store: cached, hub: broadcasts}

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
		if err := registerDevices(ctx, live, reg, missing); err != nil {
			return fmt.Errorf("registering devices: %w", err)
		}
		estoria.GetLogger().Info("registered devices", "count", missing, "fleet_size", reg.count())
	}

	// the simulator is the fleet: one goroutine per device, writing through
	// the full decorated stack
	sim := newSimulator(ctx, live, reg)
	defer sim.stop()

	// Started here unless the idle supervisor owns it (see demo.go), in which
	// case it starts when the first client connects.
	if demo.pauseWhenIdleAfter <= 0 {
		sim.start()
	}

	srv := &server{
		live:          live,
		snapshotting:  snapshotting,
		history:       eventSourced,
		events:        eventStore,
		snapshots:     snapStore,
		cache:         cacheBackend,
		db:            db,
		deviceCount:   devices,
		reg:           reg,
		sim:           sim,
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
	if demo.pauseWhenIdleAfter > 0 {
		go srv.runIdleSupervisor(ctx, demo.pauseWhenIdleAfter)
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

// broadcastingStore decorates an aggregate store, pushing every successfully
// saved device to all SSE clients.
type broadcastingStore struct {
	aggregatestore.Store[Device]
	hub *hub
}

func (s broadcastingStore) Save(ctx context.Context, aggregate *aggregatestore.Aggregate[Device], opts *aggregatestore.SaveOptions) error {
	if err := s.Store.Save(ctx, aggregate, opts); err != nil {
		return err
	}

	s.hub.broadcast(deviceMessage{Version: aggregate.Version(), Device: aggregate.State()})

	return nil
}
