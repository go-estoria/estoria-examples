package main

import (
	"time"

	"github.com/go-estoria/estoria"
)

type AccountCreatedEvent struct {
	Username  string
	CreatedAt time.Time
}

func (AccountCreatedEvent) EventType() string { return "accountcreated" }

func (AccountCreatedEvent) New() estoria.DomainEvent[Account] { return &AccountCreatedEvent{} }

func (e AccountCreatedEvent) ApplyTo(account Account) Account {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}

	account.CreatedAt = e.CreatedAt
	account.Users = append(account.Users, e.Username)
	return account
}

type AccountDeletedEvent struct {
	Reason    string
	DeletedAt time.Time
}

func (AccountDeletedEvent) EventType() string { return "accountdeleted" }

func (AccountDeletedEvent) New() estoria.DomainEvent[Account] { return &AccountDeletedEvent{} }

func (e AccountDeletedEvent) ApplyTo(account Account) Account {
	if e.DeletedAt.IsZero() {
		e.DeletedAt = time.Now()
	}

	account.DeletedAt = &e.DeletedAt
	return account
}

type UserAddedEvent struct {
	Username string
	AddedAt  time.Time
}

func (UserAddedEvent) EventType() string { return "useradded" }

func (UserAddedEvent) New() estoria.DomainEvent[Account] { return &UserAddedEvent{} }

func (e UserAddedEvent) ApplyTo(account Account) Account {
	account.Users = append(account.Users, e.Username)
	return account
}

type UserRemovedEvent struct {
	Username  string
	RemovedAt time.Time
}

func (UserRemovedEvent) EventType() string { return "userremoved" }

func (UserRemovedEvent) New() estoria.DomainEvent[Account] { return &UserRemovedEvent{} }

func (e UserRemovedEvent) ApplyTo(account Account) Account {
	for i, user := range account.Users {
		if user == e.Username {
			account.Users = append(account.Users[:i], account.Users[i+1:]...)
			break
		}
	}
	return account
}

type BalanceChangedEvent struct {
	Amount    int
	ChangedAt time.Time
}

func (BalanceChangedEvent) EventType() string { return "balancechanged" }

func (BalanceChangedEvent) New() estoria.DomainEvent[Account] { return &BalanceChangedEvent{} }

func (e BalanceChangedEvent) ApplyTo(account Account) Account {
	account.Balance += e.Amount
	return account
}
