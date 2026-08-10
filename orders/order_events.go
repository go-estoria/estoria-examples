package main

import (
	"github.com/go-estoria/estoria"
)

// Each event below implements estoria.DomainEvent[Order]. The prototypes are
// value-typed (New returns a value, not a pointer); estoria handles making
// them addressable for unmarshaling. ApplyTo implementations are pure, total
// state transitions: they clone the order, apply the change, and return the
// result.
//
// The fulfillment state machine is enforced by the command handlers in
// server.go: every transition is validated against the loaded order before its
// event is appended, so an invalid event never reaches a stream. By the time
// ApplyTo runs, the event is a fact.

// OrderPlaced creates the order with its customer and line items. It must be
// the first event on the stream.
type OrderPlaced struct {
	Customer string     `json:"customer"`
	Items    []LineItem `json:"items"`
}

func (OrderPlaced) EventType() string               { return "orderplaced" }
func (OrderPlaced) New() estoria.DomainEvent[Order] { return OrderPlaced{} }
func (e OrderPlaced) ApplyTo(o Order) Order {
	next := o.clone()
	next.Customer = e.Customer
	next.Items = make([]LineItem, len(e.Items))
	copy(next.Items, e.Items)

	next.TotalCents = 0
	for _, item := range e.Items {
		next.TotalCents += int64(item.Qty) * item.PriceCents
	}

	next.Status = StatusPlaced
	return next
}

// OrderPaid records a successful payment for a placed order.
type OrderPaid struct {
	Method string `json:"method"`
}

func (OrderPaid) EventType() string               { return "orderpaid" }
func (OrderPaid) New() estoria.DomainEvent[Order] { return OrderPaid{} }
func (e OrderPaid) ApplyTo(o Order) Order {
	next := o.clone()
	next.Status = StatusPaid
	return next
}

// OrderPicked records that a paid order has been picked and packed.
type OrderPicked struct{}

func (OrderPicked) EventType() string               { return "orderpicked" }
func (OrderPicked) New() estoria.DomainEvent[Order] { return OrderPicked{} }
func (e OrderPicked) ApplyTo(o Order) Order {
	next := o.clone()
	next.Status = StatusPicked
	return next
}

// OrderShipped records a picked order leaving the warehouse with a carrier.
type OrderShipped struct {
	Carrier  string `json:"carrier"`
	Tracking string `json:"tracking"`
}

func (OrderShipped) EventType() string               { return "ordershipped" }
func (OrderShipped) New() estoria.DomainEvent[Order] { return OrderShipped{} }
func (e OrderShipped) ApplyTo(o Order) Order {
	next := o.clone()
	next.Status = StatusShipped
	return next
}

// OrderDelivered records a shipped order reaching the customer. It is the
// happy-path terminal state.
type OrderDelivered struct{}

func (OrderDelivered) EventType() string               { return "orderdelivered" }
func (OrderDelivered) New() estoria.DomainEvent[Order] { return OrderDelivered{} }
func (e OrderDelivered) ApplyTo(o Order) Order {
	next := o.clone()
	next.Status = StatusDelivered
	return next
}

// OrderCancelled terminates an order before it ships. Once a package is on a
// truck (or already delivered), cancellation is no longer possible — a rule
// the command handlers enforce before appending this event.
type OrderCancelled struct {
	Reason string `json:"reason"`
}

func (OrderCancelled) EventType() string               { return "ordercancelled" }
func (OrderCancelled) New() estoria.DomainEvent[Order] { return OrderCancelled{} }
func (e OrderCancelled) ApplyTo(o Order) Order {
	next := o.clone()
	next.Status = StatusCancelled
	return next
}

// orderEventPrototypes lists every event type for registration with the
// aggregate store and for decoding raw stream and outbox events.
func orderEventPrototypes() []estoria.DomainEvent[Order] {
	return []estoria.DomainEvent[Order]{
		OrderPlaced{},
		OrderPaid{},
		OrderPicked{},
		OrderShipped{},
		OrderDelivered{},
		OrderCancelled{},
	}
}
