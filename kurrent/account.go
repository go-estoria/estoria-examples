package main

import (
	"fmt"
	"time"

	"github.com/gofrs/uuid/v5"
)

// Account is an example entity type.
// In this example project, it is the aggregate root
// for which events are stored, loaded, and applied.
type Account struct {
	ID      uuid.UUID
	Users   []string
	Balance int

	CreatedAt time.Time
	DeletedAt *time.Time
}

// NewAccount creates a new Account entity with the given ID.
// This factory function is used by Estoria when loading the
// entity via the aggregate store.
func NewAccount(id uuid.UUID) Account {
	account := Account{
		ID:      id,
		Users:   make([]string, 0),
		Balance: 0,
	}

	return account
}

// String implements the fmt.Stringer interface for easy printing.
// This is not required by Estoria, but useful for demonstration purposes.
func (a Account) String() string {
	return fmt.Sprintf("Account %s {Users: %v} Balance: %d", a.ID, a.Users, a.Balance)
}
