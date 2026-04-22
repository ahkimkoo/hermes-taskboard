# Hermes Task Board · Operator Manual

A kanban-style scheduler for the Hermes Agent: jot down tasks → dispatch them to Hermes → watch each run stream in → review and archive.

> [中文](./manual.zh-CN.md) | English

---

## 1. The board and task lifecycle

The board has six columns (rendered as horizontal tabs on mobile):

| Column | Meaning | How tasks enter / leave |
|---|---|---|
| **Draft** | Ideas / notes you haven't decided to run yet | New tasks land here; drag to **Plan** when ready |
| **Plan** | Queued for execution | Tasks with `trigger_mode=auto` are auto-dispatched by the scheduler; `manual` ones wait for you to click **Start now** |
| **Execute** | Hermes is currently running | While any Attempt is streaming; auto-flips to **Verify** when every Attempt finishes |
| **Verify** | Done, awaiting your review | Read the output; drag to **Done** if happy, or send a follow-up message in the card to make Hermes continue (auto-bounces back to **Execute**) |
| **Done** | Verified complete | Manually move on to **Archive** |
| **Archive** | Filed away | Anything can be dragged here |

**Mobile**: the six columns become a horizontal tab strip, only one column visible at a time. **Drag a card up onto a tab** to move it to that column.

---

## 2. Creating a task

Click **+ New task** in the top-right (on mobile, the floating `+` in the bottom-right).

Fields:

- **Title** *(required)* — short label shown on the card
- **Description** *(optional)* — markdown, supports paste / drop / pick of:
  - Images (`png/jpg/gif/webp/svg`) → inserted as `![](url)`, render inline in the preview
  - Audio (`mp3/wav/m4a`) → `[🎵 filename](url)`
  - Video (`mp4/mov/avi/webm`) → `[🎬 filename](url)`
  - Documents (`pdf/doc/docx/xls/xlsx/ppt/pptx/txt/md`) → `[📄 filename](url)`
  - Per-file cap **50 MB**

  Uploads only work when Aliyun OSS is configured (see §6.6) — otherwise the URL wouldn't be reachable from Hermes's LLM provider.
- **Priority** P1–P5 — purely visual (red→grey badge), not used by the scheduler
- **Trigger mode**:
  - `auto` — once the task reaches the **Plan** column the scheduler dispatches it (default)
  - `manual` — never auto-runs; needs **Start now** in the card
- **Hermes Server** *(optional)* — defaults to the Server marked default; manage them in Settings
- **Model (agent profile)** *(optional)* — defaults to the server's default; if the server has no models configured, taskboard falls back to `hermes-agent`
- **Tags** *(optional)* — comma-separated. Each tag may carry a **System Prompt** (Settings) which gets concatenated into the instructions sent to Hermes
- **Dependencies** *(optional)* — pick prerequisite tasks; the scheduler waits until those reach their required state

---

## 3. Running a task (Attempt)

Open a card and the **Attempts** panel sits below the description.

- First time: click **▶ Start now** to start an Attempt
- Subsequently: **+ New attempt** opens another (auto tasks also reopen on their own when conditions allow)

Once an Attempt is running:

- **Chat bubbles** stream Hermes's output. Each bubble has a **⎘ copy** button (top-right on hover) that puts the raw markdown on your clipboard.
- **Tool calls** collapse into cards; tap the header to expand args / output.
- **Timestamps** in muted grey on every message.
- **Input box**: Ctrl/⌘+Enter to send (plain Enter inserts a newline). Auto-grows up to 6 lines on mobile.
- **Stop**: two-click confirm to avoid accidents.
- **↓ Jump to bottom** floating pill, auto-hides within 60 px of the bottom.
- **↑ Load earlier** at the top of the stream — events fetch only the most recent 30 by default.
- **🔄 Refresh** at the very bottom — reconnects to the Hermes run and re-pulls the tail. Use this instead of typing "continue" to catch up.

---

## 4. Tags & System Prompts

Settings → **Tags** lets you maintain the tag library. Beyond name and color, each tag can carry a **System Prompt**.

When a task is dispatched, every tag-prompt attached to it is concatenated and shipped as **instructions** (the `/v1/responses` equivalent of a `role=system` message in `/v1/chat/completions`).

Example:
- Tag `wechat-notify` carries a prompt: "Once the task is complete, POST a markdown summary to https://qyapi.weixin.qq.com/...".
- Any task tagged `wechat-notify` automatically gets that behaviour without repeating it in every description.

---

## 5. Scheduled runs

Each task card has a collapsible **Schedule** section. Open it and click **+ Add schedule**. Modes:

- **Every N minutes / hours** — simple recurring interval
- **Daily at HH:MM**
- **Weekly on [days] at HH:MM** — pick one or more weekdays
- **Monthly on day D at HH:MM**
- **Advanced (cron)** — raw 5-field cron expression

The backend stores **only standard cron** (robfig/cron/v3); the picker translates the friendly choices into a cron string and renders existing rows back as human prose ("Weekly Mon, Wed, Fri at 09:00"). A task can carry as many schedules as you like; each is independently on/off.

