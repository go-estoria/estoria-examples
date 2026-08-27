package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-estoria/estoria/eventstore"
	"github.com/go-estoria/estoria/projection"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// projectionName is the one named projection this app maintains. Every
// version of it is a separate Postgres table named by the version's ID:
// account_balances_v1, account_balances_v2, and so on.
const projectionName = "account_balances"

// A readModel builds and queries the versioned account_balances tables.
//
// Version 1 is the schema this app "shipped" with: holder and balance only.
// Every later version also derives activity statistics — deposit and
// withdrawal counts and the last activity time — from the same event
// history. That difference is the point: the enriched columns exist in the
// events all along, and a rebuild is how a running system backfills them
// without touching the version still serving reads.
type readModel struct {
	pool *pgxpool.Pool
}

// handler is the projection handler factory, for the lifecycle orchestrator
// and the steady-state processor alike: the versioned ID flows in so each
// handler targets its own version's table. The factory itself touches no
// storage — a retirement interrupted after its teardown re-resolves the
// handler on repair, so preparing storage here would recreate a table the
// retirement just dropped.
func (m *readModel) handler(id projection.ID) (projection.EventHandler, error) {
	return &balancesHandler{pool: m.pool, id: id}, nil
}

// enriched reports whether a projection version carries the activity
// columns: every version after the original schema does.
func enriched(id projection.ID) bool {
	return id.Version > 1
}

// A balancesHandler maintains one version's table. It creates the table on
// the first event it handles, and applies each event at most once per row:
// every row records the global position of the last event applied to it,
// and an event at or below that position is a redelivery, skipped. That
// per-row guard is what makes at-least-once delivery safe for counters.
type balancesHandler struct {
	pool   *pgxpool.Pool
	id     projection.ID
	schema sync.Once
	err    error
}

func (h *balancesHandler) Handle(ctx context.Context, event *eventstore.Event) error {
	// Domain and lifecycle streams share the store's global sequence; this
	// projection folds account streams only.
	if event.StreamID.Type != "account" {
		return nil
	}

	h.schema.Do(func() { h.err = h.createTable(ctx) })
	if h.err != nil {
		return h.err
	}

	if event.GlobalPosition == nil {
		return fmt.Errorf("event %s has no global position", event.ID)
	}

	position := *event.GlobalPosition
	account := event.StreamID.UUID.String()
	table := h.id.String()

	switch event.ID.Type {
	case AccountOpened{}.EventType():
		var opened AccountOpened
		if err := json.Unmarshal(event.Data, &opened); err != nil {
			return fmt.Errorf("decoding %s: %w", event.ID.Type, err)
		}

		_, err := h.pool.Exec(ctx, fmt.Sprintf(
			`INSERT INTO %s (account_id, holder, balance, last_position) VALUES ($1, $2, 0, $3)
			 ON CONFLICT (account_id) DO NOTHING`, table),
			account, opened.Holder, position)

		return err
	case FundsDeposited{}.EventType():
		var deposited FundsDeposited
		if err := json.Unmarshal(event.Data, &deposited); err != nil {
			return fmt.Errorf("decoding %s: %w", event.ID.Type, err)
		}

		return h.apply(ctx, account, position, deposited.Amount, "deposits", deposited.At)
	case FundsWithdrawn{}.EventType():
		var withdrawn FundsWithdrawn
		if err := json.Unmarshal(event.Data, &withdrawn); err != nil {
			return fmt.Errorf("decoding %s: %w", event.ID.Type, err)
		}

		return h.apply(ctx, account, position, -withdrawn.Amount, "withdrawals", withdrawn.At)
	default:
		return nil
	}
}

// apply adjusts one account's balance, and on enriched versions the
// activity columns, guarded by the row's last applied position.
func (h *balancesHandler) apply(ctx context.Context, account string, position, delta int64, counter string, at time.Time) error {
	table := h.id.String()

	if !enriched(h.id) {
		_, err := h.pool.Exec(ctx, fmt.Sprintf(
			`UPDATE %s SET balance = balance + $1, last_position = $2
			 WHERE account_id = $3 AND last_position < $2`, table),
			delta, position, account)

		return err
	}

	_, err := h.pool.Exec(ctx, fmt.Sprintf(
		`UPDATE %s SET balance = balance + $1, %s = %s + 1, last_activity = $2, last_position = $3
		 WHERE account_id = $4 AND last_position < $3`, table, counter, counter),
		delta, at, position, account)

	return err
}

func (h *balancesHandler) createTable(ctx context.Context) error {
	columns := `
		account_id    text PRIMARY KEY,
		holder        text NOT NULL,
		balance       bigint NOT NULL,
		last_position bigint NOT NULL`

	if enriched(h.id) {
		columns += `,
		deposits      bigint NOT NULL DEFAULT 0,
		withdrawals   bigint NOT NULL DEFAULT 0,
		last_activity timestamptz`
	}

	_, err := h.pool.Exec(ctx, fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (%s)", h.id.String(), columns))
	if err != nil {
		return fmt.Errorf("creating table %s: %w", h.id.String(), err)
	}

	return nil
}

// Teardown implements projection.Teardowner: retiring a version drops its
// table. Idempotent and concurrent-safe, as retirement repair requires.
func (h *balancesHandler) Teardown(ctx context.Context, id projection.ID) error {
	if _, err := h.pool.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", id.String())); err != nil {
		return fmt.Errorf("dropping table %s: %w", id.String(), err)
	}

	return nil
}

// A balanceRow is one account as a projection version's table records it.
// The activity fields are nil on versions whose schema predates them.
type balanceRow struct {
	AccountID    string     `json:"accountId"`
	Holder       string     `json:"holder"`
	Balance      int64      `json:"balance"`
	Deposits     *int64     `json:"deposits,omitempty"`
	Withdrawals  *int64     `json:"withdrawals,omitempty"`
	LastActivity *time.Time `json:"lastActivity,omitempty"`
}

// table reports one version's rows, ordered by holder. A version whose
// table does not exist — never built, or torn down — reports exists=false
// rather than an error.
func (m *readModel) table(ctx context.Context, id projection.ID) (rows []balanceRow, exists bool, err error) {
	query := fmt.Sprintf("SELECT account_id, holder, balance FROM %s ORDER BY holder", id.String())
	if enriched(id) {
		query = fmt.Sprintf(
			"SELECT account_id, holder, balance, deposits, withdrawals, last_activity FROM %s ORDER BY holder",
			id.String())
	}

	result, err := m.pool.Query(ctx, query)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" { // undefined_table
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("querying %s: %w", id.String(), err)
	}
	defer result.Close()

	rows = []balanceRow{}

	for result.Next() {
		var row balanceRow

		if enriched(id) {
			err = result.Scan(&row.AccountID, &row.Holder, &row.Balance, &row.Deposits, &row.Withdrawals, &row.LastActivity)
		} else {
			err = result.Scan(&row.AccountID, &row.Holder, &row.Balance)
		}

		if err != nil {
			return nil, false, fmt.Errorf("scanning %s row: %w", id.String(), err)
		}

		rows = append(rows, row)
	}

	if err := result.Err(); err != nil {
		return nil, false, fmt.Errorf("reading %s rows: %w", id.String(), err)
	}

	return rows, true, nil
}
