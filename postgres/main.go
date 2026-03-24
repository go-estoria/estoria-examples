package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/go-estoria/estoria"
	pgeventstore "github.com/go-estoria/estoria-contrib/postgres/eventstore"
	pgstrategy "github.com/go-estoria/estoria-contrib/postgres/eventstore/strategy"
	pgoutbox "github.com/go-estoria/estoria-contrib/postgres/outbox"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/go-estoria/estoria/eventstore"
	"github.com/go-estoria/estoria/eventstore/projection"
	"github.com/gofrs/uuid/v5"
	_ "github.com/lib/pq"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if slices.ContainsFunc(os.Args, func(s string) bool { return s == "-h" || s == "--help" }) {
		fmt.Fprintf(os.Stderr, "usage: %s [postgres-dsn]\n", os.Args[0])
		os.Exit(0)
	}

	if os.Getenv("DEBUG") != "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})))
	}

	// the logger defaults to slog but can be adapted as needed
	estoria.SetLogger(estoria.DefaultLogger())

	// default if using 'make up' to spin up Postgres locally
	dsn := "postgres://estoria:estoria@localhost:5432/estoria?sslmode=disable"
	// otherwise just pass a DSN as the first argument to this program
	if len(os.Args) == 2 {
		dsn = os.Args[1]
	}

	// establish a database connection
	db, err := sql.Open("postgres", dsn)
	check(err)

	if err := db.Ping(); err != nil {
		panic(err)
	}

	// the default strategy uses a single table for all events and a metadata table for tracking offsets
	strategy, _ := pgstrategy.NewDefaultStrategy()
	if _, err := db.ExecContext(ctx, strategy.Schema()); err != nil {
		panic(err)
	}

	// create an outbox to reliably deliver events to external consumers;
	// the handler is called for each event as it is processed from the outbox
	outbox, err := pgoutbox.New(db, func(_ context.Context, item *pgoutbox.Item) error {
		fmt.Printf("  outbox: processed %s @%d from stream %s\n", item.EventID.Type, item.StreamVersion, item.StreamID.ShortString())
		return nil
	})
	check(err)

	if _, err := db.ExecContext(ctx, outbox.Schema()); err != nil {
		panic(err)
	}

	eventStore, err := pgeventstore.New(db,
		pgeventstore.WithStrategy(strategy),
		pgeventstore.WithAppendTransactionHooks(outbox),
	)
	check(err)

	var aggregateStore aggregatestore.Store[Account]

	// create an event-sourced store to load and save aggregates using the event store
	aggregateStore, err = aggregatestore.New(eventStore, NewAccount, aggregatestore.WithEventTypes(
		AccountCreatedEvent{},
		AccountDeletedEvent{},
		UserAddedEvent{},
		UserRemovedEvent{},
		BalanceChangedEvent{},
	))
	check(err)

	// create a new Account aggregate
	accountID := uuid.Must(uuid.NewV4())
	aggregate := aggregatestore.NewAggregate(NewAccount(accountID), 0)

	fmt.Printf("created new account:\n  %s\n", aggregate.Entity())

	// append some events to the aggregate
	if err := aggregate.Append(
		AccountCreatedEvent{Username: "Leonardo"},
		BalanceChangedEvent{Amount: +1000, ChangedAt: time.Now().UTC()},
		UserAddedEvent{Username: "Michalangelo"},
		BalanceChangedEvent{Amount: -500, ChangedAt: time.Now().UTC()},
		BalanceChangedEvent{Amount: +250, ChangedAt: time.Now().UTC()},
		UserAddedEvent{Username: "Raphael"},
		UserRemovedEvent{Username: "Michalangelo"},
		BalanceChangedEvent{Amount: -708, ChangedAt: time.Now().UTC()},
	); err != nil {
		panic(err)
	}

	// save the aggregate
	if err := aggregateStore.Save(ctx, aggregate, nil); err != nil {
		panic(err)
	}

	fmt.Printf("saved account:\n  %s\n", aggregate.Entity())

	// process all outbox items; in a real application this would typically
	// run continuously via outbox.Run(ctx) in a separate goroutine
	fmt.Println()
	fmt.Println("processing outbox items:")
	for {
		if err := outbox.ProcessNext(ctx); err != nil {
			if errors.Is(err, pgoutbox.ErrNoItems) {
				break
			}
			panic(err)
		}
	}

	// load the aggregate
	loadedAggregate, err := aggregateStore.Load(ctx, accountID, nil)
	check(err)

	fmt.Printf("loaded account:\n  %s\n", loadedAggregate.Entity())

	//
	// the below demonstrates some lower-level event store operations
	//

	// create an iterator to read events from a specific stream
	iter, err := eventStore.ReadStream(ctx, aggregate.ID(), eventstore.ReadStreamOptions{})
	check(err)

	// create a projection using the event iterator
	proj, err := projection.New(iter)
	check(err)

	// run the projection, simply printing a line for each event
	fmt.Println()
	fmt.Printf("events in stream %s:\n", aggregate.ID())
	_, err = proj.Project(ctx, projection.EventHandlerFunc(func(_ context.Context, evt *eventstore.Event) error {
		fmt.Printf("  %s @%d %s %s\n", evt.StreamID.ShortString(), evt.StreamVersion, evt.Timestamp.Format(time.DateTime), evt.ID.Type)
		return nil
	}))
	check(err)

	// some event stores, such as this one, support listing streams
	streams, err := eventStore.ListStreams()
	check(err)
	fmt.Println()
	fmt.Println("all streams in event store:")
	for _, stream := range streams {
		fmt.Printf("  %s @%d\n", stream.StreamID.ShortString(), stream.LastOffset)
	}

	// some event stores, such as this one, support reading all events in the store (global ordering)
	allIter, err := eventStore.ReadAll(ctx, eventstore.ReadStreamOptions{})
	check(err)

	// create a projection using the "all events" iterator
	allProj, err := projection.New(allIter)
	check(err)

	// run the projection
	fmt.Println()
	fmt.Println("all events in event store:")
	_, err = allProj.Project(ctx, projection.EventHandlerFunc(func(_ context.Context, evt *eventstore.Event) error {
		fmt.Printf("  %s @%d %s\n", evt.StreamID.ShortString(), evt.StreamVersion, evt.ID.ShortString())
		return nil
	}))
	check(err)
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
