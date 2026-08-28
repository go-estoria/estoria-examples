# syntax=docker/dockerfile:1

# Build stage. CGO is off because pgx is pure Go, so the result is a static
# binary that runs in an image with no libc at all.
FROM golang:1.26 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/ledger .

# Runtime stage: no shell, no package manager, no libc. The console's assets
# are served from disk (http.Dir("web")), so they are copied in alongside the
# binary and the working directory is set to their parent.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=build /out/ledger /app/ledger
COPY --from=build /src/web /app/web

USER nonroot:nonroot

# Informational only. The app listens on $PORT when the platform sets one
# (Railway, Fly, Cloud Run, ...), falling back to :8084.
EXPOSE 8084

# The database comes from $DATABASE_URL, which is what a managed Postgres
# add-on sets. Pass -dsn instead to point somewhere else.
#
# The default command is the hosted-demo configuration: every deploy starts
# from an empty store, and writes are rate limited per client IP. Reads are
# never limited — watching a rebuild fill a table is the whole point. None of
# this is on when the example is run directly with `go run .`.
ENTRYPOINT ["/app/ledger"]
CMD ["-reset-on-boot", "-trust-proxy", "-writes-per-minute", "60"]
