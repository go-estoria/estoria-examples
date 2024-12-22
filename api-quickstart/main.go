package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/go-estoria/estoria"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/go-estoria/estoria/eventstore/memory"
	"github.com/go-estoria/estoria/outbox"
	"github.com/go-estoria/estoria/snapshotstore"
	"github.com/gofrs/uuid/v5"
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

	obox := memory.NewOutbox()

	logger := &OutboxLogger{}
	obox.RegisterHandlers(AccountCreatedEvent{}.EventType(), logger)
	obox.RegisterHandlers(AccountDeletedEvent{}.EventType(), logger)
	obox.RegisterHandlers(UserAddedEvent{}.EventType(), logger)
	obox.RegisterHandlers(UserRemovedEvent{}.EventType(), logger)
	obox.RegisterHandlers(BalanceChangedEvent{}.EventType(), logger)

	outboxProcessor := outbox.NewProcessor(obox)
	outboxProcessor.RegisterHandlers(logger)

	if err := outboxProcessor.Start(ctx); err != nil {
		panic(err)
	}

	eventStore, err := memory.NewEventStore(
		memory.WithOutbox(obox),
	)
	if err != nil {
		panic(err)
	}

	var aggregateStore aggregatestore.Store[Account]

	aggregateStore, err = aggregatestore.NewEventSourcedStore(eventStore, NewAccount, aggregatestore.WithEventTypes(
		AccountCreatedEvent{},
		AccountDeletedEvent{},
		UserAddedEvent{},
		UserRemovedEvent{},
		BalanceChangedEvent{},
	))
	if err != nil {
		panic(err)
	}

	snapshotStore := snapshotstore.NewMemoryStore()
	snapshotPolicy := snapshotstore.EventCountSnapshotPolicy{N: 8}
	aggregateStore, err = aggregatestore.NewSnapshottingStore(aggregateStore, NewAccount, snapshotStore, snapshotPolicy)
	if err != nil {
		panic(err)
	}

	hookableStore, err := aggregatestore.NewHookableStore(aggregateStore)
	if err != nil {
		panic(err)
	}
	hookableStore.BeforeSave(func(ctx context.Context, aggregate *aggregatestore.Aggregate[Account]) error {
		slog.Info("before-save aggregate store hook", "aggregate_id", aggregate.ID())
		return nil
	})
	hookableStore.AfterSave(func(ctx context.Context, aggregate *aggregatestore.Aggregate[Account]) error {
		slog.Info("after-save aggregate store hook", "aggregate_id", aggregate.ID())
		return nil
	})

	aggregateStore = hookableStore

	aggregate := aggregatestore.NewAggregate(NewAccount(uuid.Must(uuid.NewV4())), 0)

	fmt.Println("created new account:", aggregate.Entity())

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

	if err := aggregateStore.Save(ctx, aggregate, aggregatestore.SaveOptions{}); err != nil {
		panic(err)
	}

	fmt.Println("saved account:", aggregate.Entity())

	loadedAggregate, err := aggregateStore.Load(ctx, aggregate.ID(), aggregatestore.LoadOptions{})
	if err != nil {
		panic(err)
	}

	fmt.Println("loaded account:", loadedAggregate.Entity())
}

type OutboxLogger struct{}

func (l OutboxLogger) Name() string {
	return "logger"
}

func (l OutboxLogger) Handle(_ context.Context, item outbox.Item) error {
	slog.Info("handling outbox item", "event_id", item.EventID(), "handlers", len(item.Handlers()))
	return nil
}
