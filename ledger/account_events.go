package main

import (
	"time"

	"github.com/go-estoria/estoria"
)

// accountEventPrototypes registers every account event type with the
// aggregate store.
func accountEventPrototypes() []estoria.DomainEvent[Account] {
	return []estoria.DomainEvent[Account]{
		AccountOpened{},
		FundsDeposited{},
		FundsWithdrawn{},
	}
}

// AccountOpened records the account's creation.
type AccountOpened struct {
	Holder string    `json:"holder"`
	At     time.Time `json:"at"`
}

func (AccountOpened) EventType() string { return "accountopened" }

func (AccountOpened) New() estoria.DomainEvent[Account] { return AccountOpened{} }

func (e AccountOpened) ApplyTo(a Account) Account {
	a.Holder = e.Holder
	return a
}

// FundsDeposited records a deposit.
type FundsDeposited struct {
	Amount int64     `json:"amount"`
	At     time.Time `json:"at"`
}

func (FundsDeposited) EventType() string { return "fundsdeposited" }

func (FundsDeposited) New() estoria.DomainEvent[Account] { return FundsDeposited{} }

func (e FundsDeposited) ApplyTo(a Account) Account {
	a.Balance += e.Amount
	return a
}

// FundsWithdrawn records a withdrawal.
type FundsWithdrawn struct {
	Amount int64     `json:"amount"`
	At     time.Time `json:"at"`
}

func (FundsWithdrawn) EventType() string { return "fundswithdrawn" }

func (FundsWithdrawn) New() estoria.DomainEvent[Account] { return FundsWithdrawn{} }

func (e FundsWithdrawn) ApplyTo(a Account) Account {
	a.Balance -= e.Amount
	return a
}
