package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	pgoutbox "github.com/go-estoria/estoria-contrib/postgres/outbox"
	"github.com/gofrs/uuid/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The tables this file owns or reads directly: outboxTable is the outbox's
// default table name, which the pending-count query in pendingOutbox reads for
// the monitor panel, and readModelTable is the read model created in schema().
// The demo reset truncates both (see demo.go).
const (
	outboxTable    = "outbox"
	readModelTable = "order_summaries"
)

// An orderSummary is one row of the order_summaries read model: the handful
// of denormalized fields the order list needs, kept current by the outbox
// handler. Listing orders never touches the event store.
type orderSummary struct {
	ID         uuid.UUID `json:"id"`
	Customer   string    `json:"customer"`
	TotalCents int64     `json:"totalCents"`
	ItemCount  int       `json:"itemCount"`
	Status     Status    `json:"status"`
	PlacedAt   time.Time `json:"placedAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// readModel is the query side of the app: a plain Postgres table projected
// from the event stream by the outbox processor. The outbox handler is its
// ONLY writer — the HTTP handlers only ever SELECT from it.
type readModel struct {
	pool *pgxpool.Pool
}

func newReadModel(pool *pgxpool.Pool) *readModel {
	return &readModel{pool: pool}
}

// schema returns the DDL for the read model table. Like the event store and
// outbox schemas, it is idempotent and applied at startup.
func (rm *readModel) schema() string {
	return `CREATE TABLE IF NOT EXISTS order_summaries (
    id          uuid        PRIMARY KEY,
    customer    text        NOT NULL,
    total_cents bigint      NOT NULL,
    item_count  integer     NOT NULL,
    status      text        NOT NULL,
    placed_at   timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL
);`
}

// apply projects a single outbox item into the read model. It is called by
// the outbox processor with strict per-stream FIFO ordering, so by the time
// any status event arrives, the stream's OrderPlaced row is guaranteed to
// exist. Both branches are idempotent upserts: the outbox delivers
// at-least-once, so a redelivered item must be harmless.
func (rm *readModel) apply(ctx context.Context, item *pgoutbox.Item) error {
	switch item.EventID.Type {
	case OrderPlaced{}.EventType():
		var e OrderPlaced
		if err := json.Unmarshal(item.Data, &e); err != nil {
			return fmt.Errorf("decoding %s: %w", item.EventID.Type, err)
		}

		var totalCents int64
		itemCount := 0
		for _, li := range e.Items {
			totalCents += int64(li.Qty) * li.PriceCents
			itemCount += li.Qty
		}

		_, err := rm.pool.Exec(ctx, `
			INSERT INTO order_summaries (id, customer, total_cents, item_count, status, placed_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $6)
			ON CONFLICT (id) DO UPDATE
			SET customer = $2, total_cents = $3, item_count = $4, status = $5, placed_at = $6, updated_at = $6`,
			item.StreamID.UUID, e.Customer, totalCents, itemCount, StatusPlaced, item.Timestamp)
		return err

	case OrderPaid{}.EventType(), OrderPicked{}.EventType(), OrderShipped{}.EventType(),
		OrderDelivered{}.EventType(), OrderCancelled{}.EventType():
		_, err := rm.pool.Exec(ctx, `
			UPDATE order_summaries SET status = $2, updated_at = $3 WHERE id = $1`,
			item.StreamID.UUID, statusAfter(item.EventID.Type), item.Timestamp)
		return err

	default:
		// Unknown event types are skipped rather than failed: a failed item
		// blocks its entire stream until an operator intervenes.
		return nil
	}
}

// statusAfter maps a status-changing event type to the status it produces.
func statusAfter(eventType string) Status {
	switch eventType {
	case OrderPaid{}.EventType():
		return StatusPaid
	case OrderPicked{}.EventType():
		return StatusPicked
	case OrderShipped{}.EventType():
		return StatusShipped
	case OrderDelivered{}.EventType():
		return StatusDelivered
	case OrderCancelled{}.EventType():
		return StatusCancelled
	default:
		return ""
	}
}

// list returns the most recent orders, straight from the read model. This is
// the CQRS payoff: listing 100 orders is one SELECT, not 100 aggregate
// hydrations.
func (rm *readModel) list(ctx context.Context) ([]orderSummary, error) {
	rows, err := rm.pool.Query(ctx, `
		SELECT id, customer, total_cents, item_count, status, placed_at, updated_at
		FROM order_summaries
		ORDER BY placed_at DESC
		LIMIT 100`)
	if err != nil {
		return nil, fmt.Errorf("querying order summaries: %w", err)
	}
	defer rows.Close()

	summaries := []orderSummary{}
	for rows.Next() {
		var s orderSummary
		if err := rows.Scan(&s.ID, &s.Customer, &s.TotalCents, &s.ItemCount, &s.Status, &s.PlacedAt, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scanning order summary: %w", err)
		}
		summaries = append(summaries, s)
	}

	return summaries, rows.Err()
}

// statusCounts returns the number of orders in each status, for the header
// badges. Also served from the read model, never from aggregates.
func (rm *readModel) statusCounts(ctx context.Context) (map[Status]int, error) {
	rows, err := rm.pool.Query(ctx, `SELECT status, count(*) FROM order_summaries GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("querying status counts: %w", err)
	}
	defer rows.Close()

	counts := map[Status]int{}
	for rows.Next() {
		var status Status
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("scanning status count: %w", err)
		}
		counts[status] = count
	}

	return counts, rows.Err()
}

