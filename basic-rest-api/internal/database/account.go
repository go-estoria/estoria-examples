package database

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

// An Account is a bank account. This is a business entity that satifies
// the requirements of the estoria.Entity interface. In doing so, it can
// be stored and retrieved via event sourcing.
type Account struct {
	ID      uuid.UUID
	Users   []string
	Balance int

	CreatedAt time.Time
	DeletedAt *time.Time
}

var _ estoria.Entity = (*Account)(nil)

// NewAccount creates a new account.
// This is a factory function that creates a new account with a unique ID.
// Estoria uses this function to create new instances of the entity when
// creating new aggregates and loading existing aggregates from the event store.
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

// EntityID returns the ID of the entity.
// Satifies the estoria.Entity interface.
func (a *Account) EntityID() typeid.UUID {
	return typeid.FromUUID(accountType, a.ID)
}

// SetEntityID sets the ID of the entity.
// Satifies the estoria.Entity interface.
func (a *Account) SetEntityID(id typeid.UUID) {
	a.ID = id.UUID()
}

// EventTypes returns the event types that can be applied to the entity.
// Estoria uses this method to determine which events can be used with the entity.
// Satifies the estoria.Entity interface.
func (a *Account) EventTypes() []estoria.EntityEvent {
	return []estoria.EntityEvent{
		&AccountCreatedEvent{},
		&AccountDeletedEvent{},
		&BalanceChangedEvent{},
		&UserAddedEvent{},
		&UserRemovedEvent{},
	}
}

// ApplyEvent applies an event to the entity.
// Type switching is used to determine which type of event is being applied.
// Satifies the estoria.Entity interface.
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
