package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// This file exists only for the hosted demo. Running the example locally, none
// of it is active: nothing resets and nothing is rate limited.

// resetStore drops everything this app owns and lets startup recreate it.
//
// Reset happens at boot rather than on a timer, because unlike the other
// examples this one has substantial in-memory state derived from storage — the
// lifecycle orchestrator, the serving router, the running processors — and
// rebuilding that state is exactly what startup already does. Wiping first and
// letting the normal path run is simpler and less likely to be subtly wrong
// than tearing down and re-establishing a live rebuild mid-flight.
//
// It also handles a trap that only appears on a hosted service. A deploy
// carrying a library upgrade may expect columns the existing tables don't have,
// and `CREATE TABLE IF NOT EXISTS` is a no-op on a table that already exists —
// so the app would start, serve reads, pass its health check, and fail every
// write. Dropping first makes that drift self-heal.
//
// The versioned projection tables are discovered rather than listed: how many
// exist depends on how many rebuilds visitors ran before the reset.
func resetStore(ctx context.Context, pool *pgxpool.Pool) error {
	tables, err := projectionTables(ctx, pool)
	if err != nil {
		return err
	}

	// The event store's own tables, the checkpoint table, and every
	// account_balances_vN built by a rebuild.
	tables = append(tables, "event", "stream", "projection_checkpoints")

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning reset transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, table := range tables {
		if _, err := tx.Exec(ctx, "DROP TABLE IF EXISTS "+quoteIdent(table)); err != nil {
			return fmt.Errorf("dropping table %s: %w", table, err)
		}
	}

	return tx.Commit(ctx)
}

// projectionTables lists the versioned read-model tables currently in the
// database — account_balances_v1, account_balances_v2, and so on.
func projectionTables(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT tablename FROM pg_tables
		WHERE schemaname = current_schema() AND tablename LIKE $1`,
		projectionName+"_v%")
	if err != nil {
		return nil, fmt.Errorf("listing projection tables: %w", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scanning table name: %w", err)
		}
		tables = append(tables, name)
	}

	return tables, rows.Err()
}

// quoteIdent quotes a SQL identifier. Every name reaching it is either a
// constant or a pg_tables row matching this app's own prefix, but the tables
// are interpolated rather than parameterized, so quoting is not optional.
func quoteIdent(name string) string {
	return `"` + name + `"`
}
