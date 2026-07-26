package main

import (
	"fmt"
	"math/rand/v2"
)

// A tiny hardcoded catalog so that "New order" can fabricate a plausible
// order with one click. A real system would source these from inventory.
var catalog = []LineItem{
	{SKU: "TEE-001", Name: "Estoria Tee", PriceCents: 2499},
	{SKU: "MUG-002", Name: "Event Sourcing Mug", PriceCents: 1450},
	{SKU: "HDY-003", Name: "CQRS Hoodie", PriceCents: 5995},
	{SKU: "STK-004", Name: "Aggregate Sticker Pack", PriceCents: 650},
	{SKU: "CAP-005", Name: "Outbox Cap", PriceCents: 2199},
	{SKU: "NBK-006", Name: "Projection Notebook", PriceCents: 1275},
	{SKU: "BTL-007", Name: "Stream Bottle", PriceCents: 1899},
	{SKU: "PIN-008", Name: "Snapshot Enamel Pin", PriceCents: 950},
}

var customers = []string{
	"Ada Lovelace",
	"Grace Hopper",
	"Alan Turing",
	"Barbara Liskov",
	"Edsger Dijkstra",
	"Margaret Hamilton",
	"Donald Knuth",
	"Leslie Lamport",
	"Frances Allen",
	"Tony Hoare",
}

var paymentMethods = []string{"visa", "mastercard", "amex", "paypal"}

var carriers = []string{"UPS", "FedEx", "USPS", "DHL"}

// randomOrder fabricates an OrderPlaced event: a random customer buying 1-4
// distinct catalog items, each in a quantity of 1-3.
func randomOrder() OrderPlaced {
	picks := rand.Perm(len(catalog))[:1+rand.IntN(4)]

	items := make([]LineItem, len(picks))
	for i, p := range picks {
		items[i] = catalog[p]
		items[i].Qty = 1 + rand.IntN(3)
	}

	return OrderPlaced{
		Customer: customers[rand.IntN(len(customers))],
		Items:    items,
	}
}

// randomPayment picks a payment method for the demo "Pay" command.
func randomPayment() OrderPaid {
	return OrderPaid{Method: paymentMethods[rand.IntN(len(paymentMethods))]}
}

// randomShipment fabricates a carrier and tracking number for the demo
// "Ship" command.
func randomShipment() OrderShipped {
	return OrderShipped{
		Carrier:  carriers[rand.IntN(len(carriers))],
		Tracking: fmt.Sprintf("1Z%09d", rand.IntN(1_000_000_000)),
	}
}

// fmtMoney renders cents as dollars, e.g. 1234 -> "$12.34".
func fmtMoney(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s$%d.%02d", sign, cents/100, cents%100)
}
