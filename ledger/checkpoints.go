package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-estoria/estoria/projection"
	"github.com/go-estoria/estoria/projection/checkpointstore"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A checkpointStore persists projection progress in Postgres, one row per
// projection version. Saves are last-write-wins and refresh updated_at even
// at an unchanged position — checkpoint recency is the liveness signal the
// lifecycle console reads. Deletes tolerate concurrent retirement repairs:
// every overlapping caller sees success or ErrCheckpointNotFound.
type checkpointStore struct {
	pool *pgxpool.Pool
}

// Schema returns the DDL for the checkpoint table, applied at startup
// alongside the event store's schema.
func (s *checkpointStore) Schema() string {
	return `CREATE TABLE IF NOT EXISTS projection_checkpoints (
		name       text NOT NULL,
		version    int  NOT NULL,
		position   bigint NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (name, version)
	)`
}

func (s *checkpointStore) Load(ctx context.Context, id projection.ID) (checkpointstore.Checkpoint, error) {
	checkpoint := checkpointstore.Checkpoint{ProjectionID: id}

	err := s.pool.QueryRow(ctx,
		"SELECT position, updated_at FROM projection_checkpoints WHERE name = $1 AND version = $2",
		id.Name, id.Version,
	).Scan(&checkpoint.Position, &checkpoint.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return checkpointstore.Checkpoint{}, checkpointstore.ErrCheckpointNotFound
	} else if err != nil {
		return checkpointstore.Checkpoint{}, fmt.Errorf("loading checkpoint for %s: %w", id, err)
	}

	return checkpoint, nil
}

func (s *checkpointStore) Save(ctx context.Context, id projection.ID, position int64) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO projection_checkpoints (name, version, position) VALUES ($1, $2, $3)
		 ON CONFLICT (name, version) DO UPDATE SET position = $3, updated_at = now()`,
		id.Name, id.Version, position)
	if err != nil {
		return fmt.Errorf("saving checkpoint for %s: %w", id, err)
	}

	return nil
}

func (s *checkpointStore) Delete(ctx context.Context, id projection.ID) error {
	tag, err := s.pool.Exec(ctx,
		"DELETE FROM projection_checkpoints WHERE name = $1 AND version = $2",
		id.Name, id.Version)
	if err != nil {
		return fmt.Errorf("deleting checkpoint for %s: %w", id, err)
	}

	if tag.RowsAffected() == 0 {
		return checkpointstore.ErrCheckpointNotFound
	}

	return nil
}
