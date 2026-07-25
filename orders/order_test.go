package main

import (
	"context"
	"errors"
	"testing"

	"github.com/go-estoria/estoria"
	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/go-estoria/estoria/eventstore"
	"github.com/go-estoria/estoria/eventstore/memory"
	"github.com/gofrs/uuid/v5"
)

// Event-sourced domains are easy to test: given an order, apply an event,
// assert on the resulting state. No storage, no mocks.

var testItems = []LineItem{
	{SKU: "TEE-001", Name: "Estoria Tee", Qty: 2, PriceCents: 2499},
	{SKU: "MUG-002", Name: "Event Sourcing Mug", Qty: 1, PriceCents: 1450},
}

// orderAt replays the happy path up to (and including) the given status and
// returns the resulting order.
func orderAt(t *testing.T, status Status) Order {
	t.Helper()

	steps := []struct {
		status Status
		event  estoria.EntityEvent[Order]
	}{
		{StatusPlaced, OrderPlaced{Customer: "Ada Lovelace", Items: testItems}},
		{StatusPaid, OrderPaid{Method: "visa"}},
		{StatusPicked, OrderPicked{}},
		{StatusShipped, OrderShipped{Carrier: "UPS", Tracking: "1Z000000001"}},
		{StatusDelivered, OrderDelivered{}},
	}

	order := NewOrder(uuid.Must(uuid.NewV4()))
	for _, step := range steps {
		var err error
		if order, err = step.event.ApplyTo(context.Background(), order); err != nil {
			t.Fatalf("applying %T: %v", step.event, err)
		}
		if step.status == status {
			return order
		}
	}

	t.Fatalf("status %q is not on the happy path", status)
	return order
}

func TestEventApplication(t *testing.T) {
	t.Parallel()

	t.Run("walks the full happy path", func(t *testing.T) {
		t.Parallel()
		order := orderAt(t, StatusDelivered)

		if order.Status != StatusDelivered {
			t.Errorf("status = %q, want %q", order.Status, StatusDelivered)
		}
		if order.Customer != "Ada Lovelace" {
			t.Errorf("customer = %q, want Ada Lovelace", order.Customer)
		}
		if want := int64(2*2499 + 1450); order.TotalCents != want {
			t.Errorf("total = %d, want %d", order.TotalCents, want)
		}
		if units := order.UnitCount(); units != 3 {
			t.Errorf("unit count = %d, want 3", units)
		}
	})

	t.Run("cancels before shipping", func(t *testing.T) {
		t.Parallel()
		for _, status := range []Status{StatusPlaced, StatusPaid, StatusPicked} {
			order, err := OrderCancelled{Reason: "changed my mind"}.ApplyTo(context.Background(), orderAt(t, status))
			if err != nil {
				t.Errorf("cancelling a %s order: %v", status, err)
				continue
			}
			if order.Status != StatusCancelled {
				t.Errorf("status after cancelling a %s order = %q, want %q", status, order.Status, StatusCancelled)
			}
		}
	})

	t.Run("rejects invalid transitions", func(t *testing.T) {
		t.Parallel()

		// every event that is NOT the single legal next step for each status
		invalid := map[Status][]estoria.EntityEvent[Order]{
			StatusPlaced:    {OrderPlaced{Customer: "x", Items: testItems}, OrderPicked{}, OrderShipped{}, OrderDelivered{}},
			StatusPaid:      {OrderPlaced{Customer: "x", Items: testItems}, OrderPaid{}, OrderShipped{}, OrderDelivered{}},
			StatusPicked:    {OrderPlaced{Customer: "x", Items: testItems}, OrderPaid{}, OrderPicked{}, OrderDelivered{}},
			StatusShipped:   {OrderPlaced{Customer: "x", Items: testItems}, OrderPaid{}, OrderPicked{}, OrderShipped{}, OrderCancelled{}},
			StatusDelivered: {OrderPlaced{Customer: "x", Items: testItems}, OrderPaid{}, OrderPicked{}, OrderShipped{}, OrderDelivered{}, OrderCancelled{}},
		}

		for status, events := range invalid {
			base := orderAt(t, status)
			for _, event := range events {
				if _, err := event.ApplyTo(context.Background(), base); err == nil {
					t.Errorf("%T applied to a %s order: expected an error", event, status)
				}
			}
		}
	})

	t.Run("rejects everything after cancellation", func(t *testing.T) {
		t.Parallel()

		cancelled, err := OrderCancelled{Reason: "test"}.ApplyTo(context.Background(), orderAt(t, StatusPlaced))
		if err != nil {
			t.Fatal(err)
		}

		for _, event := range orderEventPrototypes() {
			if _, err := event.ApplyTo(context.Background(), cancelled); err == nil {
				t.Errorf("%T applied to a cancelled order: expected an error", event)
			}
		}
	})

	t.Run("rejects commands on an unplaced order", func(t *testing.T) {
		t.Parallel()

		empty := NewOrder(uuid.Must(uuid.NewV4()))
		for _, event := range []estoria.EntityEvent[Order]{
			OrderPaid{}, OrderPicked{}, OrderShipped{}, OrderDelivered{}, OrderCancelled{},
		} {
			if _, err := event.ApplyTo(context.Background(), empty); err == nil {
				t.Errorf("%T applied to an unplaced order: expected an error", event)
			}
		}
	})

	t.Run("rejects an order with no items", func(t *testing.T) {
		t.Parallel()

		empty := NewOrder(uuid.Must(uuid.NewV4()))
		if _, err := (OrderPlaced{Customer: "Ada"}).ApplyTo(context.Background(), empty); err == nil {
			t.Error("expected an error placing an order with no items")
		}
	})

	t.Run("does not mutate the input order", func(t *testing.T) {
		t.Parallel()

		before := orderAt(t, StatusPlaced)
		itemsBefore := len(before.Items)

		if _, err := (OrderPaid{Method: "visa"}).ApplyTo(context.Background(), before); err != nil {
			t.Fatal(err)
		}

		if before.Status != StatusPlaced || len(before.Items) != itemsBefore {
			t.Errorf("input order was mutated: %+v", before)
		}
	})
}

