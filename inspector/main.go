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
	addr := flag.String("addr", ":8085", "HTTP listen address")
	backendName := flag.String("backend", "sqlite", "event store backend ("+strings.Join(backendNames(), ", ")+")")
	dsn := flag.String("dsn", "../kanban/kanban.db", "backend DSN (sqlite: path to the database file; postgres: connection URL)")
	flag.Parse()

	if os.Getenv("DEBUG") != "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})))
	}

	estoria.SetLogger(estoria.DefaultLogger())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *addr, *backendName, *dsn); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, addr, backendName, dsn string) error {
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
