# Hermes + taskboard-bridge sidecar image.
#
# A drop-in Docker image that ships the Hermes gateway with our WS bridge
# plugin pre-installed and the entry point swapped. Runs alongside the
# taskboard container (typically over Docker networking at
# ws://taskboard:1900/api/plugin/ws).
#
# Build:
#   docker build -f plugin/Dockerfile -t hermes-taskboard-bridge:0.1.0 .
#
# Run (standalone):
#   docker run --rm -it \
#     -e TASKBOARD_WS_URL=ws://host.docker.internal:1900/api/plugin/ws \
#     -e OPENROUTER_API_KEY=… \
#     -v ~/.hermes:/home/hermes/.hermes \
#     hermes-taskboard-bridge:0.1.0
#
# See docker-compose.yml at the repo root for the two-service setup.

# syntax=docker/dockerfile:1.6
FROM python:3.11-slim-bookworm

LABEL org.opencontainers.image.title="hermes-taskboard-bridge" \
      org.opencontainers.image.source="https://github.com/ahkimkoo/hermes-taskboard" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.description="Hermes gateway + taskboard WebSocket bridge, patched in-memory at startup."

# hermes-agent is not published to PyPI; we install it from the upstream
# git repo. Pin to a release tag for reproducible builds:
#   docker build --build-arg HERMES_AGENT_REF=v0.9.0 ...
# Leave empty to take the default branch (only do that locally while
# testing; shipping :latest against a moving target will bite you).
ARG HERMES_AGENT_REPO=https://github.com/NousResearch/hermes-agent.git
ARG HERMES_AGENT_REF=""

# Debian slim lacks git, build-essential, and a couple of C-ext deps Hermes
# needs (e.g. for the feishu adapter's crypto). Install, then prune.
RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates \
      build-essential \
      libssl-dev \
    && rm -rf /var/lib/apt/lists/*

# Create the runtime user Hermes will own its state under.
RUN useradd --create-home --uid 1000 --shell /bin/bash hermes

USER hermes
ENV PATH="/home/hermes/.local/bin:${PATH}"
WORKDIR /home/hermes

# Install hermes-agent from git. The plugin's entry point,
# `hermes-taskboard-bridge`, is what we run instead of `hermes gateway run`.
RUN if [ -n "${HERMES_AGENT_REF}" ]; then \
        pip install --user --no-cache-dir \
            "git+${HERMES_AGENT_REPO}@${HERMES_AGENT_REF}"; \
    else \
        pip install --user --no-cache-dir \
            "git+${HERMES_AGENT_REPO}"; \
    fi

# The bridge package ships on PyPI. Override BRIDGE_VERSION to pin to an
# older release if needed; default picks up whatever's latest.
ARG BRIDGE_VERSION=""
RUN pip install --user --no-cache-dir \
      "hermes-taskboard-bridge${BRIDGE_VERSION:+==}${BRIDGE_VERSION}"

# Smoke-test the patches at build time. `doctor` imports Hermes, applies
# the runtime patches in dry-run, and fails loudly if anything moved.
RUN TASKBOARD_WS_URL=ws://dummy/ hermes-taskboard-bridge doctor

# /home/hermes/.hermes holds config.yaml, .env, sessions/, etc. — mount a
# volume here to persist across container restarts.
VOLUME ["/home/hermes/.hermes"]

# Hermes's api_server is on 8642 if enabled in config. The bridge itself is
# an outbound WS client; it needs no inbound port.
EXPOSE 8642

ENTRYPOINT ["hermes-taskboard-bridge", "run"]
