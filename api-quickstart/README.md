# API Quickstart

This example demonstrates the basic usage of all core [Estoria](https://github.com/go-estoria/estoria) components in a single, unannotated `main()` function.

In-memory implementations are used wherever persistence is required so the example can be run without any additional dependencies.

## Running the example

To run the example, run the Go program in the `main` package:

    ```shell
    go run ./cmd
    ```

This will run the example code, which constructs Estoria components and then creates, saves, and retrieves an entity, appending events to modify the entity's state. Output is printed to the console.

To view debug log output from Estoria, set the `DEBUG` environment variable to `true` prior to running the program.

## See Also

- [Estoria](https://github.com/go-estoria/estoria): Event sourcing toolkit for Go
- [Estoria Contrib](https://github.com/go-estoria/estoria-contrib): Event store implementations for Estoria (among other things)