// TestOrderRoundTrip runs the full aggregate lifecycle against estoria's
// in-memory event store: save, load, load at a past version, and conflict
// detection.
func TestOrderRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	eventStore, err := memory.NewEventStore()
	if err != nil {
		t.Fatal(err)
	}

	store, err := aggregatestore.New(eventStore, NewOrder,
		aggregatestore.WithEventTypes(orderEventPrototypes()...))
	if err != nil {
		t.Fatal(err)
	}

	orderID := uuid.Must(uuid.NewV7())

	agg := store.New(orderID)
	if err := agg.Append(
		OrderPlaced{Customer: "Grace Hopper", Items: testItems},
		OrderPaid{Method: "amex"},
	); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, agg, nil); err != nil {
		t.Fatal(err)
	}

	// load the latest state
	loaded, err := store.Load(ctx, orderID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := loaded.Version(); v != 2 {
		t.Fatalf("loaded version = %d, want 2", v)
	}
	if order := loaded.Entity(); order.Status != StatusPaid || order.Customer != "Grace Hopper" {
		t.Fatalf("loaded order = %+v, want Grace Hopper's order in status paid", order)
	}

	// load the order as it was before payment
	past, err := store.Load(ctx, orderID, &aggregatestore.LoadOptions{ToVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if order := past.Entity(); order.Status != StatusPlaced {
		t.Fatalf("order at v1 = %+v, want status placed", order)
	}

	// optimistic concurrency: two writers save from the same version
	first, err := store.Load(ctx, orderID, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Load(ctx, orderID, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := first.Append(OrderPicked{}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, first, nil); err != nil {
		t.Fatal(err)
	}

	if err := second.Append(OrderCancelled{Reason: "too slow"}); err != nil {
		t.Fatal(err)
	}
	err = store.Save(ctx, second, nil)
	if err == nil {
		t.Fatal("expected a version conflict saving from a stale version")
	}
	if !errors.Is(err, eventstore.StreamVersionMismatchError{}) {
		t.Fatalf("expected StreamVersionMismatchError, got: %v", err)
	}
}
