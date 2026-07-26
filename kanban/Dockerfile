# syntax=docker/dockerfile:1

# Build stage. CGO is off because the SQLite driver is modernc.org/sqlite, a
# pure-Go implementation — so the result is a static binary that runs in an
# image with no libc at all.
FROM golang:1.26 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/kanban .

# The database directory has to be created here: the final image has no shell
# to mkdir with, and the app runs as a non-root user that couldn't write to a
# root-owned directory anyway. 65532 is distroless' "nonroot" uid/gid.
RUN mkdir -p /data && chown 65532:65532 /data

# Runtime stage: no shell, no package manager, no libc — just the binary and a
# writable data directory.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/kanban /usr/local/bin/kanban
COPY --from=build --chown=65532:65532 /data /data

USER nonroot:nonroot

# Informational only. The app listens on $PORT when the platform sets one
# (Railway, Fly, Cloud Run, ...), falling back to :8080.
EXPOSE 8080

# Mount a volume here to keep the board across deploys; without one the
# container starts from a freshly seeded board every time.
VOLUME ["/data"]

ENTRYPOINT ["/usr/local/bin/kanban", "-db", "/data/kanban.db"]
