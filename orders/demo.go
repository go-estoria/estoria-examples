package main

import (
	"context"
	"fmt"
	"time"

	"github.com/go-estoria/estoria"
)

// This file exists only for the hosted demo. Running the example locally, none
// of it is active: the reset scheduler is off unless -hourly-reset is passed.

// runHourlyReset clears every order at the top of every hour until ctx is
// done. The wall clock is deliberate — "orders are cleared on the hour" is
// something a visitor can predict, unlike an interval counted from boot.
func (s *server) runHourlyReset(ctx context.Context) {
	s.runResets(ctx, nextHour)
}

// runResets resets the demo at each instant produced by next, until ctx is
// done. Splitting it from runHourlyReset is what lets a test drive the loop
// without waiting for a real hour to elapse.
func (s *server) runResets(ctx context.Context, next func(time.Time) time.Time) {
	for {
		timer := time.NewTimer(time.Until(next(time.Now())))

		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		if err := s.resetDemo(ctx); err != nil {
			estoria.GetLogger().Error("resetting demo orders", "error", err)
			continue
		}

		estoria.GetLogger().Info("reset demo orders")
	}
}

// nextHour returns the next hour boundary after t. Truncate works on absolute
// time, so the boundary is a UTC one — identical to the local top of the hour
// wherever the offset is a whole number of hours, which includes the UTC
// containers this runs in.
func nextHour(t time.Time) time.Time {
	return t.Truncate(time.Hour).Add(time.Hour)
}

// resetDemo deletes every order: the event streams, the undelivered outbox
// rows, and the read model built from them.
//
// It drops and recreates rather than truncating, which matters for a hosted
// demo beyond tidiness. A deploy carrying a library upgrade may expect columns
// the existing tables don't have — `CREATE TABLE IF NOT EXISTS` is a no-op on
// a table that already exists, so the app would start, serve reads, pass its
// health check, and fail every write. Rebuilding the schema makes that drift
// self-heal instead of becoming a silent half-outage.
//
// Note what this does *not* do: it doesn't ask estoria to delete anything. The
// core interface offers reads and appends, and a StreamDeleter for backends
// that can remove committed events — none of which is the right tool for
// "throw the whole store away and start over". So the reset reaches past
// estoria to the storage directly, which is honest about what it is: a demo
// affordance, not an event sourcing operation.
//
// The read model is the one table here that is legitimately disposable: it's
// derived data, rebuildable from the streams by definition. That it goes with
// them is a CQRS property, not a compromise.
func (s *server) resetDemo(ctx context.Context) error {
	// Block command handling for the duration: without this, a command that
	// loaded its order a moment ago could save into a stream that no longer
	// exists.
	s.resetMu.Lock()
	defer s.resetMu.Unlock()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning reset transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, table := range []string{eventsTable, streamsTable, outboxTable, readModelTable} {
		if _, err := tx.Exec(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
			return fmt.Errorf("dropping table %s: %w", table, err)
		}
	}

	if err := s.createSchema(ctx, tx); err != nil {
		return fmt.Errorf("recreating schema: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing reset: %w", err)
	}

	s.log.reset()
	s.hub.broadcast(map[string]any{"type": "reset"})

	return nil
}
