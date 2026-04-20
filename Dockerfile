# syntax=docker/dockerfile:1.6
# Multi-stage: build a static binary in golang, then drop into a minimal image.

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
# Seed an empty /data skeleton so the distroless stage can COPY --chown it
# in — distroless has no shell, so a RUN mkdir+chown at the final stage isn't
# possible and a bare VOLUME directive leaves /data owned by root.
RUN mkdir -p /skel-data

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="hermes-taskboard" \
      org.opencontainers.image.source="https://github.com/ahkimkoo/hermes-taskboard" \
      org.opencontainers.image.licenses="MIT"
WORKDIR /app
COPY --from=build /out/hermes-taskboard /app/hermes-taskboard
# distroless/nonroot is UID/GID 65532. Pre-owning /data means both named
# volumes and bind mounts (when the host path is world-writable) let the
# binary mkdir its subdirs without a host-side chown step.
COPY --from=build --chown=65532:65532 /skel-data /data
VOLUME ["/data"]
EXPOSE 1900
USER nonroot:nonroot
ENTRYPOINT ["/app/hermes-taskboard", "-data", "/data"]
