# Kanban — a collaborative, time-traveling board

A real-time collaborative kanban board where **every change is an event**. Drag cards
between columns, watch other browser tabs update live, then scrub the timeline to
watch the board rebuild itself at any point in its history.

Everything runs from a single Go binary with a local SQLite file. **No Docker, no
external services.**

```sh
go run .
# then open http://localhost:8080
```

Open the app in two browser windows side by side to see live collaboration, and open
**Under the Hood** (top right) to watch the event streams, snapshots, and store stack
while you work.

## What it demonstrates

| Estoria feature | Where to see it |
| --------------- | --------------- |
| Aggregate modeling with pure `ApplyTo` transitions | [`board.go`](./board.go), [`board_events.go`](./board_events.go) |
| The aggregate store decorator stack (`Store[E]` composition) | [`main.go`](./main.go) — `EventSourcedStore` → `SnapshottingStore` → `HookableStore` |
| Lifecycle hooks (`AfterSave` powers the live sync) | [`main.go`](./main.go) — the hook broadcasts every saved change over SSE |
| Time travel with `LoadOptions.ToVersion` | `GET /api/board?version=N` in [`server.go`](./server.go); the timeline slider in the UI |
| Optimistic concurrency (`ExpectVersion` → `StreamVersionMismatchError`) | `runCommand` in [`server.go`](./server.go) maps conflicts to HTTP 409; the ⚡ button triggers one on demand |
| Snapshots with `EventCountSnapshotPolicy` | Every 10 events; the Under the Hood panel announces each one |
| Snapshots stored *as events* (`snapshotstore/eventstream`) | The `boardsnapshot_…` stream in the Under the Hood panel — same SQLite file, no extra storage |
| Stream projections (`eventstore/projection`) | The activity feed: `handleActivity` in [`server.go`](./server.go) replays the stream into human-readable history |
| SQLite event store (`estoria-contrib`, pure Go) | [`main.go`](./main.go) — single-table strategy, WAL mode |
| Value-typed event prototypes, `typeid`, typed errors | Throughout |
| Testing event-sourced domains (no mocks) | [`board_test.go`](./board_test.go) — pure transitions + a round trip against the in-memory event store |

## How it works

### One stream, one aggregate

The whole board is a single `Board` aggregate whose stream lives in SQLite. Every
drag, edit, and rename appends one event (`CardMoved`, `CardEdited`, …). Current
state is derived by replaying events through each event's `ApplyTo` — the server
never mutates stored state.

### The write path

Every command follows the same route (`runCommand` in [`server.go`](./server.go)):

1. The client sends the command with `baseVersion` — the board version it last saw.
2. The server loads the board **at that version** (`LoadOptions.ToVersion`) and
   validates the command against it.
3. It appends the resulting event and saves. Estoria saves with
   `ExpectVersion: baseVersion`, so if any other write landed in between, the event
   store rejects the append with a `StreamVersionMismatchError`.
4. The server surfaces that as **HTTP 409**; the client refreshes and retries once.

This is real end-to-end optimistic concurrency — there is no app-level version
check standing in for the storage-level one.

### Two views over the same stream

The server composes two aggregate stores over one event store:

- **live** (`HookableStore` → `SnapshottingStore` → `EventSourcedStore`): serves
  latest-state reads and all writes. Loads start from the most recent snapshot;
  saves trigger the snapshot policy and the `AfterSave` broadcast hook.
- **history** (`EventSourcedStore` only): serves version-pinned loads for the
  timeline slider. Time travel bypasses the snapshotting decorator because the
  event-stream snapshot store always returns the *latest* snapshot, which may be
  newer than the version being requested.

Both implement the same four-method `aggregatestore.Store[Board]` interface —
composing them differently per use case is the point.

### Snapshots are just events

The `SnapshottingStore` writes a snapshot every 10 events using
`snapshotstore/eventstream`, which appends snapshots to a parallel
`boardsnapshot_<id>` stream in the same event store. Open **Under the Hood** and
make a few changes: you'll see the snapshot stream's version tick up, and loads
begin replaying only the events after the latest snapshot.

## HTTP API

| Route | Description |
| ----- | ----------- |
| `GET /api/board` | Latest board state and version |
| `GET /api/board?version=N` | The board as it was at version N |
| `POST /api/board/rename` | Rename the board |
| `POST /api/columns`, `POST /api/columns/{id}/rename` | Add / rename a column |
| `POST /api/cards`, `.../edit`, `.../move`, `.../delete` | Card commands |
| `GET /api/activity` | The event stream projected into readable history |
| `GET /api/stats` | Streams, snapshot info, and the store stack |
| `GET /api/watch` | Server-sent events: every saved change, pushed live |

All commands take a JSON body with `baseVersion` plus command-specific fields, and
return `200 {"version": N}`, `409` on a version conflict, or `422` on validation
failure.

## Running

```sh
make run              # go run . (listens on :8080, db: kanban.db)
make test             # domain tests, race detector on
make clean            # remove the database and start fresh
DEBUG=1 go run .      # verbose estoria logging (watch hydration and snapshots)
go run . -h           # flags: -addr, -db, -snapshot-every
```

## Deploying it

The example ships a Dockerfile: a two-stage build ending in distroless static
(about 20 MB, no shell, runs as nonroot). The SQLite driver is pure Go, so
`CGO_ENABLED=0` is all it takes.

```sh
docker build -t estoria-kanban .
docker run -p 8080:8080 estoria-kanban
```

On Railway, add a service pointed at this repository and set its **Root
Directory** to `/kanban` — without it the build runs from the repository root,
which has no module and no Dockerfile. `railway.toml` in this folder covers
the rest (builder, health check, restart policy).

It listens on `$PORT` when the platform sets one, so it drops straight onto
Railway, Fly, or Cloud Run. The database lives at `/data` — mount a volume there
to keep the board across deploys, or don't, and every deploy starts fresh.

Four flags exist only for public hosting, and are off unless passed:

| Flag | Effect |
| --- | --- |
| `-hourly-reset` | clears and reseeds the board at the top of every hour |
| `-writes-per-minute N` | per-IP token bucket on state-changing requests; reads are never limited |
| `-trust-proxy` | take the client IP from `X-Forwarded-For` (only behind a proxy that overwrites it) |
| `-max-clients N` | cap concurrent SSE connections |

The image's default command turns all four on. Running the example locally with
`go run .` turns none of them on.

## Things to try

- Set `-snapshot-every 3` and watch snapshots fly in the Under the Hood panel.
- `DEBUG=1 go run .` then reload the page: the log shows the board hydrating from
  the latest snapshot instead of replaying from version 1.
- Kill the server mid-session and restart it — the board comes back byte-for-byte,
  because the events *are* the database.
- Extend the domain: add a `ColumnRemoved` event (decide what happens to its
  cards!), or per-card comments. Notice that old events never need migrating.
- Swap SQLite for Postgres or MongoDB from
  [estoria-contrib](https://github.com/go-estoria/estoria-contrib) — only the
  event store construction in `main.go` changes.
