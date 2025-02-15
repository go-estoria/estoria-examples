package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"

	"github.com/go-estoria/estoria"
	postgreses "github.com/go-estoria/estoria-contrib/postgres/eventstore"
	"github.com/go-estoria/estoria-contrib/postgres/eventstore/strategy"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/go-estoria/estoria/eventstore"
	"github.com/go-estoria/estoria/eventstore/memory"

	// memoryes "github.com/go-estoria/estoria/eventstore/memory"
	"github.com/go-estoria/estoria/outbox"
	"github.com/go-estoria/estoria/snapshotstore"
	memoryss "github.com/go-estoria/estoria/snapshotstore/memory"
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

	var eventStore eventstore.Store

	// eventStore, err := memoryes.NewEventStore()
	// mongoClient, err := mongo.Connect(options.Client().ApplyURI("mongodb://localhost:27017").SetReplicaSet("rs0"))
	// if err != nil {
	// 	panic(err)
	// }
	db, err := sql.Open("postgres", "postgres://estoria:estoria@localhost:5432/estoria?sslmode=disable")
	if err != nil {
		panic(err)
	}

	slog.Info("pinging Postgres")
	if err := db.Ping(); err != nil {
		panic(err)
	}

	slog.Info("connected to Postgres")

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

	// strat, err := strategy.NewMultiCollectionStrategy(
	// 	mongoClient,
	// 	mongoClient.Database("estoria"),
	// 	strategy.CollectionPerStreamType(),
	// )
	strat, err := strategy.NewSingleTableStrategy(db, "events")
	if err != nil {
		panic(err)
	}

	postgresEventStore, err := postgreses.New(db,
		postgreses.WithStrategy(strat),
	)
	if err != nil {
		panic(err)
	}

	eventStore = postgresEventStore

	var aggregateStore aggregatestore.Store[Account]

	// create an event-sourced store to load and save aggregates using the event store
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

	// create a snapshot store to save and load snapshots before hitting the event store
	snapshotStore := memoryss.NewSnapshotStore()

	snapshotPolicy := snapshotstore.EventCountSnapshotPolicy{N: 3}
	aggregateStore, err = aggregatestore.NewSnapshottingStore(aggregateStore, snapshotStore, snapshotPolicy)
	if err != nil {
		panic(err)
	}

	// add hooks around the snapshotting store
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

	// fmt.Println("all streams:")
	// streams, err := postgresEventStore.ListStreams(ctx)
	// if err != nil {
	// 	panic(err)
	// }

	// for _, stream := range streams {
	// 	fmt.Println(stream)
	// }

	// fmt.Println("all events:")
	// iter, err := postgresEventStore.ReadAll(ctx, eventstore.ReadStreamOptions{})
	// if err != nil {
	// 	panic(err)
	// }

	// proj, err := projection.New(iter)
	// if err != nil {
	// 	panic(err)
	// }

	// if _, err := proj.Project(ctx, projection.EventHandlerFunc(func(ctx context.Context, evt *eventstore.Event) error {
	// 	fmt.Printf("%s @%d %s %s\n", evt.StreamID, evt.StreamVersion, evt.Timestamp.Format(time.DateTime), evt.ID.TypeName())
	// 	return nil
	// })); err != nil {
	// 	panic(err)
	// }
}

type OutboxLogger struct{}

func (l OutboxLogger) Name() string {
	return "logger"
}

func (l OutboxLogger) Handle(_ context.Context, item outbox.Item) error {
	slog.Info("handling outbox item", "event_id", item.EventID(), "handlers", len(item.Handlers()))
	return nil
}
