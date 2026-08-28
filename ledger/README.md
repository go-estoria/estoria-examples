# Ledger — live blue/green read-model rebuilds

**A bank ledger whose read model is rebuilt while the service keeps serving.**
Accounts are event-sourced aggregates in Postgres; the account list is served
from a versioned projection (`account_balances_v1`, `account_balances_v2`, …);
and a rebuild console drives the full projection lifecycle: build a new
version alongside the live one, watch both tables fill in real time, promote,
roll back, and retire — all as durable, arbitrated lifecycle events.

```shell
make up    # start Postgres (host port 5434)
make run   # start the service
open http://localhost:8084
```

## What it demonstrates

| Estoria feature | Where to see it |
| --- | --- |
| Versioned projection identity: each rebuild targets a fresh version with its own table and checkpoint | [`readmodel.go`](readmodel.go) |
| The projection lifecycle orchestrator: `Begin`/`Resume`, `Run`, `Promote`, `Rollback`, `Abandon`, `Retire` | [`server.go`](server.go) |
| Logical cutover: reads consult a `Router` for the live version and compose the table name per query | [`server.go`](server.go) (`handleListAccounts`), [`main.go`](main.go) |
| The cutover worker converging the router on recorded promotions and rollbacks | [`main.go`](main.go) |
| A durable Postgres checkpoint store, with checkpoint recency as the liveness signal | [`checkpoints.go`](checkpoints.go) |
| At-least-once processing made safe by per-row apply-if-newer guards | [`readmodel.go`](readmodel.go) (`balancesHandler`) |
| Steady-state serving handed between the lifecycle run and a plain processor | [`serving.go`](serving.go) |
| Crash recovery: standing runner claims, and audited operator takeover | [`server.go`](server.go) (`handleResumeRebuild`) |
| Graceful shutdown joining the run so its claim release lands | [`main.go`](main.go), [`server.go`](server.go) (`drain`) |
| Governed retirement: durable witness policies and audited overrides | [`server.go`](server.go) (`handleRetire`, `handleSetPolicy`) |

## Try the rebuild flow

1. **Start traffic** (top right) — a generator opens accounts and appends
   deposits and withdrawals continuously, so every rebuild runs against a
   moving ledger.
2. **Start the first rebuild.** No read model exists yet: the build creates
   `account_balances_v1`, catches up to the head, and certifies. **Promote**
   cuts reads over to it; **Retire previous** completes the first rebuild
   (there is nothing to destroy). A steady-state processor takes over
   tailing the live version.
3. **Rebuild again.** Version 2 uses an enriched schema — deposit and
   withdrawal counts and last-activity times, derived from the same event
   history. Watch `account_balances_v2` fill beside the live v1: same
   accounts, same balances, more columns. Reads keep hitting v1 the whole
   time.
4. **Promote**, and the account list flips to v2 mid-traffic. Changed your
   mind? **Roll back** — reads revert to v1, the attempt ends, and v2's
   table is left in place, inert: version numbers are never reused.
5. **Record a retirement policy** witnessed by `router`, rebuild once more,
   promote, and **Retire previous**: the router attests that reads have
   converged before the old version's table is dropped and its checkpoint
   deleted.
6. **Crash it.** Kill the process mid-build (`kill -9`, not Ctrl-C) and
   restart. Resuming is refused — the dead run's claim is still standing —
   until you take it over with an attested actor and reason, which is
   recorded durably in the claim. A graceful stop (Ctrl-C) instead releases
   the claim on the way down, and resuming is transparent.

## How it works

**One store, one arbitration domain.** Domain streams and the projection's
lifecycle stream share the Postgres event store, so a single global sequence
orders everything, and every lifecycle decision — admitting a rebuild,
promoting, rolling back, retiring — is an event appended to the projection's
lifecycle stream under optimistic concurrency. Competing decisions conflict
at that one stream, and exactly one wins.

**Rebuilds are blue/green by construction.** A rebuild never touches the
version serving reads: it builds a fresh table under a fresh version number
with its own checkpoint, and promotion is a recorded cutover, not a data
migration. Rollback is the same flip in reverse, which is why it is instant.

**Reads follow the router.** The account list asks the router which version
is live and queries that table. A cutover worker tails the global sequence
and converges the router on every recorded promotion and rollback — this
process learns about flips the same way any other process would, so running
several instances of this app against one database behaves correctly.

**The lifecycle hands serving back and forth.** While a rebuild runs, the
lifecycle's own processor builds and tails the target version; the steady
state processor keeps tailing the live one. After promotion the rebuild's
processor is the live tail, and when the run winds down, the steady-state
manager takes over. The manager's one rule lives in
[`serving.go`](serving.go): tail the live version, unless the in-flight
attempt targets it.

## Deploying it

The example ships a Dockerfile: a two-stage build ending in distroless static
(about 21 MB, no shell, runs as nonroot). pgx is pure Go, so `CGO_ENABLED=0` is
all it takes. The console's assets are served from disk, so they are copied in
alongside the binary.

```sh
docker build -t estoria-ledger .
docker run -p 8084:8084 -e DATABASE_URL=postgres://... estoria-ledger
```

On Railway, add a service pointed at this repository, set its **Root Directory**
to `/ledger`, and give it a Postgres service to reference. It listens on `$PORT`
and reads its DSN from `$DATABASE_URL` when they are set.

Three flags exist only for public hosting:

| Flag | Effect |
| --- | --- |
| `-reset-on-boot` | drop every table this app owns at startup, so each deploy starts from an empty store |
| `-writes-per-minute N` | per-IP token bucket on state-changing requests; reads are never limited |
| `-trust-proxy` | take the client IP from `X-Forwarded-For` (only behind a proxy that overwrites it) |

`-reset-on-boot` also heals schema drift. A deploy carrying a library upgrade
may expect columns the existing tables don't have, and `CREATE TABLE IF NOT
EXISTS` is a no-op on a table that already exists — so without it the app would
start, serve reads, pass its health check, and fail every write.
