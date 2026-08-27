FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /ledger .

FROM alpine:3.22
WORKDIR /app
COPY --from=build /ledger ./ledger
COPY web ./web
EXPOSE 8084
CMD ["./ledger"]
