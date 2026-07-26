package main

import (
	"context"
	"fmt"
	"time"

	"github.com/go-estoria/estoria"
)

// This file exists only for the hosted demo. Running the example locally, none
// of it is active: the reset scheduler is off unless -hourly-reset is passed.

// runHourlyReset resets the demo board at the top of every hour until ctx is
// done. The wall clock is deliberate — "the board resets on the hour" is
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
			estoria.GetLogger().Error("resetting demo board", "error", err)
			continue
		}

		estoria.GetLogger().Info("reset demo board")
	}
}

// nextHour returns the next hour boundary after t. Truncate works on absolute
// time, so the boundary is a UTC one — identical to the local top of the hour
// wherever the offset is a whole number of hours, which includes the UTC
// containers this runs in.
func nextHour(t time.Time) time.Time {
	return t.Truncate(time.Hour).Add(time.Hour)
}

// resetDemo clears the event store and reseeds the starting board.
//
// Note what this does *not* do: it doesn't ask estoria to delete anything. An
// event store is append-only — that's the whole premise, and the core
// interface offers reads and appends and nothing else. So the reset reaches
// past estoria and truncates the storage tables directly, which is honest
// about what it is: a demo affordance, not an event sourcing operation.
//
// The reseed saves through the hookable store, so the AfterSave hook
// broadcasts the fresh board to every connected browser — open tabs return to
// the starting state on their own, with no client-side handling.
func (s *server) resetDemo(ctx context.Context) error {
	// Block command handling for the duration: without this, a command that
	// loaded the board a moment ago could save its event into the freshly
	// seeded stream.
	s.resetMu.Lock()
	defer s.resetMu.Unlock()

	for _, table := range []string{eventsTable, streamsTable} {
		if _, err := s.db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return fmt.Errorf("clearing table %s: %w", table, err)
		}
	}

	// This save fires the AfterSave hook, which broadcasts while resetMu is
	// held for writing. That is safe because the hub takes only its own lock
	// and no handler broadcasts while holding resetMu — keep it that way.
	if err := seedBoard(ctx, s.live, s.boardID); err != nil {
		return fmt.Errorf("reseeding board: %w", err)
	}

	return nil
}
