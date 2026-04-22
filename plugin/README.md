# hermes-taskboard-bridge

A Hermes gateway extension that exposes a WebSocket-driven platform
bridge so [hermes-taskboard](../../README.md) can own Hermes session
lifecycles independently of any HTTP/SSE client connection.

**No changes to Hermes source files.** Patches live entirely in the
running Python interpreter; the package applies them when its entry
point runs.

## Why this exists

Hermes's built-in `/v1/responses` endpoint explicitly cancels a
streaming agent run when the HTTP client disconnects (see
`gateway/platforms/api_server.py` — it calls `agent.interrupt()` on
client disconnect). For a task-board–style product where the browser
tab or the taskboard server itself can come and go, that design loses
work.

This package registers a new `Platform.TASKBOARD` platform whose adapter
keeps the session alive regardless of client connectivity, buffers
events, and replays them after reconnect.

## Install

In the same venv as `hermes-agent`:

```bash
pip install hermes-taskboard-bridge
```

Or from a local source clone (for development):

```bash
pip install -e ./plugin
```

Then swap the pm2 / systemd command from:

    hermes gateway run

to:

    hermes-taskboard-bridge run

Example `pm2 restart` flow:

```bash
pm2 delete hermes
pm2 start "hermes-taskboard-bridge run" --name hermes
pm2 save
```

## Configure

Add to `~/.hermes/.env`:

```
TASKBOARD_WS_URL=ws://127.0.0.1:1900/api/plugin/ws
TASKBOARD_PLUGIN_TOKEN=         # optional; sent in hello_ack frame
TASKBOARD_HERMES_ID=            # optional; defaults to socket.gethostname()
```

`TASKBOARD_HERMES_ID` is the identity taskboard uses to route tasks
back to *this* Hermes host. Leave blank to use the machine hostname;
set it explicitly when you have multiple Hermes installs on the same
host or want a shorter / more stable label than the hostname.

No `config.yaml` changes required — when `TASKBOARD_WS_URL` is set the
bridge injects its own `PlatformConfig` at startup.

### Taskboard-side auto-registration

When a plugin connects, taskboard auto-registers it under the
announced `hermes_id`: no need to add a `hermes_servers[]` entry on
the taskboard side. Auto-registered servers appear in the task-create
dropdown with a 🔌 badge and `max_concurrent=5` defaults. Add a real
`hermes_servers[]` entry with `transport: plugin` + matching `id`
only when you want a friendly `name` or a non-default concurrency cap.

## Verify

```bash
hermes-taskboard-bridge doctor
```

This imports Hermes, applies the patches in dry-run, and prints what's
configured. If your Hermes version has refactored any of the attributes
this package touches, `doctor` will fail with a clear message.

## Uninstall

```bash
pip uninstall hermes-taskboard-bridge
```

…and revert the pm2/systemd command back to `hermes gateway run`.

## Protocol

One JSON message per WebSocket frame, bidirectional. See `adapter.py`
for the full frame catalogue.

## What the runtime patches actually do

All applied by `runtime.apply_patches()`, which is invoked at startup by
the `hermes-taskboard-bridge run` entry point:

1. Adds a new `TASKBOARD = "taskboard"` value to `gateway.config.Platform`.
2. Adds a `taskboard` entry to `hermes_cli.platforms.PLATFORMS` so
   toolset resolution works (`default_toolset="hermes-cli"`).
3. Wraps `GatewayRunner._create_adapter` to instantiate our adapter when
   it sees `Platform.TASKBOARD`; all other platforms fall through to the
   original.
4. Wraps `GatewayRunner._is_user_authorized` to always return True for
   the new platform (taskboard connections are trusted, they're not
   per-user chats).
5. Wraps `GatewayRunner.start` so that on each boot it injects a
   `PlatformConfig` for TASKBOARD into `runner.config.platforms` when
   `TASKBOARD_WS_URL` is set — avoids requiring the user to edit
   `config.yaml`.
6. Pre-seeds `TASKBOARD_HOME_CHANNEL` so the one-time "No home channel
   is set" onboarding message doesn't fire (irrelevant for a
   machine-to-machine platform).
