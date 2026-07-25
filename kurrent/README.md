# KurrentDB

A walkthrough of Estoria's core components — aggregates, event stores, and
projections — backed by [KurrentDB](https://www.kurrent.io) (formerly
EventStoreDB), a purpose-built event store where Estoria streams map 1:1 to
native streams.

The program creates a bank-account aggregate, appends events, saves and
reloads it, and then demonstrates lower-level event store operations: reading
a stream through a projection, listing streams, and reading all events in
global order.

## Running

```sh
make up       # start KurrentDB in Docker (insecure mode, for local use only)
go run .      # run the walkthrough; output is printed to the console
make kurrent  # open the KurrentDB web UI (http://localhost:2113)
make down     # stop and remove the container
```

Pass an alternate connection string as the first argument, and set `DEBUG=1`
to see Estoria's debug logging.

## See Also

- [Estoria](https://github.com/go-estoria/estoria): Event sourcing toolkit for Go
- [Estoria Contrib](https://github.com/go-estoria/estoria-contrib): Event store implementations for Estoria (among other things)
