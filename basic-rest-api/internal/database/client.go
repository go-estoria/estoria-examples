package database

import (
	"context"
	"fmt"

	"github.com/go-estoria/estoria/aggregatestore"
	"github.com/go-estoria/estoria/typeid"
	"github.com/gofrs/uuid/v5"
)

// Client is a database client that provides methods for interacting with the database layer.
// It is responsible for creating, reading, updating, and deleting Accounts.
// It is dependent on an Estoria AggregateStore that can load and store Account aggregates.
type Client struct {
	accounts aggregatestore.Store[*Account]
}

// NewClient creates a new database client using the provided aggregate store.
func NewClient(accounts aggregatestore.Store[*Account]) *Client {
	return &Client{
		accounts: accounts,
	}
}

// CreateAccount creates a new account with the provided initial user.
func (c *Client) CreateAccount(ctx context.Context, initialUser string) (*Account, error) {
	if initialUser == "" {
		return nil, fmt.Errorf("initial user cannot be empty")
	}

	aggregate, err := c.accounts.NewAggregate(nil) // passing nil generates a new aggregate ID
	if err != nil {
		return nil, fmt.Errorf("creating aggregate: %w", err)
	}

	if err := aggregate.Append(
		&AccountCreatedEvent{Username: initialUser},
	); err != nil {
		return nil, fmt.Errorf("appending event: %w", err)
	}

	if err := c.accounts.Save(ctx, aggregate, aggregatestore.SaveOptions{}); err != nil {
		return nil, fmt.Errorf("saving aggregate: %w", err)
	}

	return aggregate.Entity(), nil
}

// GetAccount retrieves an account by its ID.
func (c *Client) GetAccount(ctx context.Context, accountID uuid.UUID) (*Account, error) {
	aggregateID := typeid.FromUUID(accountType, accountID)
	aggregate, err := c.accounts.Load(ctx, aggregateID, aggregatestore.LoadOptions{})
	if err != nil {
		return nil, fmt.Errorf("loading aggregate: %w", err)
	}

	return aggregate.Entity(), nil
}

// DeleteAccount deletes an account by its ID.
func (c *Client) DeleteAccount(ctx context.Context, accountID uuid.UUID, reason string) error {
	aggregateID := typeid.FromUUID(accountType, accountID)
	aggregate, err := c.accounts.Load(ctx, aggregateID, aggregatestore.LoadOptions{})
	if err != nil {
		return fmt.Errorf("loading aggregate: %w", err)
	}

	// by definition, deleting an aggregate is a soft delete
	if err := aggregate.Append(&AccountDeletedEvent{
		Reason: reason,
	}); err != nil {
		return fmt.Errorf("appending event: %w", err)
	}

	if err := c.accounts.Save(ctx, aggregate, aggregatestore.SaveOptions{}); err != nil {
		return fmt.Errorf("saving aggregate: %w", err)
	}

	return nil
}
