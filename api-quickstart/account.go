package main

import (
	"context"
	"fmt"
	"log/slog"
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

var _ estoria.Entity = (*Account)(nil)

func NewAccount() *Account {
	tid, err := typeid.NewUUID(accountType)
	if err != nil {
		panic(err)
	}

	slog.Debug("creating new account", "type", accountType, "id", tid)
	return &Account{
		ID:      tid.UUID(),
		Users:   make([]string, 0),
		Balance: 0,
	}
}

func (a *Account) EntityID() typeid.UUID {
	return typeid.FromUUID(accountType, a.ID)
}

func (a *Account) SetEntityID(id typeid.UUID) {
	a.ID = id.UUID()
}

func (a *Account) EventTypes() []estoria.EntityEvent {
	return []estoria.EntityEvent{
		&AccountCreatedEvent{},
		&AccountDeletedEvent{},
		&BalanceChangedEvent{},
		&UserAddedEvent{},
		&UserRemovedEvent{},
	}
}

func (a *Account) ApplyEvent(_ context.Context, event estoria.EntityEvent) error {
	switch e := event.(type) {

	case *AccountCreatedEvent:
		slog.Info("applying account created event", "username", e.Username)
		if !a.CreatedAt.IsZero() {
			return fmt.Errorf("account already created")
		}

		a.CreatedAt = time.Now()
		a.Users = append(a.Users, e.Username)
		return nil

	case *AccountDeletedEvent:
		slog.Info("applying account deleted event", "reason", e.Reason)
		if a.DeletedAt != nil {
			return fmt.Errorf("account already deleted")
		}

		now := time.Now()
		a.DeletedAt = &now
		return nil

	case *BalanceChangedEvent:
		slog.Info("applying balance changed event", "amount", e.Amount)
		a.Balance += e.Amount
		return nil

	case *UserAddedEvent:
		slog.Info("applying user created event", "username", e.Username)
		a.Users = append(a.Users, e.Username)
		return nil

	case *UserRemovedEvent:
		slog.Info("applying user deleted event", "username", e.Username)
		for i, user := range a.Users {
			if user == e.Username {
				a.Users = append(a.Users[:i], a.Users[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("user %s not found", e.Username)

	default:
		return fmt.Errorf("invalid event type: %T", event)
	}
}

func (a *Account) String() string {
	return fmt.Sprintf("Account %s {Users: %v} Balance: %d", a.ID, a.Users, a.Balance)
}
