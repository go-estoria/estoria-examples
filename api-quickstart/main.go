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
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if os.Getenv("DEBUG") != "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})))
	}

	obox := memory.NewOutbox()

	logger := &OutboxLogger{}
	obox.RegisterHandlers(AccountCreatedEvent{}, logger)
	obox.RegisterHandlers(AccountDeletedEvent{}, logger)
	obox.RegisterHandlers(UserAddedEvent{}, logger)
	obox.RegisterHandlers(UserRemovedEvent{}, logger)
	obox.RegisterHandlers(BalanceChangedEvent{}, logger)

	outboxProcessor := outbox.NewProcessor(obox)
	outboxProcessor.RegisterHandlers(logger)

	if err := outboxProcessor.Start(ctx); err != nil {
		panic(err)
	}

	eventStore := memory.NewEventStore(
		memory.WithOutbox(obox),
	)

	var aggregateStore aggregatestore.Store[*Account]
	var err error

	aggregateStore, err = aggregatestore.NewEventSourcedStore(eventStore, NewAccount)
	if err != nil {
		panic(err)
	}

	snapshotStore := snapshotstore.NewMemoryStore()
	snapshotPolicy := snapshotstore.EventCountSnapshotPolicy{N: 8}
	aggregateStore = aggregatestore.NewSnapshottingStore(aggregateStore, snapshotStore, snapshotPolicy)

	hookableStore := aggregatestore.NewHookableStore(aggregateStore)
	hookableStore.AddHook(aggregatestore.BeforeSave, func(ctx context.Context, aggregate *estoria.Aggregate[*Account]) error {
		slog.Info("before-save aggregate store hook", "aggregate_id", aggregate.ID())
		return nil
	})
	hookableStore.AddHook(aggregatestore.AfterSave, func(ctx context.Context, aggregate *estoria.Aggregate[*Account]) error {
		slog.Info("after-save aggregate store hook", "aggregate_id", aggregate.ID())
		return nil
	})

	aggregateStore = hookableStore

	aggregate, err := aggregateStore.New(nil)
	if err != nil {
		panic(err)
	}

	fmt.Println("created new account:", aggregate.Entity())

	if err := aggregate.Append(
		&AccountCreatedEvent{Username: "Leonardo"},
		&BalanceChangedEvent{Amount: +1000},
		&UserAddedEvent{Username: "Michalangelo"},
		&BalanceChangedEvent{Amount: -500},
		&BalanceChangedEvent{Amount: +250},
		&UserAddedEvent{Username: "Raphael"},
		&UserRemovedEvent{Username: "Michalangelo"},
		&BalanceChangedEvent{Amount: -708},
	); err != nil {
		panic(err)
	}

	if err := aggregateStore.Save(ctx, aggregate, aggregatestore.SaveOptions{}); err != nil {
		panic(err)
	}

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

func (l OutboxLogger) Handle(_ context.Context, item outbox.OutboxItem) error {
	slog.Info("handling outbox item", "event_id", item.EventID(), "handlers", len(item.Handlers()))
	return nil
}
