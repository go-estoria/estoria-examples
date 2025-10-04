# Telemetry

This example demonstrates how to instrument Estoria components with telemetry using the OpenTelemetry SDK.

## Running the example

First, start the dependencies using Docker Compose:

```shell
make up
```

Then, run the example code:

```shell
go run .
```

This will run the example code and send telemetry data to the configured backend. By default, the example will send telemetry data to the OpenTelemetry Collector running on `localhost:4317`.

You can then log in to the Grafana dashboard at `http://localhost:3000` using the credentials `admin`/`secret` and view the telemetry data, including traces and metrics.

## See Also

- [Estoria](https://github.com/go-estoria/estoria): Event sourcing toolkit for Go
- [Estoria Contrib](https://github.com/go-estoria/estoria-contrib): Event store implementations for Estoria (among other things)
