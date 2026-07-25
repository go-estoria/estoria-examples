# estoria-examples

Example applications demonstrating the features of Estoria, an event sourcing toolkit for Go.

## Applications

Complete, runnable apps showing what event sourcing with Estoria looks like end to end.

| Example | Description |
| ------- | ----------- |
| [Kanban](./kanban) | A real-time collaborative kanban board with a time-travel slider, live sync via SSE, optimistic concurrency surfaced in the UI, and snapshots — backed by SQLite, no Docker required. |

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
