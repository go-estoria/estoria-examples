# estoria-examples

Example applications demonstrating the features of Estoria, an event sourcing toolkit for Go.

## Applications

Complete, runnable apps showing what event sourcing with Estoria looks like end to end.

| Example | Description |
| ------- | ----------- |
| [Kanban](./kanban) | A real-time collaborative kanban board with a time-travel slider, live sync via SSE, optimistic concurrency surfaced in the UI, and snapshots — backed by SQLite, no Docker required. |
| [Orders](./orders) | An order-fulfillment service on Postgres: a strict state-machine domain, the transactional outbox delivering events to a CQRS read model and webhook log, and a live admin dashboard. |
| [Fleet](./fleet) | An IoT sensor-fleet dashboard with an in-process device simulator: long streams, the full snapshotting + caching decorator stack, and a live hydration benchmark. SQLite, no Docker. |
| [Chess](./chess) | Live two-player chess where each game is an event stream: move legality inside ApplyTo, a replay slider via ToVersion, optimistic concurrency as turn-race protection, and PGN export. SQLite, no Docker. |

## Backend quickstarts

The same walkthrough of Estoria's core components (aggregates, event stores, projections),
each wired to a different storage backend. Each includes a `docker-compose.yml` and Makefile
for spinning up its dependencies.

| Example | Description |
| ------- | ----------- |
| [PostgreSQL](./postgres) | Postgres event store, including the transactional outbox for reliable event delivery to external consumers. |
| [MongoDB](./mongodb) | MongoDB event store using a single-collection strategy. |
| [KurrentDB](./kurrent) | KurrentDB (formerly EventStoreDB) event store with native stream mapping. |
| [OpenTelemetry](./opentelemetry) | Instrumenting Estoria's event, aggregate, and snapshot stores with OTEL tracing and metrics (Jaeger, Prometheus, Grafana). |

## See Also

- [Estoria](https://github.com/go-estoria/estoria): Event sourcing toolkit for Go
- [Estoria Contrib](https://github.com/go-estoria/estoria-contrib): Event store implementations for Estoria (among other things)
