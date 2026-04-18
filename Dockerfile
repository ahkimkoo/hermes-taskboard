# syntax=docker/dockerfile:1.6
# Multi-stage: build a static binary in golang, then drop into a minimal image.

FROM golang:1.23-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.Version=${VERSION}" \
    -o /out/hermes-taskboard ./cmd/taskboard

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="hermes-taskboard" \
      org.opencontainers.image.source="https://github.com/ahkimkoo/hermes-taskboard" \
      org.opencontainers.image.licenses="MIT"
WORKDIR /app
COPY --from=build /out/hermes-taskboard /app/hermes-taskboard
VOLUME ["/data"]
EXPOSE 1900
USER nonroot:nonroot
ENTRYPOINT ["/app/hermes-taskboard", "-data", "/data"]
