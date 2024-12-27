package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-estoria/estoria"
)

type AccountCreatedEvent struct {
	Username  string
	CreatedAt time.Time
}

func (AccountCreatedEvent) EventType() string { return "accountcreated" }

func (AccountCreatedEvent) New() estoria.EntityEvent[Account] { return &AccountCreatedEvent{} }

func (e AccountCreatedEvent) ApplyTo(_ context.Context, account Account) (Account, error) {
	slog.Info("applying account created event", "username", e.Username)
	if !account.CreatedAt.IsZero() {
		return account, fmt.Errorf("account already created")
	} else if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}

	account.CreatedAt = e.CreatedAt
	account.Users = append(account.Users, e.Username)
	return account, nil
}

type AccountDeletedEvent struct {
	Reason    string
	DeletedAt time.Time
}

func (AccountDeletedEvent) EventType() string { return "accountdeleted" }

func (AccountDeletedEvent) New() estoria.EntityEvent[Account] { return &AccountDeletedEvent{} }

func (e AccountDeletedEvent) ApplyTo(_ context.Context, account Account) (Account, error) {
	slog.Info("applying account deleted event", "reason", e.Reason)
	if account.DeletedAt != nil {
		return account, fmt.Errorf("account already deleted")
	} else if e.DeletedAt.IsZero() {
		e.DeletedAt = time.Now()
	}

	account.DeletedAt = &e.DeletedAt
	return account, nil
}

type UserAddedEvent struct {
	Username string
	AddedAt  time.Time
}

func (UserAddedEvent) EventType() string { return "useradded" }

func (UserAddedEvent) New() estoria.EntityEvent[Account] { return &UserAddedEvent{} }

func (e UserAddedEvent) ApplyTo(_ context.Context, account Account) (Account, error) {
	slog.Info("applying user created event", "username", e.Username)
	account.Users = append(account.Users, e.Username)
	return account, nil
}

type UserRemovedEvent struct {
	Username  string
	RemovedAt time.Time
}

func (UserRemovedEvent) EventType() string { return "userremoved" }

func (UserRemovedEvent) New() estoria.EntityEvent[Account] { return &UserRemovedEvent{} }

func (e UserRemovedEvent) ApplyTo(_ context.Context, account Account) (Account, error) {
	slog.Info("applying user deleted event", "username", e.Username)
	for i, user := range account.Users {
		if user == e.Username {
			account.Users = append(account.Users[:i], account.Users[i+1:]...)
			return account, nil
		}
	}
	return account, fmt.Errorf("user %s not found", e.Username)
}

type BalanceChangedEvent struct {
	Amount    int
	ChangedAt time.Time
}

func (BalanceChangedEvent) EventType() string { return "balancechanged" }

func (BalanceChangedEvent) New() estoria.EntityEvent[Account] { return &BalanceChangedEvent{} }

func (e BalanceChangedEvent) ApplyTo(_ context.Context, account Account) (Account, error) {
	slog.Info("applying balance changed event", "amount", e.Amount)
	account.Balance += e.Amount
	return account, nil
}
