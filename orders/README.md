# Orders — CQRS and the transactional outbox

An order-fulfillment service where **every order is an event stream**. Place orders,
walk them through the pipeline (placed → paid → picked → shipped → delivered), and
watch the outbox monitor deliver each event into a Postgres read model in real time.

The write side appends events; the read side is a plain SQL table that only the
outbox processor may touch. The gap between them — usually invisible — is the
whole point of this example.

```sh
make up     # start Postgres (Docker, host port 5433)
make run    # then open http://localhost:8082
```

Click **+ New order**, then drive it through the pipeline from the detail drawer.
Keep an eye on the **Outbox monitor**: every command you issue shows up moments
later as a webhook delivery, and only then does the order list change.

## What it demonstrates

| Estoria feature | Where to see it |
| --------------- | --------------- |
| Aggregate modeling with a pure `ApplyTo` state machine | [`order.go`](./order.go), [`order_events.go`](./order_events.go) — invalid transitions are rejected in the events themselves |
| Postgres event store (`estoria-contrib`) | [`main.go`](./main.go) — default single-table strategy, schema applied at startup |
| **Transactional outbox** (`postgres/outbox`) | [`main.go`](./main.go) — registered via `WithAppendTransactionHooks`, so events and outbox rows commit atomically |
| **CQRS read model** projected by the outbox | [`readmodel.go`](./readmodel.go) — the `order_summaries` table; the outbox handler is its only writer |
| Eventual consistency, made visible | The outbox monitor panel; the list updates a beat after each command |
| Lifecycle hooks (`AfterSave` powers the live sync) | [`main.go`](./main.go) — saved commands broadcast over SSE |
| Optimistic concurrency (`ExpectVersion` → `StreamVersionMismatchError`) | `runCommand` in [`server.go`](./server.go) maps conflicts to HTTP 409 |
| Raw stream reads + projections (`eventstore/projection`) | The event timeline in the detail drawer: `orderTimeline` in [`server.go`](./server.go) |
| One stream per aggregate instance | Each order is its own `order_<uuid>` stream (contrast with kanban's single shared stream) |
| Value-typed event prototypes, `typeid`, typed errors | Throughout |
| Testing event-sourced domains (no mocks) | [`order_test.go`](./order_test.go) — the transition matrix + a round trip against the in-memory event store |

There is no snapshotting layer here: order streams top out at seven events, so
replaying from scratch is already optimal. Snapshots are the
[kanban example](../kanban)'s demo.

## How it works

### One stream per order

Each order is an `Order` aggregate with its own event stream in Postgres. Commands
append one event each (`OrderPlaced`, `OrderPaid`, …), and every `ApplyTo` enforces
the fulfillment state machine: you cannot ship before picking, cannot pay a cancelled
order, and cannot cancel once the package is on a truck. State is derived by
replaying events — the server never mutates stored state.

### The write path

Every fulfillment command follows the same route (`runCommand` in
[`server.go`](./server.go)):

1. The client sends the command with `baseVersion` — the order version it last saw.
2. The server loads the order **at that version** (`LoadOptions.ToVersion`) and
   validates the command against it (invalid transitions → **HTTP 422**).
3. It appends the resulting event and saves. Estoria saves with
   `ExpectVersion: baseVersion`, so if any other write landed in between, the event
   store rejects the append with a `StreamVersionMismatchError`.
4. The server surfaces that as **HTTP 409**; the client refreshes and retries once.

### The outbox flow

This is the showcase. The outbox is registered as an **append transaction hook** on
the Postgres event store, which means every save executes as:

```
BEGIN;
  INSERT events            -- the source of truth
  INSERT outbox rows       -- one per event, same transaction
COMMIT;
```

Because both writes share one transaction, the two failure modes of "save, then
publish" are impossible by construction:

- **No lost deliveries** — an event cannot be committed without its outbox row;
  there is no window where a crash leaves an event nobody will ever act on.
- **No phantom deliveries** — an outbox row cannot be committed without its event;
  a rolled-back save can never notify anyone about something that didn't happen.

A background processor (`outbox.Run`) polls the outbox table and hands each
undelivered item to a handler, in **strict per-stream FIFO order**, at least once.
This app's handler plays the role of a webhook dispatcher: it projects the event
into the `order_summaries` read model, records the delivery in an in-memory log,
and notifies SSE clients. Because per-stream FIFO is guaranteed, the handler can
assume an order's `OrderPlaced` row always exists before any status update for it
arrives — and because delivery is at-least-once, its writes are idempotent upserts.

### The read side (CQRS)

`GET /api/orders` never loads aggregates. It SELECTs from `order_summaries` — a
table maintained *exclusively* by the outbox handler. Listing 100 orders is one
query, not 100 stream replays, and the table can be indexed, sorted, and aggregated
like any other SQL.

The price is a moment of lag: a command responds before the outbox processor has
delivered its event, so the list may briefly trail reality. The UI doesn't hide
this — the pending counter ticks up, the delivery lands in the feed, and *then*
the list updates. That's eventual consistency, honestly surfaced (and with the
250ms poll interval, nearly instant).

The detail drawer takes the other path on purpose: it loads the aggregate itself
(always current, with its version for optimistic concurrency) plus a raw read of
its event stream rendered as a timeline.

## HTTP API

| Route | Description |
| ----- | ----------- |
| `GET /api/orders` | Order list + status counts, **from the read model** |
| `POST /api/orders` | Place a demo order (random customer and catalog items) |
| `GET /api/orders/{id}` | Full aggregate detail, version, and event timeline |
| `POST /api/orders/{id}/pay` | Pay a placed order |
| `POST /api/orders/{id}/pick` | Pick a paid order |
| `POST /api/orders/{id}/ship` | Ship a picked order (fake carrier + tracking) |
| `POST /api/orders/{id}/deliver` | Deliver a shipped order |
| `POST /api/orders/{id}/cancel` | Cancel any order that hasn't shipped |
| `GET /api/outbox` | Pending delivery count + recent webhook log |
| `GET /api/watch` | Server-sent events: saved commands and outbox deliveries |

All commands take a JSON body with `baseVersion` (cancel also accepts `reason`),
and return `200 {"version": N}`, `409` on a version conflict, or `422` on an
invalid state transition.

## Running

```sh
make up               # docker compose up (postgres:17-alpine on host port 5433)
make run              # go run . (listens on :8082)
make test             # domain tests, race detector on (no Docker needed)
make psql             # poke at the tables yourself
make down             # stop Postgres and delete its volume
DEBUG=1 go run .      # verbose estoria logging (watch appends and outbox polls)
go run . -h           # flags: -addr, -dsn
```

## Things to try

- Open the app in two tabs and race them: pay the same order from both. One tab
  wins; the other gets a 409, refreshes, retries — and then a 422, because you
  can't pay an order that's already paid. Both toasts tell the story.
- `make psql`, then `SELECT * FROM outbox ORDER BY id DESC LIMIT 5;` — the same
  rows the monitor shows, with `processed_at` stamped by the processor. The
  `event` table next to it holds the source-of-truth streams.
- Stop the app (not Postgres), issue no commands, and restart it: the read model
  is exactly where it was, because it only ever advances through committed
  outbox rows.
- Delete the read model — `TRUNCATE order_summaries` — and note the app keeps
  serving details and timelines from the streams. (Rebuilding a read model from
  the event history is the classic next exercise: try wiring `ReadAll` up to
  `readModel.apply`.)
- Extend the pipeline: add an `OrderRefunded` event for delivered orders. The
  state machine, read model, and timeline each need one small, obvious change —
  and no stored data migrates.