A scheduled fire creates a fresh Attempt (same as clicking **New attempt**), regardless of which column the task currently sits in.

---

## 6. Settings reference

Click the gear icon in the top-right.

### 6.1 Hermes Servers

Taskboard reaches Hermes over one of **two transports**. You can mix them — different Hermes hosts on different transports in the same taskboard.

| | 🌐 **HTTP** | 🔌 **Plugin** |
|---|---|---|
| Direction | taskboard → Hermes | Hermes → taskboard |
| Hermes side | Enable `api_server` platform | Install `hermes-taskboard-bridge` pip package + change startup command |
| taskboard side | Enter base URL + API key | Optionally pre-register by ID (else auto-register when plugin connects) |
| Session survives if taskboard disconnects | ❌ `api_server` interrupts the run on SSE drop | ✅ Hermes owns the session; taskboard can come and go |
| Cancel semantics | HTTP cancel (historic 404 class of bugs; now self-healed) | Hermes's native `/stop` interrupt |
| When to pick which | Hermes on remote host behind HTTP proxy you don't control | Self-hosted Hermes on a machine where you can `pip install` |

Both transports appear side-by-side in Settings → Hermes Servers. Two buttons, **+ 🌐 HTTP server** and **+ 🔌 Plugin server**, open distinct forms with inline ops guides (and a one-click "Copy prompt to Hermes" button — paste it into any Hermes chat and Hermes will do the setup itself).

#### HTTP server fields

- **ID / Name** — internal identifier + display label
- **Base URL** — e.g. `http://127.0.0.1:8642` (local) or remote IP + port
- **API Key** — Hermes's `API_SERVER_KEY`; **encrypted at rest** with AEAD using the key in `data/db/.secret`
- **Default server** — used when a task doesn't specify one
- **Server max concurrent** — cap on simultaneous Attempts against this server
- **Models (agent profiles)** — list of agent profiles the server offers
  - **Name** — must match Hermes's agent profile; default is `hermes-agent`
  - **Default model** — server's internal default
  - **Profile max concurrent** — secondary cap (server and profile stack)

> If a server has no models configured, taskboard falls back to `"hermes-agent"` — Hermes's built-in default.

**Hermes-side setup (manual):**

```bash
# 1. Generate an API key
openssl rand -hex 20
# 2. Add to ~/.hermes/.env
#    API_SERVER_ENABLED=true
#    API_SERVER_KEY=<the key>
#    API_SERVER_HOST=0.0.0.0
#    API_SERVER_PORT=8642
# 3. Restart
hermes gateway restart
# 4. Verify
curl -s http://127.0.0.1:8642/health    # -> {"status":"ok","platform":"hermes-agent"}
```

**Hermes-side setup (tell Hermes to do it)** — paste this prompt into any running Hermes chat session:

