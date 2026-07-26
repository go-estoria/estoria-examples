# Inspector — a read-only event stream browser

A generic web tool for looking inside **any estoria event store**: browse the streams
it contains, page through a stream's events in either direction, expand payloads and
metadata, and tail the global event feed as new events arrive.

The inspector is **strictly read-only** — it never appends an event, and the server
only ever holds an `eventstore.StreamReader`.

```sh
# run the kanban example first so there's something to inspect, then:
go run .
# open http://localhost:8085
```

By default it opens `../kanban/kanban.db`, so if you just ran the kanban example you
can watch its board stream (and its snapshot stream) immediately. Point it anywhere:

```sh
go run . -dsn ../fleet/fleet.db            # the fleet example's store
go run . -dsn ../chess/chess.db            # the chess example's store
go run . -backend postgres \
  -dsn "postgres://estoria:estoria@localhost:5433/estoria?sslmode=disable"
                                           # the orders example's Postgres
```

For a live demo, run the kanban example and the inspector side by side, enable
**Live tail**, and drag some cards around.

## What it demonstrates

| Estoria feature | Where to see it |
| --------------- | --------------- |
| Everything stream-scoped via the core `eventstore.StreamReader` alone | `handleStreamEvents` in [`server.go`](./server.go) — works on **any** backend |
| `ReadStreamOptions` paging semantics (`AfterVersion`, `Count`, `Direction`) | The Newest/Oldest-first toggle and "Load more" pager |
| Backend-specific extras (`ListStreams`, `ReadAll`) modeled as **optional capabilities** | [`backend.go`](./backend.go) — and the design question below |
| Graceful degradation when a capability is missing | 501 responses in [`server.go`](./server.go); the UI falls back to manual stream-ID entry |
| Global ordering via `Event.GlobalPosition` | The Global feed tab, and the tail cursor it polls with |
| Snapshots are just events in a parallel stream | Streams whose type ends in `snapshot` get a 📸 chip |
| One tool, many stores (SQLite and Postgres contrib stores) | The backend registry in [`backend.go`](./backend.go) |

## The design question

This example exists to make an open interface-design question concrete.

Estoria's core `eventstore.Store` interface is deliberately tiny:

```go
type Store interface {
    StreamReader // ReadStream
    StreamWriter // AppendStream
}
```

Two operations this tool wants — **listing the streams in a store** and **reading all
events in global order** — are not part of that contract. The SQLite and Postgres
contrib stores both provide them, but as *concrete methods*:

```go
// sqlite:   func (s *EventStore) ListStreams(ctx) ([]sqlitestrategy.StreamMetadata, error)
// postgres: func (s *EventStore) ListStreams(ctx) ([]pgstrategy.StreamMetadata, error)
//
// both:     func (s *EventStore) ReadAll(ctx, eventstore.ReadStreamOptions) (eventstore.StreamIterator, error)
```

Note the return types: each backend's `ListStreams` returns *its own* strategy
package's `StreamMetadata`. There is no shared interface to program against, so a
generic tool has two options, and this example implements one of them fully so the
trade-off can be judged on real code:

**The cost of keeping the core minimal** is visible in [`backend.go`](./backend.go).
The inspector defines its own `streamInfo` type and a `capabilities` struct with
plain function fields:

```go
type capabilities struct {
    listStreams func(ctx context.Context) ([]streamInfo, error)
    readAll     func(ctx context.Context, opts eventstore.ReadStreamOptions) (eventstore.StreamIterator, error)
}
```

Each backend entry adapts its store into these fields. The two `listStreams`
closures are textually identical but compile against different concrete types —
that duplication is the per-backend adapter tax, and it grows with each backend
and each capability. `ReadAll` happens to have the same shape on both stores, so
a one-line local interface plus a type assertion suffices — but that convergence
is a convention, not a contract the compiler enforces.

**The cost of promoting these into core interfaces** would be the opposite: a
canonical `StreamMetadata` type and, say, `StreamLister` / `AllReader` interfaces
would make this tool's adapters vanish — but the contract gets bigger, and not
every backend can honestly satisfy it. A store without a global sequence (or one
where listing streams is prohibitively expensive) would either have to lie, error
at runtime, or the interfaces would remain optional — which brings back capability
detection, just spelled `interface{ ... }` assertions against core types instead
of local ones.

