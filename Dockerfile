FROM golang:1.25-alpine AS build

WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/apartment-hunter ./cmd/apartment-hunter

FROM alpine:3.22

RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /bin/apartment-hunter /usr/local/bin/apartment-hunter
COPY configs ./configs
COPY examples ./examples

ENTRYPOINT ["apartment-hunter"]
