# syntax=docker/dockerfile:1

# Build stage. CGO is off because the SQLite driver is modernc.org/sqlite, a
# pure-Go implementation — so the result is a static binary that runs in an
# image with no libc at all.
FROM golang:1.26 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/chess .

# The database directory has to be created here: the final image has no shell
# to mkdir with, and the app runs as a non-root user that couldn't write to a
# root-owned directory anyway. 65532 is distroless' "nonroot" uid/gid.
RUN mkdir -p /data && chown 65532:65532 /data

# Runtime stage: no shell, no package manager, no libc — just the binary and a
# writable data directory.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/chess /usr/local/bin/chess
COPY --from=build --chown=65532:65532 /data /data

USER nonroot:nonroot

# Informational only. The app listens on $PORT when the platform sets one
# (Railway, Fly, Cloud Run, ...), falling back to :8084.
EXPOSE 8084

# The database lives in /data. There is deliberately no VOLUME instruction:
# Railway rejects it ("docker VOLUME ... is not supported, use Railway Volumes"),
# and it buys nothing here — `docker run -v name:/data` and a Railway Volume
# mounted at /data both work without it. With no volume at all the container
# starts from an empty lobby every time, which is what the hosted demo wants.

# The default command is the hosted-demo configuration: the lobby is cleared on
# the hour, writes are rate limited per client IP, and live connections are capped.
# Override by passing your own arguments to `docker run`. None of this is on
# when the example is run directly with `go run .`.
ENTRYPOINT ["/usr/local/bin/chess"]
CMD ["-db", "/data/chess.db", "-hourly-reset", "-trust-proxy", "-writes-per-minute", "60", "-max-clients", "200"]
