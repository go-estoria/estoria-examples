# Chess — live two-player chess with full game replay

A chess game **is** an event stream: a starting position and a sequence of moves.
Replay is not a feature we added on top — it's what event sourcing does. Play a
game live across two browser tabs, then scrub the timeline to watch any position
in the game rebuild itself, even while your opponent keeps playing.

Everything runs from a single Go binary with a local SQLite file. **No Docker, no
external services.**

```sh
go run .
# then open http://localhost:8084
```

Create a game, then open it in a second browser tab to play both sides — every
move is pushed to all tabs over SSE the moment it is saved.

## What it demonstrates

| Estoria feature | Where to see it |
| --------------- | --------------- |
| Aggregate modeling with pure `ApplyTo` transitions | [`game.go`](./game.go), [`game_events.go`](./game_events.go) |
| Domain rules enforced in the events themselves | `MoveMade.ApplyTo` rejects illegal moves — chess legality lives in the domain, not in HTTP handlers |
| One aggregate per game (many short streams, one store) | [`main.go`](./main.go); the lobby lists them via `ListStreams` |
| Lifecycle hooks (`AfterSave` powers live play) | [`main.go`](./main.go) — the hook broadcasts every saved move over SSE |
| Time travel with `LoadOptions.ToVersion` | `GET /api/games/{id}?version=N` in [`server.go`](./server.go); the replay slider in the UI |
| Optimistic concurrency (`ExpectVersion` → `StreamVersionMismatchError`) | `runCommand` in [`server.go`](./server.go) maps conflicts to HTTP 409 — turn-race protection for free |
| Deriving artifacts from the stream (SAN move lists, PGN export) | `sanHistory` in [`game.go`](./game.go), `handlePGN` in [`server.go`](./server.go) |
| SQLite event store (`estoria-contrib`, pure Go) | [`main.go`](./main.go) — single-table strategy, WAL mode |
| Value-typed event prototypes, `typeid`, typed errors | Throughout |
| Testing event-sourced domains (no mocks) | [`game_test.go`](./game_test.go) — scholar's mate as a pure event sequence, plus a round trip against the in-memory event store |

## How it works

### The game is the stream

