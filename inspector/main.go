// Command inspector is a generic, read-only event stream inspector for
// estoria event stores.
//
// Point it at an existing event store (SQLite file or Postgres database) and
// browse the streams inside: page through a stream's events in either
// direction, expand event payloads, and tail the global event feed.
//
// The inspector is also a small case study in interface design. Estoria's core
// eventstore.Store interface is deliberately tiny — ReadStream and
// AppendStream. Everything stream-scoped here (event paging, in both
// directions) works against ANY backend using only the core
// eventstore.StreamReader. Listing streams and reading the global feed are
// backend-specific extras, modeled in backend.go as optional capabilities that
// the UI degrades gracefully without. See the README for the design question
// this demonstrates.
//
// This tool is strictly read-only: it never calls AppendStream, and the server
// only ever holds an eventstore.StreamReader.
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
	"strings"
	"syscall"
	"time"

	"github.com/go-estoria/estoria"
)

func main() {
	addr := flag.String("addr", defaultAddr(":8085"), "HTTP listen address")
	backendName := flag.String("backend", "sqlite", "event store backend ("+strings.Join(backendNames(), ", ")+")")
	dsn := flag.String("dsn", defaultDSN("../kanban/kanban.db"), "backend DSN (sqlite: path to the database file; postgres: connection URL) — or $DATABASE_URL")

	// Hosted-demo limits. The inspector never writes, so there is nothing to
	// reset and no commands to throttle; what needs bounding is read cost.
	// Every /api/all/tail is a full forward scan (see server.go), so on a
	// public URL it is the one endpoint worth metering.
	readsPerMinute := flag.Int("reads-per-minute", 0, "per-IP limit on scan-heavy reads (0 disables)")
	trustProxy := flag.Bool("trust-proxy", false, "read the client IP from X-Forwarded-For (only behind a trusted proxy)")

	flag.Parse()

	if os.Getenv("DEBUG") != "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})))
	}

	estoria.SetLogger(estoria.DefaultLogger())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *addr, *backendName, *dsn, *readsPerMinute, *trustProxy); err != nil {
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

// defaultDSN honors DATABASE_URL, so a hosted inspector can be pointed at a
// managed Postgres without a command-line change. An explicit -dsn still wins.
func defaultDSN(fallback string) string {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}
	return fallback
}

func run(ctx context.Context, addr, backendName, dsn string, readsPerMinute int, trustProxy bool) error {
	entry, ok := backends[backendName]
	if !ok {
		return fmt.Errorf("unknown backend %q (available: %s)", backendName, strings.Join(backendNames(), ", "))
	}

	b, err := entry.connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connecting to %s backend: %w", backendName, err)
	}
	defer b.close()

	srv := &server{backend: b}

	handler := srv.routes()
	if readsPerMinute > 0 {
		limiter := newRateLimiter(readsPerMinute, trustProxy)
		go limiter.runSweeper(ctx)
		handler = srv.routesWithScanLimit(limiter)
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
	fmt.Printf("estoria inspector (read-only) at http://%s\n", displayAddr)
	fmt.Printf("  backend:     %s (%s)\n", b.label, redactDSN(b.dsn))
	fmt.Printf("  ListStreams: %s\n", capabilityStatus(b.caps.listStreams != nil))
	fmt.Printf("  ReadAll:     %s\n", capabilityStatus(b.caps.readAll != nil))

	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

func capabilityStatus(available bool) string {
	if available {
		return "available"
	}
	return "unavailable (UI degrades gracefully)"
}
