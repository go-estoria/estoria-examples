# syntax=docker/dockerfile:1

# Build stage. CGO is off: both drivers in play (modernc SQLite, pgx) are pure
# Go, so the result is a static binary that runs in an image with no libc.
FROM golang:1.26 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/inspector .

# Runtime stage: no shell, no package manager, no libc — just the binary. This
# example stores nothing of its own; it only reads someone else's event store.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/inspector /usr/local/bin/inspector

USER nonroot:nonroot

# Informational only. The app listens on $PORT when the platform sets one
# (Railway, Fly, Cloud Run, ...), falling back to :8085.
EXPOSE 8085

# The default command points at a Postgres store supplied as $DATABASE_URL,
# which is what a managed database add-on sets — so a hosted inspector browses
# whichever service's database it is given, with no image change. Pass -backend
# sqlite -dsn /path/to.db to read a SQLite file instead.
#
# -reads-per-minute meters only /api/all/tail, the one endpoint that scans the
# whole store. Nothing here writes, so there is no reset and nothing to throttle
# on the write side. None of this is on when run directly with `go run .`.
ENTRYPOINT ["/usr/local/bin/inspector"]
CMD ["-backend", "postgres", "-trust-proxy", "-reads-per-minute", "60"]