Either way, a generic tool must handle absence. What this example demonstrates is
what the optional-capability path looks like **in practice**: nil fields, 501
responses, and a UI that degrades gracefully (no stream list → manual stream-ID
entry; no global feed → no feed tab). Stream-scoped reading needs none of it —
the events table and its pager run entirely on the core `StreamReader`.

Neither answer is presumed here; the code is the exhibit, not the argument.

## How it works

### Stream paging on the core interface

`GET /api/streams/{id}/events` uses `ReadStream` with `ReadStreamOptions`:

- **Forward**: returns events with `StreamVersion > after` (exclusive lower bound).
  The next page passes the last-seen version as `after`.
- **Reverse**: returns events with `StreamVersion <= after` reading backwards;
  `after=0` means "start at the latest event". The next page passes
  `lastSeenVersion - 1`.

Each request asks for `count+1` events; the extra event's presence sets `hasMore`
and is trimmed from the response.

### The global feed pages by global position

For `ReadAll`, both contrib stores reinterpret `AfterVersion` as a **global
position** — the auto-incrementing `id` column of their events table, which is also
each event's `GlobalPosition`. The bounds mirror stream reads (forward: `> after`;
reverse: `<= after`, `0` = newest). Also unlike `ReadStream`, an empty result is an
empty iterator rather than `ErrStreamNotFound` — "no events" is a valid state for
a store-wide feed, and it makes tail polling cheap.

**Live tail is polling, on purpose.** Estoria event stores expose no change
notifications, so the honest generic implementation is a 2-second poll of
`/api/all?after=<highest position seen>`, which returns only news (or nothing).

### Stream IDs and the first underscore

Stream IDs render as `typeid` strings: `type_uuid`, e.g.
`board_e5701a1a-b0a2-4d00-8000-000000000001`. When you type one manually, the
inspector splits on the **first** underscore: UUIDs contain hyphens, never
underscores, and no estoria example uses an underscore in a type name. A type name
containing an underscore would defeat this parse — that assumption is documented at
`parseStreamID` in [`server.go`](./server.go).

## HTTP API

All endpoints are `GET`; there is nothing else.

| Route | Description |
| ----- | ----------- |
| `GET /api/info` | Backend name/label, redacted DSN, capability booleans, `readOnly: true` |
| `GET /api/streams` | All streams (`id`, `type`, `version`), sorted by type then ID — or `501 capability_unavailable` |
| `GET /api/streams/{id}/events?dir=forward\|reverse&after=N&count=50` | One page of a stream's events, with `hasMore` and `nextAfter` |
| `GET /api/all?dir=forward\|reverse&after=N&count=100` | One page of the global feed (paged by global position) — or `501 capability_unavailable` |
| `GET /` | The embedded UI |

Events are returned with `version`, `eventType`, `eventId`, `timestamp`,
`globalPosition` (nullable), `metadata`, and `data` — passed through as raw JSON
when the payload is valid JSON, otherwise base64-encoded with
`"dataEncoding": "base64"` so nothing is ever silently mangled.

## Running

```sh
make run              # go run . (listens on :8085, dsn: ../kanban/kanban.db)
make test             # unit + end-to-end tests over a real SQLite store
DEBUG=1 go run .      # verbose estoria logging
go run . -h           # flags: -addr, -backend, -dsn
```

The inspector never creates or migrates a database. If the SQLite file doesn't
exist, or the expected tables (`event`, `stream` — the contrib default strategy's
schema) are missing, it exits with an error saying exactly that.

## Adding a backend (exercise)

The registry in [`backend.go`](./backend.go) maps a name to a constructor
returning a `StreamReader` plus whatever capabilities the store offers.
Adding MongoDB or KurrentDB from
[estoria-contrib](https://github.com/go-estoria/estoria-contrib) is one entry:

1. Add the contrib dependency and a `connect<Backend>` function: connect, verify
   the store exists (read-only tools don't bootstrap), construct the event store,
   and hold it as an `eventstore.StreamReader`.
2. Fill in whichever `capabilities` fields the store supports, adapting its
   stream-metadata type to `streamInfo`. Leave the rest nil — the UI already
   knows what to do.
3. Register it in the `backends` map with a human-readable label.

If the backend has no global ordering, skip `readAll` and watch the Global feed
tab disappear — that graceful shrinkage is the whole point.