Each game is a `Game` aggregate with its own event stream: one `GameCreated`,
then one `MoveMade` per move (and possibly a `PlayerResigned`). The entity keeps
only plain, serializable state — players, the UCI move list, and derived fields
(FEN, turn, outcome). Whenever an event is applied, the aggregate rebuilds the
rules-engine position by replaying its moves through
[notnil/chess](https://github.com/notnil/chess). Rebuilding from scratch is O(n)
per event, and that's fine: chess streams are short, and it keeps the entity a
pure value.

### Legality lives in `ApplyTo`

`MoveMade.ApplyTo` decodes the move against the current position and asks the
rules engine to apply it. An illegal move — or any move after checkmate — returns
an error and leaves the state untouched. The HTTP layer never inspects a chess
rule; it pre-flights the event through `ApplyTo` against the loaded state and
maps a rejection to **HTTP 422**. (The pre-flight matters: estoria applies events
on save, *after* appending them, so an event the domain would reject must never
reach the stream.)

### The write path

Every command follows the same route (`runCommand` in [`server.go`](./server.go)):

1. The client sends the command with `baseVersion` — the game version it last saw.
2. The server loads the game **at that version** (`LoadOptions.ToVersion`),
   derives the event, and pre-flights it through `ApplyTo`.
3. It appends the event and saves. Estoria saves with
   `ExpectVersion: baseVersion`, so if the other player's move landed first, the
   event store rejects the append with a `StreamVersionMismatchError`.
4. The server surfaces that as **HTTP 409** — "the position changed under you."

In a two-player game, optimistic concurrency is not an edge case to paper over:
it *is* the turn discipline. Two tabs racing to move can never corrupt a game.

### Replay is just a load

The replay slider fetches `GET /api/games/{id}?version=N`, which hydrates the
aggregate with `LoadOptions.ToVersion: N` — replaying only the first N events.
Version 1 is the freshly created game; version k is the position after k−1
moves. There is no snapshot store here, deliberately: games are short streams,
replaying them is instant, and a snapshotting decorator must never be combined
with version-pinned loads anyway (a snapshot always reflects the *latest* state,
which may be newer than the version requested). Plain `EventSourcedStore` +
`HookableStore` is the whole stack.

### The lobby is a fold over `ListStreams`

`GET /api/games` lists every `game` stream in the event store and loads each
aggregate for its summary. At demo scale that's perfect; a production lobby
would maintain a read model updated by a projection instead of loading every
aggregate per request — a good exercise (see *Things to try*).

## HTTP API

| Route | Description |
| ----- | ----------- |
| `GET /api/games` | Lobby: a summary of every game |
| `POST /api/games` | Create a game (`{"white": "...", "black": "..."}`, names optional) |
| `GET /api/games/{id}` | Full game state, SAN move list, and version |
| `GET /api/games/{id}?version=N` | The game as it was at version N |
| `GET /api/games/{id}/legal-moves` | Legal moves for the live position, grouped by origin square |
| `POST /api/games/{id}/move` | Make a move: `{"baseVersion": N, "uci": "e2e4"}` |
| `POST /api/games/{id}/resign` | Resign: `{"baseVersion": N, "color": "white"}` |
| `GET /api/games/{id}/pgn` | Download the game as PGN |
| `GET /api/watch` | Server-sent events: every saved move, tagged with its `gameId` |

Commands return `200 {"version": N}`, `409` on a version conflict, or `422` when
the domain rejects the event (illegal move, game over, ...).

## Running

```sh
make run              # go run . (listens on :8084, db: chess.db)
make test             # domain tests, race detector on
make clean            # delete the database (all games are lost)
DEBUG=1 go run .      # verbose estoria logging (watch every hydration)
go run . -h           # flags: -addr, -db
```

## Deploying it

The example ships a Dockerfile: a two-stage build ending in distroless static
(about 20 MB, no shell, runs as nonroot). The SQLite driver is pure Go, so
`CGO_ENABLED=0` is all it takes.

```sh
docker build -t estoria-chess .
docker run -p 8084:8084 estoria-chess
```

It listens on `$PORT` when the platform sets one, so it drops straight onto
Railway, Fly, or Cloud Run. The database lives at `/data` — mount a volume there
to keep games across deploys, or don't, and every deploy starts fresh.

Four flags exist only for public hosting, and are off unless passed:

| Flag | Effect |
| --- | --- |
| `-hourly-reset` | deletes every game at the top of every hour |
| `-writes-per-minute N` | per-IP token bucket on state-changing requests; reads are never limited |
| `-trust-proxy` | take the client IP from `X-Forwarded-For` (only behind a proxy that overwrites it) |
| `-max-clients N` | cap concurrent SSE connections |

The image's default command turns all four on. Running the example locally with
`go run .` turns none of them on.

## Things to try

- Open a game in two tabs and play both sides. Then try to move for the same
  side from both tabs at once — the loser gets a 409, because the event store
  rejected the stale append.
- Scrub the replay slider mid-game while "your opponent" (the other tab) keeps
  playing: the slider's range grows live as new moves land in the stream.
- Play scholar's mate (1.e4 e5 2.Bc4 Nc6 3.Qh5 Nf6 4.Qxf7#) and watch the
  status flip to "Checkmate — White wins" — the outcome is derived state, not a
  stored flag.
- Download the PGN and paste it into [lichess.org/paste](https://lichess.org/paste)
  — the whole game, reconstructed from an event stream.
- Inspect the raw stream: `sqlite3 chess.db 'select stream_id, stream_offset,
  event_type, data from event'` — one row per half-move, nothing else.
- Build the read-model exercise: project every stream into a `lobby` table on
  save, and make `GET /api/games` read from it instead of loading each aggregate.
- Swap SQLite for Postgres or MongoDB from
  [estoria-contrib](https://github.com/go-estoria/estoria-contrib) — only the
  event store construction in `main.go` changes.
