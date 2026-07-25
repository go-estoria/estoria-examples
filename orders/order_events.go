package main

import (
	"context"
	"fmt"

	"github.com/go-estoria/estoria"
)

// Each event below implements estoria.EntityEvent[Order]. The prototypes are
// value-typed (New returns a value, not a pointer); estoria handles making
// them addressable for unmarshaling. ApplyTo implementations are pure state
// transitions: they clone the order, apply the change, and return the result.
//
// ApplyTo is also where the fulfillment state machine lives. Every transition
// is validated here, so an invalid event can never corrupt an order — even if
// a buggy handler were to append one, hydration would fail loudly instead of
// producing an order that skipped a step.

// OrderPlaced creates the order with its customer and line items. It must be
// the first event on the stream.
type OrderPlaced struct {
	Customer string     `json:"customer"`
	Items    []LineItem `json:"items"`
}

func (OrderPlaced) EventType() string               { return "orderplaced" }
func (OrderPlaced) New() estoria.EntityEvent[Order] { return OrderPlaced{} }
func (e OrderPlaced) ApplyTo(_ context.Context, o Order) (Order, error) {
	if o.Status != "" {
		return o, fmt.Errorf("order has already been placed (status %q)", o.Status)
	}
	if len(e.Items) == 0 {
		return o, fmt.Errorf("an order requires at least one line item")
	}

	next := o.clone()
	next.Customer = e.Customer
	next.Items = make([]LineItem, len(e.Items))
	copy(next.Items, e.Items)

	next.TotalCents = 0
	for _, item := range e.Items {
		next.TotalCents += int64(item.Qty) * item.PriceCents
	}

	next.Status = StatusPlaced
	return next, nil
}

// OrderPaid records a successful payment for a placed order.
type OrderPaid struct {
	Method string `json:"method"`
}

func (OrderPaid) EventType() string               { return "orderpaid" }
func (OrderPaid) New() estoria.EntityEvent[Order] { return OrderPaid{} }
func (e OrderPaid) ApplyTo(_ context.Context, o Order) (Order, error) {
	if o.Status != StatusPlaced {
		return o, fmt.Errorf("cannot pay an order in status %q", o.Status)
	}

	next := o.clone()
	next.Status = StatusPaid
	return next, nil
}

// OrderPicked records that a paid order has been picked and packed.
type OrderPicked struct{}

func (OrderPicked) EventType() string               { return "orderpicked" }
func (OrderPicked) New() estoria.EntityEvent[Order] { return OrderPicked{} }
func (e OrderPicked) ApplyTo(_ context.Context, o Order) (Order, error) {
	if o.Status != StatusPaid {
		return o, fmt.Errorf("cannot pick an order in status %q", o.Status)
	}

	next := o.clone()
	next.Status = StatusPicked
	return next, nil
}

// OrderShipped records a picked order leaving the warehouse with a carrier.
type OrderShipped struct {
	Carrier  string `json:"carrier"`
	Tracking string `json:"tracking"`
}

func (OrderShipped) EventType() string               { return "ordershipped" }
func (OrderShipped) New() estoria.EntityEvent[Order] { return OrderShipped{} }
func (e OrderShipped) ApplyTo(_ context.Context, o Order) (Order, error) {
	if o.Status != StatusPicked {
		return o, fmt.Errorf("cannot ship an order in status %q", o.Status)
	}

	next := o.clone()
	next.Status = StatusShipped
	return next, nil
}

// OrderDelivered records a shipped order reaching the customer. It is the
// happy-path terminal state.
type OrderDelivered struct{}

func (OrderDelivered) EventType() string               { return "orderdelivered" }
func (OrderDelivered) New() estoria.EntityEvent[Order] { return OrderDelivered{} }
func (e OrderDelivered) ApplyTo(_ context.Context, o Order) (Order, error) {
	if o.Status != StatusShipped {
		return o, fmt.Errorf("cannot deliver an order in status %q", o.Status)
	}

	next := o.clone()
	next.Status = StatusDelivered
	return next, nil
}

// OrderCancelled terminates an order before it ships. Once a package is on a
// truck (or already delivered), cancellation is no longer possible.
type OrderCancelled struct {
	Reason string `json:"reason"`
}

func (OrderCancelled) EventType() string               { return "ordercancelled" }
func (OrderCancelled) New() estoria.EntityEvent[Order] { return OrderCancelled{} }
func (e OrderCancelled) ApplyTo(_ context.Context, o Order) (Order, error) {
	switch o.Status {
	case StatusPlaced, StatusPaid, StatusPicked:
		next := o.clone()
		next.Status = StatusCancelled
		return next, nil
	default:
		return o, fmt.Errorf("cannot cancel an order in status %q", o.Status)
	}
}

// orderEventPrototypes lists every event type for registration with the
// aggregate store and for decoding raw stream and outbox events.
func orderEventPrototypes() []estoria.EntityEvent[Order] {
	return []estoria.EntityEvent[Order]{
		OrderPlaced{},
		OrderPaid{},
		OrderPicked{},
		OrderShipped{},
		OrderDelivered{},
		OrderCancelled{},
	}
}
