package storage

import (
	"github.com/go-estoria/estoria"
)

// An AccountCreatedEvent indicates that a new Account has been created. It is the first event in every account's event stream.
type AccountCreatedEvent struct {
	Username string
}

func (AccountCreatedEvent) EventType() string { return "accountcreated" }

func (AccountCreatedEvent) New() estoria.EntityEvent { return &AccountCreatedEvent{} }

// An AccountDeletedEvent indicates that an Account has been deleted. It is the last event in any deleted account's event stream.
type AccountDeletedEvent struct {
	Reason string
}

func (AccountDeletedEvent) EventType() string { return "accountdeleted" }

func (AccountDeletedEvent) New() estoria.EntityEvent { return &AccountDeletedEvent{} }

// A UserAddedEvent indicates that a user has been added to an account.
type UserAddedEvent struct {
	Username string
}

func (UserAddedEvent) EventType() string { return "useradded" }

func (UserAddedEvent) New() estoria.EntityEvent { return &UserAddedEvent{} }

// A UserRemovedEvent indicates that a user has been removed from an account.
type UserRemovedEvent struct {
	Username string
}

func (UserRemovedEvent) EventType() string { return "userremoved" }

func (UserRemovedEvent) New() estoria.EntityEvent { return &UserRemovedEvent{} }

// BalanceChangedEvent is an example event representing a change in an account's balance.
type BalanceChangedEvent struct {
	Amount int
}

func (BalanceChangedEvent) EventType() string { return "balancechanged" }

func (BalanceChangedEvent) New() estoria.EntityEvent { return &BalanceChangedEvent{} }
