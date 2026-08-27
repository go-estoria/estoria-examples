package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/go-estoria/estoria"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/go-estoria/estoria/eventstore"
	"github.com/go-estoria/estoria/projection"
	"github.com/go-estoria/estoria/projection/checkpointstore"
	"github.com/go-estoria/estoria/projection/lifecycle"
	"github.com/go-estoria/estoria/projection/processor"
)

// A servingManager keeps a steady-state processor tailing the live
// projection version whenever the lifecycle does not own it. Steady-state
// processing is deliberately not a lifecycle concern, so the application
// decides when to run one — and the rule has one subtlety: during a rebuild
// the lifecycle run's own processor tails the target version, and from
// promotion until that run winds down, the target IS the live version.
// Starting a steady processor beside it would put two writers on one table,
// and retiring the previous version requires its processor stopped first.
// So the manager reconciles on an interval toward: run a processor for the
// live version, unless an in-flight attempt targets it.
type servingManager struct {
	orchestrator *lifecycle.Orchestrator
	router       lifecycle.Router
	events       eventstore.GlobalReader
	checkpoints  checkpointstore.Store
	handler      func(projection.ID) (projection.EventHandler, error)
	log          estoria.Logger

	mu      sync.Mutex
	current projection.ID // zero when no steady processor runs
	stop    context.CancelFunc
	done    chan struct{}
}

// run reconciles until ctx ends, then stops any processor it owns.
func (m *servingManager) run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.reconcile(ctx, projection.ID{})
			return
		case <-ticker.C:
		}

		m.reconcile(ctx, m.desired(ctx))
	}
}

// serving reports the version the steady processor currently tails, and
// false when none runs.
func (m *servingManager) serving() (projection.ID, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.current, m.current != projection.ID{}
}

// desired gathers the router's live version and the lifecycle state and
// applies the serving rule. A projection that has never had a rebuild has
// nothing to serve and nothing in flight.
func (m *servingManager) desired(ctx context.Context) projection.ID {
	live, err := m.router.Live(ctx, projectionName)
	if errors.Is(err, lifecycle.ErrNoLiveVersion) {
		return projection.ID{}
	} else if err != nil {
		m.log.Error("reading the live version", "error", err)
		return projection.ID{}
	}

	state, err := m.orchestrator.Get(ctx, projectionName)
	if errors.Is(err, aggregatestore.ErrAggregateNotFound) {
		state = lifecycle.State{}
	} else if err != nil {
		m.log.Error("reading lifecycle state", "error", err)
		return projection.ID{}
	}

	return desiredVersion(live, state.Attempt)
}

// desiredVersion is the serving rule: a steady processor tails the live
// version, unless the in-flight attempt targets it — from promotion until
// the rebuild run winds down, the run's own processor tails the target, and
// a second processor beside it would double-write one table.
func desiredVersion(live projection.ID, attempt lifecycle.AttemptState) projection.ID {
	if attempt.Phase != lifecycle.PhaseNone && attempt.Target == live {
		return projection.ID{}
	}

	return live
}

// reconcile moves the running processor toward the desired version: stopping
// one that no longer matches, starting one that is missing. A processor that
// fails is logged and cleared; the next tick starts a fresh one.
func (m *servingManager) reconcile(ctx context.Context, desired projection.ID) {
	m.mu.Lock()
	current, stop, done := m.current, m.stop, m.done
	m.mu.Unlock()

	if current == desired {
		return
	}

	if stop != nil {
		stop()
		<-done

		m.mu.Lock()
		m.current, m.stop, m.done = projection.ID{}, nil, nil
		m.mu.Unlock()
	}

	if desired == (projection.ID{}) || ctx.Err() != nil {
		return
	}

	handler, err := m.handler(desired)
	if err != nil {
		m.log.Error("creating steady-state handler", "projection", desired, "error", err)
		return
	}

	proc, err := processor.New(m.events, m.checkpoints, desired, handler,
		processor.WithPollInterval(500*time.Millisecond))
	if err != nil {
		m.log.Error("creating steady-state processor", "projection", desired, "error", err)
		return
	}

	procCtx, cancel := context.WithCancel(ctx)
	finished := make(chan struct{})

	m.mu.Lock()
	m.current, m.stop, m.done = desired, cancel, finished
	m.mu.Unlock()

	go func() {
		defer close(finished)

		if err := proc.Run(procCtx); err != nil && !errors.Is(err, context.Canceled) {
			m.log.Error("steady-state processor exited", "projection", desired, "error", err)
		}

		m.mu.Lock()
		if m.current == desired {
			m.current, m.stop, m.done = projection.ID{}, nil, nil
		}
		m.mu.Unlock()

		cancel()
	}()
}
