package main

import (
	"github.com/gofrs/uuid/v5"
)

// Status is an order's position in the fulfillment pipeline. Transitions are
// validated by the command handlers in server.go before events are appended;
// the entity never sets its own status directly.
type Status string

const (
	StatusPlaced    Status = "placed"
	StatusPaid      Status = "paid"
	StatusPicked    Status = "picked"
	StatusShipped   Status = "shipped"
	StatusDelivered Status = "delivered"
	StatusCancelled Status = "cancelled"
)

// An Order is the aggregate root for a single customer order. Each order has
// its own event stream; its state is derived entirely by applying events and
// is never mutated directly.
type Order struct {
	ID         uuid.UUID  `json:"id"`
	Customer   string     `json:"customer"`
	Items      []LineItem `json:"items"`
	TotalCents int64      `json:"totalCents"`
	Status     Status     `json:"status"`
}

// A LineItem is one catalog entry within an order.
type LineItem struct {
	SKU        string `json:"sku"`
	Name       string `json:"name"`
	Qty        int    `json:"qty"`
	PriceCents int64  `json:"priceCents"`
}

// NewOrder is the estoria.StateFactory for Order aggregates.
func NewOrder(id uuid.UUID) Order {
	return Order{ID: id}
}

// clone returns a deep copy of the order so that ApplyTo implementations can
// return new state without mutating slices shared with previous versions.
func (o Order) clone() Order {
	c := o
	c.Items = make([]LineItem, len(o.Items))
	copy(c.Items, o.Items)
	return c
}

// UnitCount is the total number of units across all line items.
func (o Order) UnitCount() int {
	units := 0
	for _, item := range o.Items {
		units += item.Qty
	}
	return units
}
