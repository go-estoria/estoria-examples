# syntax=docker/dockerfile:1

# Build stage. CGO is off: the Postgres driver (pgx) is pure Go, so the result
# is a static binary that runs in an image with no libc at all.
FROM golang:1.26 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/orders .

# Runtime stage: no shell, no package manager, no libc — just the binary. This
# example keeps nothing on local disk; its state lives in Postgres.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/orders /usr/local/bin/orders

USER nonroot:nonroot

# Informational only. The app listens on $PORT when the platform sets one
# (Railway, Fly, Cloud Run, ...), falling back to :8082.
EXPOSE 8082

# The database comes from $DATABASE_URL, which is what a managed Postgres add-on
# sets. Pass -dsn instead to point somewhere else.
#
# The default command is the hosted-demo configuration: orders are cleared on
# the hour, writes are rate limited per client IP, and live connections are
# capped. Override by passing your own arguments to `docker run`. None of this
# is on when the example is run directly with `go run .`.
ENTRYPOINT ["/usr/local/bin/orders"]
CMD ["-hourly-reset", "-trust-proxy", "-writes-per-minute", "60", "-max-clients", "200"]