// pendingOutbox counts outbox rows that have been committed alongside their
// events but not yet delivered by the processor — the "lag" the monitor
// panel visualizes.
func (rm *readModel) pendingOutbox(ctx context.Context) (int, error) {
	var pending int
	err := rm.pool.QueryRow(ctx,
		`SELECT count(*) FROM `+outboxTable+` WHERE processed_at IS NULL AND failed_at IS NULL`,
	).Scan(&pending)
	return pending, err
}

// A delivery is one entry in the in-memory "webhook log": proof that the
// outbox processor handled an event. In a real system this handler would call
// an external webhook or publish to a message broker; here it projects the
// read model and records the delivery for the monitor panel.
type delivery struct {
	EventType     string    `json:"eventType"`
	OrderID       string    `json:"orderId"`
	StreamVersion int64     `json:"streamVersion"`
	DeliveredAt   time.Time `json:"deliveredAt"`
}

// A deliveryLog is a fixed-capacity ring buffer of recent deliveries, newest
// first. Because the outbox delivers at-least-once, a redelivered item may
// appear twice — which is exactly the kind of truth a delivery log should
// tell.
type deliveryLog struct {
	mu      sync.Mutex
	entries []delivery
	next    int
	full    bool
}

func newDeliveryLog(capacity int) *deliveryLog {
	return &deliveryLog{entries: make([]delivery, capacity)}
}

// add records a delivery, evicting the oldest entry once the buffer is full.
func (l *deliveryLog) add(d delivery) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries[l.next] = d
	l.next = (l.next + 1) % len(l.entries)
	if l.next == 0 {
		l.full = true
	}
}

// reset empties the log. Used only by the hosted demo's hourly reset, which
// deletes the orders those deliveries refer to (see demo.go).
func (l *deliveryLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()

	clear(l.entries)
	l.next = 0
	l.full = false
}

// recent returns the logged deliveries, newest first.
func (l *deliveryLog) recent() []delivery {
	l.mu.Lock()
	defer l.mu.Unlock()

	count := l.next
	if l.full {
		count = len(l.entries)
	}

	out := make([]delivery, 0, count)
	for i := 1; i <= count; i++ {
		out = append(out, l.entries[(l.next-i+len(l.entries))%len(l.entries)])
	}
	return out
}
