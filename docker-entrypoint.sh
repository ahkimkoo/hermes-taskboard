#!/bin/sh
# Entrypoint that lets a bind-mounted /data "just work" on first boot
# regardless of its host ownership. Docker bind mounts preserve the
# host directory's uid/gid, so a plain `mkdir taskboard-data &&
# docker compose up` leaves /data owned by the host user (often 1000)
# — the taskboard binary can't create /data/db/.secret because it runs
# as the unprivileged `taskboard` account inside the container.
#
# We start as root, fix ownership in-place, then drop to the taskboard
# user via su-exec. This replaces the manual `sudo chown 65532:65532
# taskboard-data` step from the distroless-based image.
set -e

if [ -d /data ]; then
	chown -R taskboard:taskboard /data 2>/dev/null || true
	chmod 700 /data 2>/dev/null || true
fi

exec su-exec taskboard:taskboard "$@"
