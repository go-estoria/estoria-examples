// Command ledger is a bank-ledger service whose read model can be rebuilt
// live, blue/green, while the service keeps serving. Every account is an
// aggregate with its own event stream in Postgres; a versioned projection
// (account_balances_v1, account_balances_v2, ...) serves the account list;
// and a rebuild console drives the full projection lifecycle. The app
// demonstrates, end to end:
//
//   - versioned projection identity: each rebuild targets a fresh version
//     with its own table and checkpoint, so building v2 cannot disturb the
//     v1 still serving reads
//   - the projection lifecycle orchestrator: Begin/Resume, a Run that
//     catches up and tails, and Promote, Rollback, Abandon, and Retire as
//     explicit operator commands with optimistic concurrency arbitrating
//     competing decisions
//   - logical cutover: reads consult a Router for the live version and
//     compose the table name per query; a cutover worker converges the
//     router on recorded promotions and rollbacks
//   - checkpointed, at-least-once processing made safe by per-row
//     apply-if-newer guards, with checkpoint recency as the liveness signal
//   - crash recovery: a standing runner claim refuses a transparent second
//     run, and the console performs an audited operator takeover
//   - governed retirement: a durable witness policy gates destroying the
//     previous version's storage, with an audited override
//
// Run `make up` to start Postgres, then `make run` and open
// http://localhost:8084.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-estoria/estoria"
	pgeventstore "github.com/go-estoria/estoria-contrib/postgres/eventstore"
	pgstrategy "github.com/go-estoria/estoria-contrib/postgres/eventstore/strategy"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/go-estoria/estoria/projection/lifecycle"
	"github.com/go-estoria/estoria/projection/processor"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	addr := flag.String("addr", defaultAddr(":8084"), "HTTP listen address")
	dsn := flag.String("dsn", defaultDSN("postgres://estoria:estoria@localhost:5434/estoria?sslmode=disable"),
		"Postgres DSN (the default matches docker-compose.yml, or $DATABASE_URL when set)")

	// Hosted-demo settings, inert by default (see demo.go).
	resetOnBoot := flag.Bool("reset-on-boot", false,
		"drop every table this app owns at startup, so each deploy starts clean (for public demos)")
	writesPerMinute := flag.Int("writes-per-minute", 0,
		"per-IP limit on state-changing requests (0 disables)")
	trustProxy := flag.Bool("trust-proxy", false,
		"read the client IP from X-Forwarded-For (only behind a trusted proxy)")

	flag.Parse()

	estoria.SetLogger(estoria.DefaultLogger())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *addr, *dsn, *resetOnBoot, *writesPerMinute, *trustProxy); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, addr, dsn string, resetOnBoot bool, writesPerMinute int, trustProxy bool) error {
	// The whole app winds down together: a fatal background failure cancels
	// this context the same way a signal does.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	log := estoria.GetLogger()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("creating connection pool: %w", err)
	}
	defer pool.Close()

	// A hosted demo starts from nothing each deploy: wipe before the schema
	// below recreates it, which also heals a schema left behind by an older
	// build (see demo.go).
	if resetOnBoot {
		if err := resetStore(ctx, pool); err != nil {
			return fmt.Errorf("resetting store at boot: %w", err)
		}
		estoria.GetLogger().Info("reset store at boot")
	}

	// 1. The event store: domain streams and the projection's lifecycle
	// stream share it, so one global sequence orders everything and one
	// optimistic-concurrency domain arbitrates every lifecycle decision.
	strat, err := pgstrategy.NewDefaultStrategy()
	if err != nil {
		return fmt.Errorf("creating strategy: %w", err)
	}

	if _, err := pool.Exec(ctx, strat.Schema()); err != nil {
		return fmt.Errorf("creating event store schema: %w", err)
	}

	eventStore, err := pgeventstore.New(pool, pgeventstore.WithStrategy(strat))
	if err != nil {
		return fmt.Errorf("creating event store: %w", err)
	}

	// 2. The write side: accounts as event-sourced aggregates.
	accounts, err := aggregatestore.New(eventStore, "account", NewAccount,
		aggregatestore.WithEventTypes(accountEventPrototypes()...))
	if err != nil {
		return fmt.Errorf("creating aggregate store: %w", err)
	}

	// 3. The read side: versioned balance tables, and durable checkpoints
	// for whichever processor is building or tailing each version.
	checkpoints := &checkpointStore{pool: pool}
	if _, err := pool.Exec(ctx, checkpoints.Schema()); err != nil {
		return fmt.Errorf("creating checkpoint schema: %w", err)
	}

	model := &readModel{pool: pool}

	// 4. The lifecycle: the router answers which version serves reads, the
	// orchestrator records and acts on lifecycle decisions, and the cutover
	// worker converges the router on every recorded promotion or rollback.
	// The router is also registered as a retirement witness: destroying a
	// previous version's table requires the router to vouch that it no
	// longer routes reads there.
	router := lifecycle.NewMemoryRouter()

	orchestrator, err := lifecycle.NewOrchestrator(lifecycle.Config{
		Events:          eventStore,
		Checkpoints:     checkpoints,
		Handler:         model.handler,
		LifecycleEvents: eventStore,
	},
		lifecycle.WithProcessorOptions(processor.WithPollInterval(500*time.Millisecond)),
		lifecycle.WithReconcileInterval(3*time.Second),
		lifecycle.WithRetirementWitness("router", router),
	)
	if err != nil {
		return fmt.Errorf("creating lifecycle orchestrator: %w", err)
	}

	worker, err := lifecycle.NewWorker(eventStore,
		lifecycle.WithCutoverSetter(router),
		lifecycle.WithPollInterval(500*time.Millisecond),
	)
	if err != nil {
		return fmt.Errorf("creating cutover worker: %w", err)
	}

	go func() {
		if err := worker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("cutover worker exited; shutting down", "error", err)
			cancel()
		}
	}()

	// Serve nothing until the router holds the recorded routing truth.
	select {
	case <-worker.Ready():
	case <-ctx.Done():
		return ctx.Err()
	}

	// 5. Steady-state serving: a processor tails the live version whenever
	// the lifecycle does not own it.
	serving := &servingManager{
		orchestrator: orchestrator,
		router:       router,
		events:       eventStore,
		checkpoints:  checkpoints,
		handler:      model.handler,
		log:          log,
	}

	go serving.run(ctx)

	// 6. Demo traffic, toggled from the console.
	traffic := &trafficGenerator{accounts: accounts, log: log}
	go traffic.run(ctx)

	srv := &server{
		appCtx:       ctx,
		accounts:     accounts,
		orchestrator: orchestrator,
		router:       router,
		model:        model,
		checkpoints:  checkpoints,
		serving:      serving,
		traffic:      traffic,
		log:          log,
	}

	// Hosted-demo limiting, off unless -writes-per-minute is passed. Reads are
	// never limited: watching a rebuild fill a table is the whole point.
	handler := srv.routes()
	if writesPerMinute > 0 {
		limiter := newRateLimiter(writesPerMinute, trustProxy)
		go limiter.runSweeper(ctx)
		handler = limiter.middleware(handler)
	}

	httpServer := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		<-ctx.Done()

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Error("shutting down HTTP server", "error", err)
		}
	}()

	fmt.Printf("ledger rebuild console listening at http://localhost%s\n", addr)

	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving: %w", err)
	}

	// Join the active rebuild run before the process exits: its wind-down
	// durably releases the runner claim, so the next run resumes
	// transparently instead of needing an operator takeover.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer drainCancel()
	srv.drain(drainCtx)

	return nil
}

// defaultAddr honors the PORT environment variable, which is how container
// platforms hand the app its listen port.
func defaultAddr(fallback string) string {
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}

	return fallback
}

// defaultDSN honors DATABASE_URL, which is how those same platforms hand an
// app its database.
func defaultDSN(fallback string) string {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}

	return fallback
}
