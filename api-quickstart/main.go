package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/go-estoria/estoria"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/go-estoria/estoria/snapshotstore"

	otelstore "github.com/go-estoria/estoria-contrib/opentelemetry/aggregatestore"
	// "github.com/go-estoria/estoria/eventstore/memory"
	s3es "github.com/go-estoria/estoria-contrib/aws/s3/eventstore"
	s3snapshotstore "github.com/go-estoria/estoria-contrib/aws/s3/snapshotstore"
	"github.com/go-estoria/estoria/outbox"

	// "github.com/go-estoria/estoria/snapshotstore"
	"github.com/gofrs/uuid/v5"
)

const appName = "estoria-api-quickstart"

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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

	// obox := memory.NewOutbox()

	// logger := &OutboxLogger{}
	// obox.RegisterHandlers(AccountCreatedEvent{}.EventType(), logger)
	// obox.RegisterHandlers(AccountDeletedEvent{}.EventType(), logger)
	// obox.RegisterHandlers(UserAddedEvent{}.EventType(), logger)
	// obox.RegisterHandlers(UserRemovedEvent{}.EventType(), logger)
	// obox.RegisterHandlers(BalanceChangedEvent{}.EventType(), logger)

	// outboxProcessor := outbox.NewProcessor(obox)
	// outboxProcessor.RegisterHandlers(logger)

	// if err := outboxProcessor.Start(ctx); err != nil {
	// 	panic(err)
	// }

	// configure AWS for local minio server
	awsConfig, err := config.LoadDefaultConfig(ctx,
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("minio", "minio123", "")),
		config.WithRegion("us-east-1"),
	)
	if err != nil {
		panic(err)
	}

	s3Client, err := s3es.NewDefaultS3Client(ctx, awsConfig)
	if err != nil {
		panic(err)
	}

	eventStore, err := s3es.New(s3Client)
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

	snapshotStore := s3snapshotstore.New(s3Client)
	snapshotPolicy := snapshotstore.EventCountSnapshotPolicy{N: 3}
	aggregateStore, err = aggregatestore.NewSnapshottingStore(aggregateStore, snapshotStore, snapshotPolicy)
	if err != nil {
		panic(err)
	}

	hookableStore, err := aggregatestore.NewHookableStore(aggregateStore)
	if err != nil {
		panic(err)
	}
	hookableStore.BeforeLoad(func(ctx context.Context, id uuid.UUID) error {
		slog.Info("before-load aggregate store hook", "aggregate_id", id)
		return nil
	})
	hookableStore.AfterLoad(func(ctx context.Context, aggregate *aggregatestore.Aggregate[Account]) error {
		slog.Info("after-load aggregate store hook", "aggregate_id", aggregate.ID())
		return nil
	})
	hookableStore.BeforeSave(func(ctx context.Context, aggregate *aggregatestore.Aggregate[Account]) error {
		slog.Info("before-save aggregate store hook", "aggregate_id", aggregate.ID())
		return nil
	})
	hookableStore.AfterSave(func(ctx context.Context, aggregate *aggregatestore.Aggregate[Account]) error {
		slog.Info("after-save aggregate store hook", "aggregate_id", aggregate.ID())
		return nil
	})

	aggregateStore = hookableStore

	instrumentedStore, err := otelstore.NewInstrumentedStore(aggregateStore)
	if err != nil {
		panic(err)
	}

	aggregateStore = instrumentedStore

	accountID := uuid.Must(uuid.NewV4())

	aggregate := aggregatestore.NewAggregate(NewAccount(accountID), 0)

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

	loadedAggregate, err := aggregateStore.Load(ctx, accountID, aggregatestore.LoadOptions{})
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
