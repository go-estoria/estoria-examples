package main

import (
	"fmt"
	"time"

	"github.com/go-estoria/estoria"
	"github.com/go-estoria/estoria/typeid"
	"github.com/gofrs/uuid/v5"
)

const (
	accountType = "account"
)

type Account struct {
	ID      uuid.UUID
	Users   []string
	Balance int

	CreatedAt time.Time
	DeletedAt *time.Time
}

var _ estoria.Entity = Account{}

func NewAccount(id uuid.UUID) Account {
	account := Account{
		ID:      id,
		Users:   make([]string, 0),
		Balance: 0,
	}

	return account
}

func (a Account) EntityID() typeid.UUID {
	return typeid.FromUUID(accountType, a.ID)
}

func (a Account) String() string {
	return fmt.Sprintf("Account %s {Users: %v} Balance: %d", a.ID, a.Users, a.Balance)
}
