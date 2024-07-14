package main

import (
	"github.com/go-estoria/estoria"
)

type AccountCreatedEvent struct {
	Username string
}

func (AccountCreatedEvent) EventType() string { return "accountcreated" }

func (AccountCreatedEvent) New() estoria.EntityEvent { return &AccountCreatedEvent{} }

type AccountDeletedEvent struct {
	Reason string
}

func (AccountDeletedEvent) EventType() string { return "accountdeleted" }

func (AccountDeletedEvent) New() estoria.EntityEvent { return &AccountDeletedEvent{} }

type UserAddedEvent struct {
	Username string
}

func (UserAddedEvent) EventType() string { return "useradded" }

func (UserAddedEvent) New() estoria.EntityEvent { return &UserAddedEvent{} }

type UserRemovedEvent struct {
	Username string
}

func (UserRemovedEvent) EventType() string { return "userremoved" }

func (UserRemovedEvent) New() estoria.EntityEvent { return &UserRemovedEvent{} }

type BalanceChangedEvent struct {
	Amount int
}

func (BalanceChangedEvent) EventType() string { return "balancechanged" }

func (BalanceChangedEvent) New() estoria.EntityEvent { return &BalanceChangedEvent{} }
