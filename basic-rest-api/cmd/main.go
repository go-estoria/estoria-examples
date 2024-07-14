package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-estoria/estoria-examples/basic-rest-api/internal/application"
	"github.com/go-estoria/estoria-examples/basic-rest-api/internal/storage"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/go-estoria/estoria/eventstore/memory"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if os.Getenv("DEBUG") == "true" {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})))
	}

	// create an event store (in-memory) to store events
	eventStore := memory.NewEventStore()

	// create an aggregate store to load and store Accounts
	aggregateStore, err := aggregatestore.NewEventSourcedAggregateStore(eventStore, storage.NewAccount)
	if err != nil {
		slog.Error("creating aggregate store", "error", err)
		return
	}

	// create the storage client used by the app's request handlers
	// for interacting with the database using event sourcing
	stg := storage.NewClient(aggregateStore)

	// create an HTTP server to serve the REST API
	httpServer := &http.Server{
		Addr: ":8080",
	}

	// create and run the application, injecting the HTTP server and storage client as dependencies
	app := application.New(httpServer, stg)
	app.Run(ctx)

	// graceful shutdown
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig

	slog.Info("shutting down")
}
