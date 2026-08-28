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
| Optional capabilities: `GlobalReader` from core, `ListStreams` adapted per backend | [`backend.go`](./backend.go) — and the design discussion below |
| Graceful degradation when a capability is missing | 501 responses in [`server.go`](./server.go); the UI falls back to manual stream-ID entry |
| Global ordering via `Event.GlobalPosition` | The Global feed tab, and the tail cursor it polls with |
| Snapshots are just events in a parallel stream | Streams whose type ends in `snapshot` get a 📸 chip |
| One tool, many stores (SQLite and Postgres contrib stores) | The backend registry in [`backend.go`](./backend.go) |

## The design question, and how it was answered

This example was built to make an open interface-design question concrete, and
it has since been settled — differently for each of the two capabilities, which
is what makes the code worth reading now.

Estoria's core `eventstore.Store` interface is deliberately tiny:

```go
type Store interface {
    StreamReader // ReadStream
    StreamWriter // AppendStream
}
```

Two operations this tool wants — **listing the streams in a store** and
**reading all events in global order** — were not part of that contract. Both
were available on the SQLite and Postgres contrib stores, but only as concrete
methods, so a generic tool had to adapt each backend by hand.

**Global reads were promoted to core**, as `eventstore.GlobalReader`: optional,
discovered by type assertion, and carrying a genuinely demanding contract —
positions form a *stable prefix* (once position P is yielded, nothing new may
ever commit at or below P), and a read observes a finite frontier. That contract
is what makes a position a resumable checkpoint instead of a hint. Discovery is
now one assertion against a core type, and the method is used unmodified:

```go
if reader, ok := any(store).(eventstore.GlobalReader); ok {
    caps.readAll = reader.ReadAll
}
```

The promotion came with a constraint: **global reads are forward-only**. There
is no direction in `ReadAllOptions`, because the stable-prefix guarantee is only
defined going forward. That is why the feed here opens on `/api/all/tail`, which
scans forward and keeps the last page, rather than reading backwards from the
end — see [`server.go`](./server.go), where the cost is spelled out.

**`ListStreams` was not promoted**, and the adapter tax it charges is still
visible in [`backend.go`](./backend.go):

```go
type capabilities struct {
    listStreams func(ctx context.Context) ([]streamInfo, error)   // adapted per backend
    readAll     func(ctx context.Context, opts eventstore.ReadAllOptions) (eventstore.StreamIterator, error)
}
```

Each backend's `ListStreams` returns *its own* strategy package's
`StreamMetadata`, so this tool defines a local `streamInfo` and a closure per
backend to convert into it. Those two closures are textually identical and
compile against different concrete types. That is the cost of leaving a
capability out of core, sitting right next to the one that was promoted.

So the answer was neither "put everything in core" nor "keep core minimal at all
costs". It was: promote a capability when you can state a contract strong enough
to program against — and `GlobalReader`'s stable-prefix rule is exactly such a
statement — and leave it out when you cannot. Either way a generic tool must
still handle absence, which is what the nil fields, the 501 responses, and the
degrading UI here demonstrate. Stream-scoped reading needs none of it: the
events table and its pager run entirely on the core `StreamReader`.

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

### The global feed pages forward by global position

`ReadAll` takes `ReadAllOptions`, whose `AfterPosition` is an exclusive lower
bound on `GlobalPosition` — a dedicated type, not stream options reinterpreted.
There is **no direction**: global reads are forward-only, because the
stable-prefix guarantee that makes a position resumable is only defined going
forward.

That has a consequence for a browser like this one. The newest events cannot be
fetched by reading backwards from the end, so `GET /api/all/tail` scans forward
and keeps the last page — O(events) in time, O(count) in memory. The feed calls
it once on open, then tails forward from the position it returns.

Unlike `ReadStream`, an empty result is an empty iterator rather than
`ErrStreamNotFound`: "no events" is a valid state for a store-wide feed, and it
is what makes tail polling cheap.

**Live tail is polling, on purpose.** Estoria event stores expose no change
notifications, so the honest generic implementation is a 2-second poll of
`/api/all?after=<highest position seen>`, which returns only news (or nothing).
The stable-prefix contract is what makes that safe: nothing can later commit at
or below a position already yielded, so polling forward cannot skip an event.

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
| `GET /api/all?after=N&count=100` | One page forward through the global feed, by global position — or `501 capability_unavailable` |
| `GET /api/all/tail?count=100` | The newest events plus the frontier position, for opening the feed — or `501 capability_unavailable` |
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

## Deploying it

The example ships a Dockerfile: a two-stage build ending in distroless static
(about 26 MB, no shell, runs as nonroot). Both drivers are pure Go, so
`CGO_ENABLED=0` is all it takes.

```sh
docker build -t estoria-inspector .
docker run -p 8085:8085 -e DATABASE_URL=postgres://... estoria-inspector
```

On Railway, add a service pointed at this repository and set its **Root
Directory** to `/inspector`. It listens on `$PORT` and reads its store from
`$DATABASE_URL` when they are set, so a hosted inspector browses whichever
service's database it is given — reference another service's `DATABASE_URL` and
nothing else changes.

Two flags exist only for public hosting:

| Flag | Effect |
| --- | --- |
| `-reads-per-minute N` | per-IP token bucket on `/api/all/tail`, the one endpoint that scans the whole store |
| `-trust-proxy` | take the client IP from `X-Forwarded-For` (only behind a proxy that overwrites it) |

There is no reset and no write limiting, because this tool never writes: the
store is held as an `eventstore.StreamReader` and `AppendStream` is never
called. What needs bounding is read cost. Stream lists and paged reads are
bounded work and stay unmetered — throttling them would only make a public
instance feel broken.

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
