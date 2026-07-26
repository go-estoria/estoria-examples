# MongoDB

A walkthrough of Estoria's core components — aggregates, event stores, and
projections — backed by [MongoDB](https://www.mongodb.com) using the
single-collection storage strategy from
[estoria-contrib](https://github.com/go-estoria/estoria-contrib).

The program creates a bank-account aggregate, appends events, saves and
reloads it, and then demonstrates lower-level event store operations: reading
a stream through a projection, listing streams, and reading all events in
global order.

## Running

```sh
make up        # start MongoDB (single-node replica set) and mongo-express in Docker
go run .       # run the walkthrough; output is printed to the console
make mongosh   # open a mongosh shell in the container
make down      # stop and remove the containers and volumes
```

The mongo-express admin UI is available at http://localhost:8888 while the
containers are up. Pass an alternate connection string as the first argument,
and set `DEBUG=1` to see Estoria's debug logging.

## See Also

- [Estoria](https://github.com/go-estoria/estoria): Event sourcing toolkit for Go
- [Estoria Contrib](https://github.com/go-estoria/estoria-contrib): Event store implementations for Estoria (among other things)
