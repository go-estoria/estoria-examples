package main

import (
	"context"
	"fmt"
	"time"

	"github.com/go-estoria/estoria"
)

// This file exists only for the hosted demo. Running the example locally, none
// of it is active: the simulator runs continuously and nothing resets.

// runIdleSupervisor starts the simulator while someone is watching and stops
// it once nobody has been for idleAfter.
//
// A fleet of simulated devices writing forever is the right behavior for a
// local run and the wrong one for a hosted demo: it burns CPU around the clock
// and grows the streams without bound, with nobody looking. Gating it on live
// connections keeps the demo honest — a visitor still sees readings arrive
// within a tick of opening the page — while an unwatched instance costs
// nothing and its streams stay short enough that the hydration benchmark still
// tells a clear story.
func (s *server) runIdleSupervisor(ctx context.Context, idleAfter time.Duration) {
	const tick = 5 * time.Second

	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	var idleFor time.Duration

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if s.hub.count() > 0 {
			idleFor = 0
			if s.sim.start() {
				estoria.GetLogger().Info("simulator resumed: a client is watching")
			}
			continue
		}

		// Nobody is connected. Wait out the grace period before stopping, so a
		// page reload doesn't halt the fleet between the disconnect and the
		// reconnect.
		if !s.sim.isRunning() {
			continue
		}
		if idleFor += tick; idleFor >= idleAfter {
			if s.sim.stop() {
				estoria.GetLogger().Info("simulator paused: nobody is watching", "idle_for", idleFor)
			}
			idleFor = 0
		}
	}
}

// runHourlyReset clears the fleet's history at the top of every hour until ctx
// is done. The wall clock is deliberate — "the fleet resets on the hour" is
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
			estoria.GetLogger().Error("resetting demo fleet", "error", err)
			continue
		}

		estoria.GetLogger().Info("reset demo fleet")
	}
}

// nextHour returns the next hour boundary after t. Truncate works on absolute
// time, so the boundary is a UTC one — identical to the local top of the hour
// wherever the offset is a whole number of hours, which includes the UTC
// containers this runs in.
func nextHour(t time.Time) time.Time {
	return t.Truncate(time.Hour).Add(time.Hour)
}

// resetDemo clears every device's history and re-registers a fresh fleet.
//
// Unlike the other examples' resets, this one has a reason beyond tidiness: the
// simulator appends continuously, so without a periodic truncation the streams
// grow without bound for as long as the demo is watched. Snapshots keep loads
// fast, but the database file would not stop growing.
//
// The simulator is stopped for the duration and restarted afterwards only if
// someone is still watching — otherwise the idle supervisor will start it when
// somebody arrives.
func (s *server) resetDemo(ctx context.Context) error {
	wasRunning := s.sim.isRunning()
	s.sim.stop()

	if err := func() error {
		s.resetMu.Lock()
		defer s.resetMu.Unlock()

		for _, table := range []string{eventsTable, streamsTable} {
			if _, err := s.db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
				return fmt.Errorf("clearing table %s: %w", table, err)
			}
		}

		// The cache holds aggregates for devices that no longer exist.
		if err := s.cache.Reset(); err != nil {
			return fmt.Errorf("resetting cache: %w", err)
		}

		s.reg.reset()

		return registerDevices(ctx, s.live, s.reg, s.deviceCount)
	}(); err != nil {
		return err
	}

	if wasRunning && s.hub.count() > 0 {
		s.sim.start()
	}

	return nil
}
