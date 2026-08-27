# Fleet — an IoT dashboard at event-volume scale

A live sensor-fleet dashboard where **every reading is an event**. An in-process
simulator plays a fleet of temperature/humidity sensors, each appending to its own
event stream every few seconds — forever. Watch the streams grow, then run the
**hydration benchmark** to see why long streams need snapshots and caches.

Everything runs from a single Go binary with a local SQLite file. **No Docker, no
external services, no message broker.**

```sh
go run .
# then open http://localhost:8083
```

Click any device card to open its detail drawer, and open **Under the Hood**
(top right) to watch the streams, snapshots, and store stack while the fleet runs.

## What it demonstrates

| Estoria feature | Where to see it |
| --------------- | --------------- |
| Long, ever-growing streams (one aggregate per device) | [`device.go`](./device.go), [`simulator.go`](./simulator.go) — a `ReadingRecorded` every 1.5–4s per device |
| The full aggregate store decorator stack (`Store[E]` composition) | [`main.go`](./main.go) — `EventSourcedStore` → `SnapshottingStore` → `CachedStore` → an app-local broadcasting decorator |
| Snapshots with `EventCountSnapshotPolicy` | Every 200 events per device (`-snapshot-every`); the 📸 toast announces each one |
| Snapshots stored *as events* (`snapshotstore/eventstream`) | The `devicesnapshot_…` streams in the Under the Hood panel — same SQLite file, no extra storage |
| Aggregate caching (`CachedStore` + `estoria-contrib` bigcache) | [`main.go`](./main.go) — hot loads skip storage entirely; `POST /api/devices/{id}/evict` proves it |
| The hydration benchmark: cold replay vs snapshot vs cache | `handleBenchmark` in [`server.go`](./server.go); the bar chart in the detail drawer |
| Writing your own store decorator (powers the live dashboard) | [`main.go`](./main.go) — `broadcastingStore` pushes every saved change over SSE |
| Derived state with bounded memory (a ring buffer in the entity) | [`device_events.go`](./device_events.go) — the entity keeps the last 60 readings; the stream keeps them all |
| Deriving a registry from `ListStreams` (no device table) | [`main.go`](./main.go) — every stream of type `device` *is* a device |
| SQLite event store (`estoria-contrib`, pure Go) | [`main.go`](./main.go) — single-table strategy, WAL mode |
| Value-typed event prototypes, `typeid`, typed errors | Throughout |
| Testing event-sourced domains (no mocks) | [`device_test.go`](./device_test.go) — pure transitions + snapshot round trip over the in-memory event store |

## How it works

### Why long streams need snapshots

Most event-sourced examples have short streams: a board with 50 changes, an order
with 10. A sensor is different — its stream *only grows*. At one reading every
~2.5 seconds, a device accumulates ~35,000 events a day. Replaying all of them to
answer "what's the temperature right now?" gets slower every single day. That is
the problem the decorator stack solves, and each layer removes another chunk of
work from the read path:

- **`EventSourcedStore`** replays the entire stream. Correct, and O(stream length).
- **`SnapshottingStore`** writes a snapshot every N events (`-snapshot-every`, default
  200) and hydrates from the latest one, replaying only the ≤ N events after it.
  Loads become O(N) regardless of stream length.
- **`CachedStore`** keeps hydrated aggregates in an in-process
  [bigcache](https://github.com/allegro/bigcache). Every save re-populates the
  cache, and the simulator saves constantly, so serving reads touch no storage
  at all. Loads become O(1).
- **`broadcastingStore`** (app-local, ~15 lines) broadcasts every saved change
  over SSE, which is how the dashboard stays live without polling.

All four implement the same four-method `aggregatestore.Store[Device]` interface,
so the stack is just constructor nesting in [`main.go`](./main.go).

### The simulator is an ordinary client

There is no privileged write path. Each device goroutine does exactly what a real
ingest endpoint would: load the aggregate through the full stack (a cache hit),
append `ReadingRecorded` / `StatusChanged` / `AlertRaised` / … events, and save.
The domain is a pure recorder — the rule that raises an `overheat` alert above
30°C (and clears it below 28°C) lives in the simulator, while the aggregate only
enforces consistency (no duplicate alerts, no unknown statuses).

### The benchmark methodology

`GET /api/devices/{id}/benchmark` loads the *same device* three times,
sequentially, timing each hydration:

1. **Cold replay** — through the plain `EventSourcedStore`: every event since
   version 1.
2. **From snapshot** — through the `SnapshottingStore` (no cache): the latest
   snapshot plus the events after it.
3. **From cache** — through the full stack: straight out of bigcache.

The loads run against the live database while the simulator writes, so treat the
numbers as an illustration, not a controlled microbenchmark — the *ratios* are
the story, and they widen as streams grow. On a young database (fewer events than
`-snapshot-every`) there is no snapshot yet, and cold ≈ snapshot is the expected
honest result. Use **Evict from cache** to watch the cached load fall through to
the snapshot path once, then recover.

### One caveat worth knowing

The event-stream snapshot store always returns the **latest** snapshot — its
reader ignores version bounds. That is fine for latest-state loads, but a
version-pinned load (`LoadOptions.ToVersion`) through the snapshotting store
could hydrate *past* the requested version and fail. This app therefore keeps the
undecorated `EventSourcedStore` around (the `history` field in
[`server.go`](./server.go)) — it is both the benchmark's cold path and the store
any time-travel feature would have to use. The [kanban example](../kanban) does
exactly that for its timeline slider.

## HTTP API

| Route | Description |
| ----- | ----------- |
| `GET /api/fleet` | Every device at its latest version |
| `GET /api/devices/{id}` | One device, plus its latest snapshot version |
| `GET /api/devices/{id}/benchmark` | Three timed hydrations: `{eventCount, snapshotVersion, coldMicros, snapshotMicros, cachedMicros}` |
| `POST /api/devices/{id}/evict` | Remove the device from the aggregate cache |
| `POST /api/sim/start`, `POST /api/sim/stop` | Control the simulator |
| `GET /api/stats` | Streams, event totals, events/sec, sim status, store stack |
| `GET /api/watch` | Server-sent events: every saved device change, pushed live |

## Running

```sh
make run              # go run . (listens on :8083, db: fleet.db)
make test             # domain tests, race detector on
make clean            # remove the database and start a fresh fleet
DEBUG=1 go run .      # verbose estoria logging (watch hydration and snapshots)
go run . -h           # flags: -addr, -db, -devices, -snapshot-every
```

## Things to try

- `go run . -snapshot-every 20` — snapshots land every few minutes per device;
  watch the 📸 toasts and the `devicesnapshot_…` streams climb in Under the Hood.
- `DEBUG=1 go run .` then run a benchmark: the log shows the cold load replaying
  the whole stream while the snapshot load starts from the latest snapshot.
- `go run . -devices 50` — crank the fleet and watch events/sec climb. The grid,
  SSE fan-out, and cache shrug it off.
- Leave it running overnight, then benchmark: a cold replay of tens of thousands
  of events versus a sub-millisecond cache hit makes the point better than any
  README.
- Pause the simulator, wait ten minutes (the cache TTL), and benchmark: the
  "cached" load repopulates the expired entry, and a second run hits it again.
- Extend the domain: add a `humidity-low` alert rule in the simulator, or a
  `DeviceDecommissioned` event (decide what the registry should do with it!).
  Old events never need migrating.
- Swap SQLite for Postgres or MongoDB from
  [estoria-contrib](https://github.com/go-estoria/estoria-contrib) — only the
  event store construction in `main.go` changes.
