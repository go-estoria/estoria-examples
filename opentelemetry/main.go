package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/go-estoria/estoria"
	otelaggregatestore "github.com/go-estoria/estoria-contrib/opentelemetry/aggregatestore"
	oteleventstore "github.com/go-estoria/estoria-contrib/opentelemetry/eventstore"
	otelsnapshotstore "github.com/go-estoria/estoria-contrib/opentelemetry/snapshotstore"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/go-estoria/estoria/eventstore"
	memoryes "github.com/go-estoria/estoria/eventstore/memory"
	"github.com/go-estoria/estoria/eventstore/projection"
	"github.com/go-estoria/estoria/snapshotstore"
	memoryss "github.com/go-estoria/estoria/snapshotstore/memory"
	"github.com/gofrs/uuid/v5"
)

const appName = "estoria-api-quickstart"

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var err error

	if os.Getenv("DEBUG") != "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})))
	}

	estoria.SetLogger(estoria.DefaultLogger())

	shutdownTracer := initTracer(ctx, appName)
	defer func() {
		if err := shutdownTracer(ctx); err != nil {
			slog.Error("failed to shutdown tracer", "error", err)
		}
	}()

	shutdownMeter := initMeter(ctx, appName)
	defer func() {
		if err := shutdownMeter(ctx); err != nil {
			slog.Error("failed to shutdown meter", "error", err)
		}
	}()

	// create an event store to save and load events
	var eventStore eventstore.Store
	eventStore, err = memoryes.NewEventStore()
	if err != nil {
		panic(err)
	}

	// add instrumentation around the event store
	eventStore, err = oteleventstore.NewInstrumentedStore(eventStore)
	if err != nil {
		panic(err)
	}

	// create an event-sourced aggregate store to load and save aggregates using the event store
	var aggregateStore aggregatestore.Store[Account]
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

	// add instrumentation around the event-sourced aggregate store
	aggregateStore, err = otelaggregatestore.NewInstrumentedStore(aggregateStore)
	if err != nil {
		panic(err)
	}

	// create a snapshot store to save and load snapshots before hitting the event store
	var snapshotStore snapshotstore.SnapshotStore = memoryss.NewSnapshotStore()

	// add instrumentation around the snapshot store
	snapshotStore, err = otelsnapshotstore.NewInstrumentedStore(snapshotStore)
	if err != nil {
		panic(err)
	}

	// wrap the event-sourced aggreate store with a snapshotting aggregate store that uses the snapshot store
	snapshotPolicy := snapshotstore.EventCountSnapshotPolicy{N: 3}
	aggregateStore, err = aggregatestore.NewSnapshottingStore(aggregateStore, snapshotStore, snapshotPolicy)
	if err != nil {
		panic(err)
	}

	// add instrumentation around the snapshotting aggregate store
	aggregateStore, err = otelaggregatestore.NewInstrumentedStore(aggregateStore,
		// to differentiate between the snapshotting store and the event-sourced store,
		// we can set a different metric and trace namespace
		otelaggregatestore.WithMetricNamespace[Account]("snapshottingstore"),
		otelaggregatestore.WithTraceNamespace[Account]("snapshottingstore"),
	)
	if err != nil {
		panic(err)
	}

	// create an aggregate from a new entity, append some events, and save it
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
	if err := aggregateStore.Save(ctx, aggregate, nil); err != nil {
		panic(err)
	}

	fmt.Printf("saved account:\n  %s\n", aggregate.Entity())

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
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
