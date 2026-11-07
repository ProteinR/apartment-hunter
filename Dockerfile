# syntax=docker/dockerfile:1.7

FROM golang:1.25-alpine AS build

WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOMAXPROCS=1 GOGC=25 go build -p 1 -gcflags=github.com/gotd/td/tg=-l -trimpath -o /bin/apartment-hunter ./cmd/apartment-hunter

FROM alpine:3.22

RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /bin/apartment-hunter /usr/local/bin/apartment-hunter
COPY configs ./configs
COPY examples ./examples

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD pidof apartment-hunter >/dev/null || exit 1

ENTRYPOINT ["apartment-hunter"]
