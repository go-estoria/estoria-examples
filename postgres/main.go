package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/go-estoria/estoria"
	pgeventstore "github.com/go-estoria/estoria-contrib/postgres/eventstore"
	pgstrategy "github.com/go-estoria/estoria-contrib/postgres/eventstore/strategy"
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

	eventStore, err := pgeventstore.New(db, pgeventstore.WithStrategy(strategy))
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

	fmt.Println("created new account:", aggregate.Entity())

	// append some events to the aggregate
	if err := aggregate.Append(
		AccountCreatedEvent{Username: "Leonardo"},
		BalanceChangedEvent{Amount: +1000},
		UserAddedEvent{Username: "Michalangelo"},
		BalanceChangedEvent{Amount: -500},
		BalanceChangedEvent{Amount: +250},
		UserAddedEvent{Username: "Raphael"},
		UserRemovedEvent{Username: "Michalangelo"},
		BalanceChangedEvent{Amount: -708},
	); err != nil {
		panic(err)
	}

	// save the aggregate
	if err := aggregateStore.Save(ctx, aggregate, aggregatestore.SaveOptions{}); err != nil {
		panic(err)
	}

	fmt.Println("saved account:", aggregate.Entity())

	// load the aggregate
	loadedAggregate, err := aggregateStore.Load(ctx, accountID, aggregatestore.LoadOptions{})
	check(err)

	fmt.Println("loaded account:", loadedAggregate.Entity())

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
	fmt.Printf("events in stream %s:\n", aggregate.ID())
	_, err = proj.Project(ctx, projection.EventHandlerFunc(func(_ context.Context, evt *eventstore.Event) error {
		fmt.Printf("%s @%d %s %s\n", evt.StreamID, evt.StreamVersion, evt.Timestamp.Format(time.DateTime), evt.ID.TypeName())
		return nil
	}))
	check(err)

	// some event stores, such as this one, support listing streams
	streams, err := eventStore.ListStreams()
	check(err)
	fmt.Println("all streams in event store:")
	for _, stream := range streams {
		fmt.Printf("- %s @%d\n", stream.StreamID, stream.LastOffset)
	}

	// some event stores, such as this one, support reading all events in the store
	allIter, err := eventStore.ReadAll(ctx, eventstore.ReadStreamOptions{})
	check(err)

	// create a projection using the "all events" iterator
	allProj, err := projection.New(allIter)
	check(err)

	// run the projection, simply incrementing a counter then printing the total
	count := 0
	_, err = allProj.Project(ctx, projection.EventHandlerFunc(func(_ context.Context, evt *eventstore.Event) error {
		count++
		return nil
	}))
	fmt.Println("total events in event store:", count)
	check(err)
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
