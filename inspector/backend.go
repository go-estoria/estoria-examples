// Backend construction and the optional-capability model.
//
// Estoria's core eventstore.Store interface is just ReadStream + AppendStream.
// That is enough for everything stream-scoped in this tool. But two features —
// listing the streams in a store, and reading all events in global order — are
// NOT part of the core interface. The SQLite and Postgres contrib stores each
// expose them as concrete methods (ListStreams and ReadAll), with per-backend
// return types.
//
// Rather than pretend a core interface exists for these, the inspector models
// them as optional capabilities: plain function fields that each backend's
// adapter fills in when its store supports the operation. A nil field means
// the capability is unavailable and the HTTP API answers 501, which the UI
// turns into a graceful fallback (manual stream-ID entry; no global feed tab).
package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	pgstore "github.com/go-estoria/estoria-contrib/postgres/eventstore"
	sqlstore "github.com/go-estoria/estoria-contrib/sqlite/eventstore"
	"github.com/go-estoria/estoria/eventstore"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "modernc.org/sqlite"
)

// streamInfo is the inspector's own view of a stream: the least common
// denominator of what the backends' ListStreams implementations return.
type streamInfo struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Version int64  `json:"version"`
}

// capabilities are the optional, backend-specific extras the inspector can
// use when the chosen store provides them. A nil field means the backend
// (or its driver in this tool) doesn't support that capability, and the UI
// degrades gracefully.
type capabilities struct {
	listStreams func(ctx context.Context) ([]streamInfo, error)
	readAll     func(ctx context.Context, opts eventstore.ReadStreamOptions) (eventstore.StreamIterator, error)
}

// allReader is the shape shared by stores that can read the global event
// feed. The SQLite and Postgres contrib stores happen to expose ReadAll with
// identical signatures, so a type assertion against this local interface is
// enough to adapt either of them — no core interface required.
type allReader interface {
	ReadAll(ctx context.Context, opts eventstore.ReadStreamOptions) (eventstore.StreamIterator, error)
}

// A backend is a connected event store plus whatever optional capabilities
// its driver could provide.
//
// The store is held ONLY as an eventstore.StreamReader: the inspector is
// strictly read-only and never calls AppendStream.
type backend struct {
	name   string
	label  string
	dsn    string
	reader eventstore.StreamReader
	caps   capabilities
	close  func()
}

// backends is the registry of supported event store backends. Adding a
// backend means adding one entry: connect to the store, hold it as a
// StreamReader, and fill in whichever capabilities its driver supports.
var backends = map[string]struct {
	label   string
	connect func(ctx context.Context, dsn string) (*backend, error)
}{
	"sqlite":   {label: "SQLite", connect: connectSQLite},
	"postgres": {label: "Postgres", connect: connectPostgres},
}

// backendNames returns the registered backend names, sorted.
func backendNames() []string {
	names := make([]string, 0, len(backends))
	for name := range backends {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// connectSQLite opens an EXISTING SQLite event store database. Being a
// read-only tool, the inspector never bootstraps a schema: if the file or the
// expected tables are missing, it fails at startup with a clear message.
//
// The DSN is a plain path to the database file.
func connectSQLite(ctx context.Context, path string) (*backend, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("database file %q does not exist (this tool is read-only and never creates one; "+
			"run an example that writes to SQLite first, e.g. ../kanban)", path)
	}

	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", path))
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	// Verify the tables written by the contrib SQLite store's default
	// strategy exist, rather than presenting an empty inspector over a
	// database that was never an event store.
	for _, table := range []string{"event", "stream"} {
		var name string
		err := db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("%q is not an estoria event store database: missing table %q "+
				"(expected the tables created by the estoria-contrib SQLite default strategy: event, stream)",
				path, table)
		}
	}

	// NewDefaultStrategy is implied: sqlstore.New falls back to it, and it is
	// the strategy that provides ListStreams and ReadAll.
	store, err := sqlstore.New(db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("creating event store: %w", err)
	}

	caps := capabilities{
		// The SQLite store's ListStreams returns its own strategy.StreamMetadata
		// type, so this closure adapts it to the inspector's streamInfo.
		listStreams: func(ctx context.Context) ([]streamInfo, error) {
			metas, err := store.ListStreams(ctx)
			if err != nil {
				return nil, err
			}
			infos := make([]streamInfo, len(metas))
			for i, meta := range metas {
				infos[i] = streamInfo{
					ID:      meta.StreamID.String(),
					Type:    meta.StreamID.Type,
					Version: meta.LastOffset,
				}
			}
			return infos, nil
		},
	}

	// ReadAll needs no adapting — the concrete method already has the shape
	// the inspector wants, so capability discovery is a type assertion.
	if reader, ok := any(store).(allReader); ok {
		caps.readAll = reader.ReadAll
	}

	return &backend{
		name:   "sqlite",
		label:  "SQLite",
		dsn:    path,
		reader: store, // StreamReader only: never the store's write side
		caps:   caps,
		close:  func() { db.Close() },
	}, nil
}

// connectPostgres connects to an existing Postgres event store. As with
// SQLite, no schema is bootstrapped: missing tables are a startup error.
//
// The DSN is a Postgres connection URL, e.g.
// postgres://estoria:estoria@localhost:5433/estoria?sslmode=disable
func connectPostgres(ctx context.Context, dsn string) (*backend, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("creating connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	// Verify the tables written by the contrib Postgres store's default
	// strategy exist before serving anything.
	for _, table := range []string{"event", "stream"} {
		var regclass *string
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1)`, table).Scan(&regclass); err != nil || regclass == nil {
			pool.Close()
			return nil, fmt.Errorf("database at %q is not an estoria event store: missing table %q "+
				"(expected the tables created by the estoria-contrib Postgres default strategy: event, stream)",
				redactDSN(dsn), table)
		}
	}

	store, err := pgstore.New(pool)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("creating event store: %w", err)
	}

	caps := capabilities{
		// Same adaptation as SQLite, against the Postgres store's OWN
		// strategy.StreamMetadata type. The two closures look identical but
		// compile against different concrete types — which is exactly the
		// per-backend adapter cost this tool exists to demonstrate.
		listStreams: func(ctx context.Context) ([]streamInfo, error) {
			metas, err := store.ListStreams(ctx)
			if err != nil {
				return nil, err
			}
			infos := make([]streamInfo, len(metas))
			for i, meta := range metas {
				infos[i] = streamInfo{
					ID:      meta.StreamID.String(),
					Type:    meta.StreamID.Type,
					Version: meta.LastOffset,
				}
			}
			return infos, nil
		},
	}

	if reader, ok := any(store).(allReader); ok {
		caps.readAll = reader.ReadAll
	}

	return &backend{
		name:   "postgres",
		label:  "Postgres",
		dsn:    dsn,
		reader: store, // StreamReader only: never the store's write side
		caps:   caps,
		close:  pool.Close,
	}, nil
}

// redactDSN removes the password from a DSN before it is displayed or served
// over the API. Non-URL DSNs (e.g. SQLite file paths) pass through unchanged.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		// key=value style DSNs aren't URLs; drop any password=... token
		if strings.Contains(dsn, "password=") {
			fields := strings.Fields(dsn)
			kept := fields[:0]
			for _, f := range fields {
				if !strings.HasPrefix(f, "password=") {
					kept = append(kept, f)
				}
			}
			return strings.Join(kept, " ")
		}
		return dsn
	}

	if _, hasPassword := u.User.Password(); hasPassword {
		u.User = url.User(u.User.Username())
	}
	return u.String()
}
