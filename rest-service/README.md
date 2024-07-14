# REST Service

This example demonstrates how to create a basic REST API service that uses [Estoria](https://github.com/go-estoria/estoria) to store and load entities using event sourcing.

Key Takeaways:

- How to create an event store that loads and stores events
- How to create an aggregate store that uses the event store to load and store aggregates
- How CQRS is not required to use event sourcing

The example code uses an in-memory event store for simplicity; terminating the application will wipe the event store data. In a real-world application, you would choose (or create) a persistent event store implementation backed by a data store.

## Running the example

To run the example, run the Go program in the `main` package:

    ```shell
    go run ./cmd
    ```

This will start a simple HTTP server to which you can send requests to create, update, and retrieve entities.

To view debug log output from Estoria, set the `DEBUG` environment variable to `true` prior to running the program.

## API

The API has the following endpoints:

| Method  | Path            | Description              |
| ------- | --------------- | ------------------------ |
| POST    | `/entities`     | Create a new entity      |
| GET     | `/entities/:id` | Retrieve an entity by ID |
| DELETE  | `/entities/:id` | Delete an entity by ID   |

## Project Layout

The project is structured as follows:

- `cmd`: Main package; configures and injects application dependencies and starts the HTTP server
- `internal/application`: Domain logic layer; handles HTTP requests and uses the database layer to load and store Accounts
- `internal/database`: Database layer; uses Estoria to load and store Accounts via an aggregate store

## See Also

- [Estoria](https://github.com/go-estoria/estoria): Event sourcing toolkit for Go
- [Estoria Contrib](https://github.com/go-estoria/estoria-contrib): Event store implementations for Estoria (among other things)
