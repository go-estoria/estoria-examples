# PostgreSQL

A walkthrough of Estoria's core components — aggregates, event stores, and
projections — backed by [PostgreSQL](https://www.postgresql.org) using the
single-table storage strategy from
[estoria-contrib](https://github.com/go-estoria/estoria-contrib), including
the transactional outbox for reliable event delivery to external consumers.

The program creates a bank-account aggregate, appends events, saves and
reloads it, processes the resulting outbox items, and then demonstrates
lower-level event store operations: reading a stream through a projection,
listing streams, and reading all events in global order.

For a complete application built on this stack (outbox-driven CQRS read
models, a web UI), see the [orders](../orders) example.

## Running

```sh
make up      # start PostgreSQL in Docker
go run .     # run the walkthrough; output is printed to the console
make psql    # connect to the database with psql
make down    # stop and remove the container and its volumes
```

Pass an alternate DSN as the first argument, and set `DEBUG=1` to see
Estoria's debug logging.

## See Also

- [Estoria](https://github.com/go-estoria/estoria): Event sourcing toolkit for Go
- [Estoria Contrib](https://github.com/go-estoria/estoria-contrib): Event store implementations for Estoria (among other things)
