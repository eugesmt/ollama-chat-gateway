# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS build

ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /src

RUN apk add --no-cache ca-certificates

COPY go.mod go.sum* ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' -o /out/server ./cmd/server

FROM alpine:3.22

RUN apk add --no-cache ca-certificates wget \
    && addgroup -S app \
    && adduser -S -G app -h /nonexistent -s /sbin/nologin app

COPY --from=build --chown=app:app /out/server /usr/local/bin/server

USER app:app
WORKDIR /app

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/server"]
