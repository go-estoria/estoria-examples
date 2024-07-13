package storage

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/go-estoria/estoria"
	"github.com/go-estoria/estoria/typeid"
	"github.com/gofrs/uuid/v5"
)

type Client struct {
	accounts estoria.AggregateStore[*Account]
}

func NewClient(accounts estoria.AggregateStore[*Account]) *Client {
	return &Client{
		accounts: accounts,
	}
}

func (c *Client) CreateAccount(ctx context.Context, initialUser string) (*Account, error) {
	if initialUser == "" {
		return nil, fmt.Errorf("initial user cannot be empty")
	}

	aggregate, err := c.accounts.NewAggregate(nil) // passing nil generates a new aggregate ID
	if err != nil {
		return nil, fmt.Errorf("creating aggregate: %w", err)
	}

	// append the initial event to the aggregate
	if err := aggregate.Append(
		&AccountCreatedEvent{Username: initialUser},
	); err != nil {
		return nil, fmt.Errorf("appending event: %w", err)
	}

	slog.Info("saving aggregate", "id", aggregate.ID())

	if err := c.accounts.Save(ctx, aggregate, estoria.SaveAggregateOptions{}); err != nil {
		return nil, fmt.Errorf("saving aggregate: %w", err)
	}

	slog.Info("saved aggregate", "id", aggregate.ID())

	// return the entity from the aggregate
	return aggregate.Entity(), nil
}

func (c *Client) GetAccount(ctx context.Context, accountID uuid.UUID) (*Account, error) {
	aggregateID := typeid.FromUUID(accountType, accountID)
	aggregate, err := c.accounts.Load(ctx, aggregateID, estoria.LoadAggregateOptions{})
	if err != nil {
		return nil, fmt.Errorf("loading aggregate: %w", err)
	}

	slog.Info("loaded aggregate", "id", aggregate.ID(), "entity", aggregate.Entity().String())

	return aggregate.Entity(), nil
}

func (c *Client) DeleteAccount(ctx context.Context, accountID uuid.UUID, reason string) error {
	aggregateID := typeid.FromUUID(accountType, accountID)
	aggregate, err := c.accounts.Load(ctx, aggregateID, estoria.LoadAggregateOptions{})
	if err != nil {
		return fmt.Errorf("loading aggregate: %w", err)
	}

	// append the deletion event to the aggregate
	if err := aggregate.Append(&AccountDeletedEvent{
		Reason: reason,
	}); err != nil {
		return fmt.Errorf("appending event: %w", err)
	}

	if err := c.accounts.Save(ctx, aggregate, estoria.SaveAggregateOptions{}); err != nil {
		return fmt.Errorf("saving aggregate: %w", err)
	}

	return nil
}
