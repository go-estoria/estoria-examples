package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/go-estoria/estoria"
	"github.com/gofrs/uuid/v5"
)

// An Account is a ledger account. It is the aggregate's state, produced
// entirely by folding the account's events; commands validate against it
// before appending anything.
type Account struct {
	ID      uuid.UUID
	Holder  string
	Balance int64 // cents
}

// NewAccount is the aggregate store's state factory.
func NewAccount(id uuid.UUID) Account {
	return Account{ID: id}
}

// Opened reports whether the account exists: an account with no holder has
// no AccountOpened event in its stream.
func (a Account) Opened() bool {
	return a.Holder != ""
}

// Open validates opening the account, returning the event to append.
func (a Account) Open(holder string, at time.Time) (estoria.DomainEvent[Account], error) {
	if a.Opened() {
		return nil, errors.New("account is already open")
	}

	if holder == "" {
		return nil, errors.New("account holder is required")
	}

	return AccountOpened{Holder: holder, At: at}, nil
}

// Deposit validates a deposit, returning the event to append.
func (a Account) Deposit(amount int64, at time.Time) (estoria.DomainEvent[Account], error) {
	if !a.Opened() {
		return nil, errors.New("account is not open")
	}

	if amount <= 0 {
		return nil, errors.New("deposit amount must be positive")
	}

	return FundsDeposited{Amount: amount, At: at}, nil
}

// Withdraw validates a withdrawal, returning the event to append.
func (a Account) Withdraw(amount int64, at time.Time) (estoria.DomainEvent[Account], error) {
	if !a.Opened() {
		return nil, errors.New("account is not open")
	}

	if amount <= 0 {
		return nil, errors.New("withdrawal amount must be positive")
	}

	if amount > a.Balance {
		return nil, fmt.Errorf("insufficient funds: balance is %d, withdrawal is %d", a.Balance, amount)
	}

	return FundsWithdrawn{Amount: amount, At: at}, nil
}
