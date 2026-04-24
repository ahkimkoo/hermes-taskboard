# syntax=docker/dockerfile:1.6
# Build a static Go binary, then package it in alpine with a small
# entrypoint script so bind-mounted /data works regardless of its host
# ownership. The previous distroless layout required operators to
# manually `chown 65532:65532 taskboard-data` before `docker compose
# up` — this version chowns on boot instead.

FROM golang:1.25-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.Version=${VERSION}" \
    -o /out/hermes-taskboard ./cmd/taskboard

FROM alpine:3.20
LABEL org.opencontainers.image.title="hermes-taskboard" \
      org.opencontainers.image.source="https://github.com/ahkimkoo/hermes-taskboard" \
      org.opencontainers.image.licenses="MIT"

# su-exec drops privileges after the entrypoint fixes /data ownership.
# tini makes PID 1 a real init so SIGTERM propagates cleanly to the Go
# binary on `docker compose down`.
RUN apk add --no-cache su-exec tini ca-certificates tzdata \
    && addgroup -S -g 1000 taskboard \
    && adduser -S -u 1000 -G taskboard -s /sbin/nologin taskboard

WORKDIR /app
COPY --from=build /out/hermes-taskboard /app/hermes-taskboard
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# Seed /data with the right owner so a named volume (which initialises
# from the image) comes up writable out of the box. Bind mounts get
# fixed at container start by the entrypoint.
RUN mkdir -p /data && chown -R taskboard:taskboard /data

VOLUME ["/data"]
EXPOSE 1900

ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/docker-entrypoint.sh"]
CMD ["/app/hermes-taskboard", "-data", "/data"]
