package main

import (
	"context"
	"fmt"
	"time"

	"github.com/go-estoria/estoria"
)

// This file exists only for the hosted demo. Running the example locally, none
// of it is active: the reset scheduler is off unless -hourly-reset is passed.

// A resetMessage tells connected browsers the lobby has been cleared. They
// reload, because whatever game they were watching no longer exists.
type resetMessage struct {
	Reset bool `json:"reset"`
}

// runHourlyReset clears the lobby at the top of every hour until ctx is done.
// The wall clock is deliberate — "games are cleared on the hour" is something
// a visitor can predict, unlike an interval counted from boot.
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
			estoria.GetLogger().Error("resetting demo lobby", "error", err)
			continue
		}

		estoria.GetLogger().Info("reset demo lobby")
	}
}

// nextHour returns the next hour boundary after t. Truncate works on absolute
// time, so the boundary is a UTC one — identical to the local top of the hour
// wherever the offset is a whole number of hours, which includes the UTC
// containers this runs in.
func nextHour(t time.Time) time.Time {
	return t.Truncate(time.Hour).Add(time.Hour)
}

// resetDemo deletes every game.
//
// Note what this does *not* do: it doesn't ask estoria to delete anything. An
// event store is append-only — that's the premise, and the core interface
// offers reads and appends and nothing else — so the reset reaches past
// estoria and truncates the storage tables directly, which is honest about
// what it is: a demo affordance, not an event sourcing operation.
//
// Unlike the kanban example there is nothing to reseed: an empty lobby with a
// "new game" button is this app's natural starting state.
func (s *server) resetDemo(ctx context.Context) error {
	// Block command handling for the duration: without this, a move that
	// loaded its game a moment ago could save into a stream that no longer
	// has the position it validated against.
	s.resetMu.Lock()
	defer s.resetMu.Unlock()

	for _, table := range []string{eventsTable, streamsTable} {
		if _, err := s.db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return fmt.Errorf("clearing table %s: %w", table, err)
		}
	}

	s.hub.broadcast(resetMessage{Reset: true})

	return nil
}
