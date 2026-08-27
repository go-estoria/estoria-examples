package main

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-estoria/estoria"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/gofrs/uuid/v5"
)

// trafficHolders are the demo account holders the generator draws from.
var trafficHolders = []string{
	"Amara Okafor", "Bo Lindqvist", "Chidi Eze", "Dana Whitfield",
	"Esa Virtanen", "Farah Haddad", "Gustavo Reyes", "Hana Sato",
}

// A trafficGenerator appends a steady trickle of deposits and withdrawals
// through the ordinary command path, so rebuilds always run against a
// moving ledger: catch-up has history to replay, tailing has fresh events
// to follow, and a promotion happens under live writes.
type trafficGenerator struct {
	accounts *aggregatestore.EventSourcedStore[Account]
	log      estoria.Logger

	enabled atomic.Bool
	writes  atomic.Int64

	mu   sync.Mutex
	open []uuid.UUID
}

// run emits one write roughly every half second while enabled, until ctx
// ends. Each write opens an account (until every holder has one) or applies
// a random deposit or withdrawal to an existing account.
func (g *trafficGenerator) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(350+rand.IntN(300)) * time.Millisecond):
		}

		if !g.enabled.Load() {
			continue
		}

		if err := g.write(ctx); err != nil && ctx.Err() == nil {
			g.log.Warn("traffic write failed", "error", err)
		}
	}
}

// running reports whether the generator is emitting, and the total writes
// it has committed.
func (g *trafficGenerator) running() (bool, int64) {
	return g.enabled.Load(), g.writes.Load()
}

func (g *trafficGenerator) setEnabled(enabled bool) {
	g.enabled.Store(enabled)
}

func (g *trafficGenerator) write(ctx context.Context) error {
	g.mu.Lock()
	opened := len(g.open)
	g.mu.Unlock()

	if opened < len(trafficHolders) {
		return g.openAccount(ctx, trafficHolders[opened])
	}

	g.mu.Lock()
	id := g.open[rand.IntN(len(g.open))]
	g.mu.Unlock()

	aggregate, err := g.accounts.Load(ctx, id, nil)
	if err != nil {
		return err
	}

	account := aggregate.State()
	amount := int64((1 + rand.IntN(200)) * 25) // 25 cents to $50

	event, err := account.Deposit(amount, time.Now())
	if rand.IntN(3) == 0 && account.Balance >= amount {
		event, err = account.Withdraw(amount, time.Now())
	}

	if err != nil {
		return err
	}

	aggregate.Append(event)

	if err := g.accounts.Save(ctx, aggregate, nil); err != nil {
		return err
	}

	g.writes.Add(1)

	return nil
}

func (g *trafficGenerator) openAccount(ctx context.Context, holder string) error {
	id := uuid.Must(uuid.NewV4())
	aggregate := g.accounts.New(id)
	account := aggregate.State()

	open, err := account.Open(holder, time.Now())
	if err != nil {
		return err
	}

	// Appended events apply at save, so the opening deposit validates
	// against the state the open event will produce.
	deposit, err := open.ApplyTo(account).Deposit(int64((20+rand.IntN(80))*100), time.Now())
	if err != nil {
		return err
	}

	aggregate.Append(open, deposit)

	if err := g.accounts.Save(ctx, aggregate, nil); err != nil {
		return err
	}

	g.mu.Lock()
	g.open = append(g.open, id)
	g.mu.Unlock()

	g.writes.Add(1)

	return nil
}