```
Help me enable Hermes's API server so that taskboard can reach this host.

1. Generate a random API key: run `openssl rand -hex 20` and remember the output.
2. Open ~/.hermes/.env and add (or update) these four lines:
     API_SERVER_ENABLED=true
     API_SERVER_KEY=<the key from step 1>
     API_SERVER_HOST=0.0.0.0
     API_SERVER_PORT=8642
3. Restart Hermes: `hermes gateway restart` (or `hermes gateway start` if it wasn't running).
4. Verify: `curl -s http://127.0.0.1:8642/health` should print `{"status":"ok","platform":"hermes-agent"}`.
5. Report back: (a) the base URL other hosts can reach (e.g. http://<this-host-ip>:8642), and (b) the API key you generated. I'll paste them into taskboard.
```

#### Plugin server fields

- **ID** — must match what the plugin announces: either `TASKBOARD_HERMES_ID` env var on the Hermes side, or (if unset) that host's machine hostname. **You don't have to pre-register** — plugins that connect with an unknown ID appear automatically in the servers list as "auto-registered" entries.
- **Name** — friendly label; optional
- **Default server** — used when a task doesn't specify one
- **Server max concurrent** — cap on simultaneous Attempts (defaults to 5)

No base URL or API key — the plugin dials us, not the other way around. Authentication is an optional shared secret (`TASKBOARD_PLUGIN_TOKEN`); not required on single-machine setups.

**Hermes-side setup (manual):**

```bash
# 1. Install the plugin (into Hermes's venv, if any)
pip install hermes-taskboard-bridge
# 2. Add to ~/.hermes/.env
#    TASKBOARD_WS_URL=ws://<taskboard-host>:1900/api/plugin/ws
#    TASKBOARD_HERMES_ID=<short-name>    (optional; defaults to hostname)
# 3. Swap the startup command — pick the one that matches your setup:
#    systemd:     hermes-taskboard-bridge install-service && hermes gateway restart
#    pm2:         pm2 delete hermes && pm2 start "hermes-taskboard-bridge run" --name hermes && pm2 save
#    foreground:  run `hermes-taskboard-bridge run` instead of `hermes gateway run`
# 4. Verify
hermes-taskboard-bridge doctor
```

The plugin auto-registers on first connect; no further action needed in taskboard unless you want to set a friendly name or concurrency cap.

**Hermes-side setup (tell Hermes to do it):**

```
Help me connect this Hermes to taskboard via the plugin bridge.

1. Install the plugin into Hermes's Python environment:
     pip install hermes-taskboard-bridge
   (If Hermes lives in a venv, use its pip — e.g. ~/.hermes/hermes-agent/venv/bin/pip install hermes-taskboard-bridge)
2. Open ~/.hermes/.env and add these two lines:
     TASKBOARD_WS_URL=ws://<TASKBOARD_HOST>:1900/api/plugin/ws
     TASKBOARD_HERMES_ID=<a short name for this host; leave blank to use hostname>
3. Swap the Hermes startup command so it loads the bridge wrapper. Pick whichever matches how Hermes is managed:
     - systemd (`hermes gateway start`):    `hermes-taskboard-bridge install-service && hermes gateway restart`
     - pm2:                                  `pm2 delete hermes && pm2 start "hermes-taskboard-bridge run" --name hermes && pm2 save`
     - foreground shell / docker:            run `hermes-taskboard-bridge run` instead of `hermes gateway run`
4. Verify: `hermes-taskboard-bridge doctor` should print all ✓ and echo the TASKBOARD_WS_URL you set.
5. Report back whether the doctor command succeeded. Taskboard auto-registers the plugin — no further action needed on the taskboard side.
```

Replace `<TASKBOARD_HOST>` with the hostname/IP of this taskboard (e.g. `127.0.0.1` if Hermes and taskboard run on the same box).

### 6.2 Tags

Library of tag definitions.

- **Name** + **Color** (chip color on cards)
- **System Prompt** — see §4

### 6.3 Scheduler

- **Scan interval (seconds)** — how often the scheduler polls the Plan column for dispatchable tasks. Default 5.
- **Global max concurrent attempts** — system-wide cap. Server and profile caps subdivide it.

### 6.4 Archive

- **Auto-purge days** — `data/attempt/{id}/` directories whose mtime is older than N days **and** whose row no longer exists in the DB get swept. Default 90.

### 6.5 Preferences

- **Language** — `zh-CN` / `en`, hot-swaps without reload
- **Theme** — dark / light
- **Sound** (`enabled` + `volume` + three event toggles):
  - `execute_start` — chime when a task starts running
  - `needs_input` — when an Attempt's state flips to needs-input
  - `done` — when an Attempt terminates (completed / failed / cancelled)

### 6.6 OSS (image uploads)

Pasting images into a task description requires this. Hermes is given the description as text, so embedded images need to be at a public URL the LLM can fetch — the local taskboard server can't host them, hence Aliyun OSS.

- **Enabled** toggle
- **Endpoint** — e.g. `oss-cn-shanghai.aliyuncs.com`
- **Bucket / AccessKey ID / Secret** — Aliyun sub-account credentials
- **Path prefix** — folder inside the bucket, e.g. `taskboard/`
- **Public base URL** — prefix used when building the final URL, e.g. `https://your-cdn.com/taskboard/`

When OSS is disabled, image paste / drop is rejected by the UI.

### 6.7 Auth

- **Enable username / password** — pick a credential pair; subsequent access requires login
- **Change password**
- **Disable** — back to no auth

Without auth, anyone who can reach port 1900 controls the board. **Only safe for localhost or a trusted LAN.**

---

## 7. Mobile-specific notes

- **Drag**: touching a card and moving 5 px starts the drag immediately. Drag onto a column tab at the top to move the card to that column.
- **Scroll**: card hit-areas are owned by drag, so scroll the column by touching the **18 px gutters on each side** of cards or the **14 px gap between cards**.
- **PWA install**: Browser menu → Add to Home Screen. Standalone mode kicks in via the iOS legacy meta tags + the manifest.

---

## 8. Troubleshooting

**Task stuck in "Plan" and never runs**
- Confirm `trigger_mode` is `auto` (`manual` requires the Start now button)
- Check that all dependencies have reached their required state
- Verify the server's `base_url` is reachable (Settings → Test connection)
- Watch the scheduler logs in the bottom-right of the page

**Attempt stuck in "running" with no new events**
- On taskboard restart, orphan attempts try to reconnect to their Hermes run via `/v1/runs/{runID}/events`. If Hermes already finished, the attempt is marked `failed` with a specific reason
- If it really seems stuck, hit the **🔄 Refresh** pill at the bottom of the message stream

**Phone doesn't show your latest code change**
- Static assets ship with `Cache-Control: no-cache` so the browser revalidates every load
- If still stuck: clear browsing data, or DevTools → right-click reload → Empty Cache and Hard Reload
- Check the version chip in the bottom-left to confirm what build is loaded

**Tag system prompt seems ignored by the agent**
- The event stream contains a `— sent system prompt (N chars) —` audit divider; expand to see the `instructions` field actually sent
- Hermes layers it on top of the agent profile's base prompt — the model has it; if behaviour doesn't match, tighten the prompt wording
