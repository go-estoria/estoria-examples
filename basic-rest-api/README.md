# Basic REST API Example using Estoria

This example demonstrates how to create a basic REST API that uses Estoria to store and load entities using event sourcing.

Key Takeaways:

- How to create an event store that loads and stores events
- How to create an aggregate store that uses the event store to load and store aggregates
- CQRS is not required to use event sourcing (although it is a common pattern, and can provides many benefits)

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
