package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/go-estoria/estoria"
	pgeventstore "github.com/go-estoria/estoria-contrib/postgres/eventstore"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/go-estoria/estoria/eventstore"
	"github.com/go-estoria/estoria/eventstore/projection"
	"github.com/gofrs/uuid/v5"
	_ "github.com/lib/pq"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if os.Getenv("DEBUG") != "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})))
	}

	estoria.SetLogger(estoria.DefaultLogger())

	// establish a database connection
	db, err := sql.Open("postgres", "postgres://estoria:estoria@localhost:5432/estoria?sslmode=disable")
	if err != nil {
		panic(err)
	}

	slog.Info("pinging Postgres")
	if err := db.Ping(); err != nil {
		panic(err)
	}

	slog.Info("connected to Postgres")

	// create the events table
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS events (
			id BIGSERIAL PRIMARY KEY,
			stream_id UUID NOT NULL,
			stream_type VARCHAR(255) NOT NULL,
			event_id UUID UNIQUE NOT NULL,
			event_type VARCHAR(255) NOT NULL,
			stream_offset BIGINT NOT NULL,
			global_offset BIGINT NOT NULL,
			timestamp TIMESTAMPTZ NOT NULL,
			data BYTEA NOT NULL
		);
	`); err != nil {
		panic(err)
	}

	eventStore, err := pgeventstore.New(db)
	if err != nil {
		panic(err)
	}

	var aggregateStore aggregatestore.Store[Account]

	// create an event-sourced store to load and save aggregates using the event store
	aggregateStore, err = aggregatestore.New(eventStore, NewAccount, aggregatestore.WithEventTypes(
		AccountCreatedEvent{},
		AccountDeletedEvent{},
		UserAddedEvent{},
		UserRemovedEvent{},
		BalanceChangedEvent{},
	))
	if err != nil {
		panic(err)
	}

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
	if err != nil {
		panic(err)
	}

	fmt.Println("loaded account:", loadedAggregate.Entity())

	//
	// the below demonstrates some lower-level event store operations
	//

	// list all streams in the event store
	fmt.Println("all streams:")
	streams, err := eventStore.ListStreams(ctx)
	if err != nil {
		panic(err)
	}

	for _, stream := range streams {
		fmt.Println(stream)
	}

	// list all events in the event store
	fmt.Println("all events:")
	iter, err := eventStore.ReadAll(ctx, eventstore.ReadStreamOptions{})
	if err != nil {
		panic(err)
	}

	// create a projection using the event iterator
	proj, err := projection.New(iter)
	if err != nil {
		panic(err)
	}

	// run the projection, printing a line for each event
	if _, err := proj.Project(ctx, projection.EventHandlerFunc(func(ctx context.Context, evt *eventstore.Event) error {
		fmt.Printf("%s @%d %s %s\n", evt.StreamID, evt.StreamVersion, evt.Timestamp.Format(time.DateTime), evt.ID.TypeName())
		return nil
	})); err != nil {
		panic(err)
	}
}
