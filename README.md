# Hermes Task Board

> **Tired of running [Hermes Agent](https://github.com/NousResearch/hermes-agent) one chat at a time?** Task Board turns your existing Hermes into a parallel batch worker. Queue your backlog, set priorities and dependencies, hit **Start**, and watch tasks run sequentially or in parallel — each with its own session, live tool-call stream, retry / stop, and verifiable output. Built for Hermes users who feel the chat-only ceiling.
>
> **嫌 Hermes 一次只能聊一个对话？** 任务看板把你已经在用的 Hermes 变成并行批处理工作者。把待办全列出来、标好优先级和依赖、按下「开始」 —— 让任务串行或并行跑起来，每个任务都有独立会话、实时工具调用流、可重跑可中断、结果可验证。给已经卡在「一次一个聊天」瓶颈上的 Hermes 用户。
>
> 🚀 Single Go binary or `docker compose up` · per-user isolation with mandatory login · scheduled runs · live SSE streaming · multi-language UI

[English](#english) · [简体中文](#简体中文) · [Operator Manual (EN)](docs/manual.en.md) · [操作手册 (中文)](docs/manual.zh-CN.md)

![Board view](docs/screenshots/board-en.png)

---

## English

### Why this exists

[Hermes](https://github.com/NousResearch/hermes-agent) is one of the more interesting agents out there. Unlike OpenClaw and most other agent frameworks, Hermes **learns from use** — every session adds to its memory, skills, and context, so it compounds: the agent you're working with six months in is measurably sharper than the one you started with. It isn't a stateless tool that vanishes after each reply; it's a digital collaborator that grows alongside you.

Hermes also plugs into a remarkable range of messaging platforms — Telegram, Discord, Slack, WhatsApp, WeChat, Feishu (Lark), DingTalk, QQ, and more — and steering it from a chat client is genuinely enjoyable. The snag is that chat-driven workflows have a hard ceiling:

- A conversation is strictly serial. You can only drive one task at a time.
- Switching sessions or agent profiles mid-flight is awkward: you lose your train of thought, and running things in parallel is basically off the table.

Once you actually have a **backlog** — a batch of refactors, an audit pass, a nightly sweep, a stack of small scripted jobs — the chat-first model starts to fight you. What you want is to queue everything up front, declare priorities and dependencies, then step away and let Hermes chew through it in order or in parallel while you do something else.

That's what this project is for. **Hermes Task Board turns Hermes from a chatbot you tend one conversation at a time into a batch-capable work partner.** Plan once, dispatch automatically, watch the tool calls stream live, verify, move on. The goal is simple: get dramatically more out of every hour Hermes is running.

### What it does

Hermes Agent executes tools, edits files, and runs shell commands. This project gives you a 6-column kanban so you can:

1. **Define** tasks — markdown description, tags, priority, dependencies, preferred Hermes server + model.
2. **Dispatch** each task to a Hermes conversation as one or more parallel **Attempts** (auto-triggered by the scheduler, or manually via *Start*).
3. **Watch** Hermes think and tool-call in real time — NDJSON event log on disk, pushed to the browser over SSE.
4. **Verify** the result, ask follow-up questions in the same session, then move the card to Done or Archive.

### Features

Frontend:

- **6-column board** with drag-to-reorder and drag-to-transition (mobile: drag a card onto a column tab to move it cross-column).
- **Markdown task descriptions** with image paste / drop (gated behind Aliyun OSS).
- **Bilingual UI** — `zh-CN` / `en`, hot-swap without reload.
- **Dark / light theme** toggle.
- **Tag system prompts** — every tag can carry a system prompt that gets injected into the Hermes call.
- **Scheduled runs** — friendly cron picker (every-N-minutes, daily, weekly, monthly, advanced) with a live cron preview.
- **Live Attempt panel** — chat-style streaming of Hermes's output, collapsible tool cards, per-message timestamps, copy-as-markdown buttons, jump-to-bottom pill, load-earlier pagination, manual refresh / reconnect.
- **Multi-line auto-growing chat input** with Ctrl/⌘+Enter to send.
- **PWA** — proper standalone install on iOS/Android with bundled icons.
- **In-app help (?)** — bottom-right opens a bilingual operator manual.
- **Frontend version chip** in the bottom-left for unambiguous bug-report builds.

Backend:

- **Single Go binary** (`-data ./data`) plus a Docker image (`successage/hermes-taskboard`).
- **Three-level concurrency gates** — global, per-server, per-(server, profile).
- **Resume orphan attempts** — taskboard restart reconnects to mid-flight Hermes runs via `/v1/runs/{id}/events`; only attempts whose remote run is genuinely gone get marked failed.
- **Tag prompt injection** — sent as `instructions` (the Hermes equivalent of a `role=system` message) on every turn.
- **Encrypted API key storage** (AEAD with `data/.secret`).
- **Filesystem reaper** — purges `data/attempt/{id}/` directories older than the configured retention.
- **Network-first service worker + `Cache-Control: no-cache`** so deploys propagate without forcing users to clear caches.

### Screenshots

| | |
|---|---|
| ![Board EN](docs/screenshots/board-en.png) | ![Board ZH](docs/screenshots/board-zh.png) |
| Desktop board, English | The same board after toggling to Chinese |
| ![Attempt live](docs/screenshots/attempt-live.png) | ![Settings](docs/screenshots/settings-servers.png) |
| Live SSE stream from a running Hermes attempt | Hermes Servers settings page |
| ![Mobile](docs/screenshots/board-mobile.png) | ![Login](docs/screenshots/login.png) |
| Phone layout: one column at a time, status tabs on top | Login page (default admin: admin / admin123) |

### Quick start (download a release)

```bash
# 1. Grab the binary for your platform from the GitHub releases page:
#    https://github.com/ahkimkoo/hermes-taskboard/releases
curl -LO https://github.com/ahkimkoo/hermes-taskboard/releases/latest/download/hermes-taskboard-v0.1.0-linux-amd64.tar.gz
tar -xzf hermes-taskboard-v0.1.0-linux-amd64.tar.gz
cd hermes-taskboard-v0.1.0-linux-amd64

# 2. (Re)start your Hermes gateway with the HTTP API enabled.
API_SERVER_ENABLED=true API_SERVER_KEY=your-strong-key hermes gateway run

# 3. Start the board.
./hermes-taskboard -data ./data
# then open http://127.0.0.1:1900 in your browser
```

On first visit you'll land on the login page. The board ships with a default admin — **username `admin` / password `admin123`**. Log in, then immediately change the password via **⚙ Settings → Access control**. Forgot it? Stop the server and run `./hermes-taskboard -data ./data --reset-admin` to put the admin account back to the default.

Once logged in, click **⚙ Settings → Hermes Servers → New server**, point `base_url` at `http://127.0.0.1:8642` and paste the same `API_SERVER_KEY`. Hit **Test Connection** — green means you're good. Back on the board, create a task and click **▶ Start**.

#### Authentication & multi-user

**Login is mandatory.** There is no anonymous mode and no read-only landing page. Every visit hits `/login` first and resolves to a signed session cookie before the board renders. The default admin (**`admin` / `admin123`**) is created on first boot if no users exist; change the password the moment you log in via **⚙ Settings → Access control**, or run `./hermes-taskboard -data ./data --reset-admin` to put it back to default if you locked yourself out.

Passwords are stored as **bcrypt hashes** (cost 12) inside each user's `config.yaml`. Plaintext is never written to disk. Hermes API keys live in the same file but as **AEAD-encrypted ciphertext** — the symmetric key is auto-generated at first boot into `data/.secret` (chmod 0600); rotate it by deleting the file (you'll need to re-enter all API keys after).

**Per-user folder layout.** Every user owns a directory. Wiping a user is `rm -rf data/{username}/` — no cross-user state to clean up:

```
data/
  config.yaml                          # global: server listen, scheduler, archive, OSS, session
  .secret                              # AEAD key for encrypting Hermes API keys at rest (chmod 0600)
  admin/
    config.yaml                        # per-user: bcrypt hash, is_admin, preferences, hermes_servers[]
    disabled                           # sentinel — presence disables the account (admins can flip via UI)
    db/taskboard.db                    # this user's tasks, deps, attempts, schedules (SQLite, WAL)
    task/{task-id}.json                # task description (markdown source) + metadata
    attempt/{attempt-id}/              # one directory per attempt
      events.ndjson                    # full Hermes event stream for replay
      meta.json                        # session id, run id, server id, model, input/output counters
    tags/{name}.private                # private tag (visible only to admin)
    tags/{name}.public                 # shared tag (visible read-only to other users)
  tony/                                # every field above scoped to Tony
    config.yaml
    db/…
    task/…
    attempt/…
    tags/…
```

**Strict isolation.** Each user only sees their own board, their own attempts, their own tags. Admins do **not** get a cross-user view — to work as another user you log out and log back in as them. The board treats username == directory name as the canonical identity, so renaming a user means delete-and-recreate.

**Sharing.** Tags and Hermes servers can be marked **Shared**. Shared entries show up in every other user's list as read-only — visible + usable to dispatch tasks against, but the owner is the only one who can edit / delete them. Tag sharing lives in the file extension (`.public` vs `.private`); server sharing is the `shared: true` flag in YAML.

**Admin features** (visible only when `is_admin: true`):

- **⚙ Settings → Users** — invite new users, reset their passwords, toggle the `disabled` sentinel, promote / demote admin rights.
- **⚙ Settings → Global** — scheduler tick interval, max concurrent attempts (global / per server), OSS integration for image upload, archive retention, "Reload config from file".

**Migration from pre-v0.3.0.** When an older single-DB install boots against a v0.3.0+ binary for the first time, a one-shot migration reassigns every task / tag / Hermes server to `admin` and moves them into `data/admin/`. The old central `data/db/taskboard.db` is removed once rows are copied (the AEAD key at `data/.secret` stays put — it's still used at runtime). **Back up `data/` externally before upgrading** if you want a safety net; the migration does not keep one for you.

### Set up Hermes for this board

The Hermes gateway ships an OpenAI-compatible HTTP API on port **8642**. Full upstream reference: **[Hermes API Server docs](https://hermes-agent.nousresearch.com/docs/user-guide/features/api-server)**.

Enable it one of three ways:

- **Environment variables** (quick and dirty):
  ```bash
  API_SERVER_ENABLED=true \
  API_SERVER_KEY=choose-a-strong-key \
  API_SERVER_PORT=8642 \
  API_SERVER_HOST=127.0.0.1 \
    hermes gateway run
  ```
- **`~/.hermes/.env`** (persistent): put the same four lines there.
- **`~/.hermes/config.yaml` → `platforms.api_server`** — see the [API Server docs](https://hermes-agent.nousresearch.com/docs/user-guide/features/api-server) for every supported field.

Sanity check:

```bash
curl -H "Authorization: Bearer your-strong-key" http://127.0.0.1:8642/health/detailed
curl -H "Authorization: Bearer your-strong-key" http://127.0.0.1:8642/v1/models
```

If you want other machines on your LAN to reach the API, set `API_SERVER_HOST=0.0.0.0` **and** use a key of at least 8 characters — Hermes refuses to bind network interfaces without one.

### Build from source

```bash
git clone git@github.com:ahkimkoo/hermes-taskboard.git
cd hermes-taskboard
./build.sh                           # current host platform
GOOS=linux GOARCH=arm64 ./build.sh   # cross-compile
VERSION=v0.1.0 ./release.sh          # cross-platform archives under dist/
```

`release.sh` produces `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, and `windows/amd64` archives by default. Each archive ships the binary, `config.example.yaml`, an empty `data/` skeleton (`db/`, `task/`, `attempt/`), and a copy of `README.md`, `CHANGELOG.md`, and `LICENSE`.

### Docker

A pre-built image is published to Docker Hub: **`successage/hermes-taskboard`**. Use the published image directly:

```bash
docker run -d --name taskboard \
  -p 1900:1900 \
  -v taskboard-data:/data \
  --add-host=host.docker.internal:host-gateway \
  successage/hermes-taskboard:latest
```

Or build from source:

```bash
docker build -t hermes-taskboard:dev .
docker run -d --name taskboard \
  -p 1900:1900 \
  -v "$PWD/tb-data:/data" \
  --add-host=host.docker.internal:host-gateway \
  hermes-taskboard:dev
```

#### docker compose

The same deployment with a folder-mapped data directory (`./taskboard-data` on the host → `/data` inside the container) — drop this `docker-compose.yml` anywhere convenient and `docker compose up -d`:

```yaml
services:
  taskboard:
    image: successage/hermes-taskboard:latest
    container_name: taskboard
    ports:
      - "1900:1900"
    environment:
      TZ: Asia/Shanghai                   # cron schedules are read in this timezone (default UTC)
    volumes:
      - ./taskboard-data:/data            # host folder → container's /data
    extra_hosts:
      - "host.docker.internal:host-gateway"  # reach Hermes running on the host
    restart: unless-stopped
```

> **Timezone note**: the image defaults to `TZ=UTC`. If you want `0 9 * * *` to fire at 9am in your local time rather than 9am UTC, set `TZ` to your zone (e.g. `Asia/Shanghai`, `Europe/London`, `America/Los_Angeles`).

First run — nothing to prep beyond creating the host folder:

```bash
mkdir -p taskboard-data
docker compose up -d
```

The container starts as root just long enough to `chown` the bind-mounted `/data` to its internal `taskboard` user (UID 1000), then drops privileges via `su-exec`. You don't need `sudo` or a `chown` on the host.

After that, everything — `data/config.yaml`, per-user dirs, the SQLite databases, attempt logs — lives in `./taskboard-data/` on the host. `rm -rf taskboard-data/{username}/` wipes a single user cleanly (see the [multi-user section](#multi-user-folder-level-pluggability)); `docker compose down && rm -rf taskboard-data` wipes everything.

To upgrade, pull + recreate:

```bash
docker compose pull
docker compose up -d
```

The per-user layout stays intact across restarts. If you're upgrading from a pre-v0.3.0 single-DB install, the first boot runs a one-shot migration that moves everything under `data/admin/` and removes the old central DB. Snapshot the host folder with `tar czf taskboard-data.backup.tar.gz taskboard-data` first if you want a pre-migration safety net.

#### Pointing taskboard at Hermes

In the app's **Settings → Hermes Servers**, set `base_url` to `http://host.docker.internal:8642` (Hermes runs on the host, not inside the container).

> **Security note**: When taskboard and Hermes share an OS user (typical local-dev setup), the Hermes agent — through its `terminal` / `execute_code` tools — has filesystem read access to `data/{username}/db/taskboard.db`. The encrypted API keys are safe at rest, but tag system prompts and task descriptions are plain text. For production, sandbox Hermes (separate container / OS user) so it can't reach taskboard's data dir.

### Architecture

```
Browser (Vue 3)              Hermes Task Board (Go)            Hermes Gateway
-----------------            ---------------------------       ------------------
Kanban view      ────SSE───► HTTP + SSE hub
Execute modal    ────HTTP──► Board service (state machine)
                             │
                             ▼
                             Scheduler  ─┐
                             AttemptRunner ───HTTP + SSE────► /v1/responses
                             │                                /v1/runs/{id}/events
                             ▼
                             SQLite + data/{task,attempt}/    model: hermes-agent
```

- **State machine:** `draft → plan → execute → verify → done → archive`. Only `plan → execute`, `execute → verify`, and `verify → execute` are auto-transitions; everything else happens via drag.
- **One Attempt = one Hermes conversation.** Follow-ups during verification stay in the same conversation — they just spawn a new `run_id`.
- **Concurrency** is gated at three levels: global, per-server, and per `(server, model)` profile. Defaults are 50 / 10 / 5.

### Config hot-reload

Everything configurable — credentials, registered Hermes servers, scheduler knobs, preferences — lives in `data/config.yaml`. Edit it by hand, then either click **Reload config from file** in the Settings page or `POST /api/config/reload`. No restart required.

### Development

```bash
go build ./...               # type-check
go build -o bin/tb ./cmd/taskboard
./bin/tb -data ./data        # dev run; the frontend is embedded via go:embed
```

Frontend sources live at `internal/webfs/web/` and are served straight out of the binary. Editing `.js`, `.css`, or `.html` in that directory requires a Go rebuild — there's no Vite or Rollup in the loop.

### Testing

- `go build ./...` — static checks.
- `./docs/smoke.sh` — API smoke test (create → transition → delete).
- **`tests/ui_test.py`** — Playwright UI regression suite. Covers every promise documented in the CHANGELOG: drag UX, ordering, theme toggle, i18n purity, settings modal re-open, attempt list, delete-only-in-archive, rich-text editor, and column subtitles. Run against any live server:
  ```bash
  pip install playwright && playwright install chromium
  TASKBOARD=http://127.0.0.1:1900 python3 tests/ui_test.py
  ```
  Every test begins with a fresh `page.goto` reload so ordering between cases is irrelevant.

### License

MIT.

---

## 简体中文

### 项目初衷

[Hermes](https://github.com/NousResearch/hermes-agent) 是目前最有意思的 Agent 之一。不同于 OpenClaw 这类"无状态工具"，Hermes 会**在使用中成长** —— 每一次对话都会沉淀到它的记忆、技能和上下文里，用得越久越聪明。它不是一个用完即走的工具，更像一位**和你一起成长的数字伙伴**，越用越合手。

Hermes 还能对接几乎所有主流的即时通讯平台 —— Telegram、Discord、Slack、WhatsApp、微信、飞书、钉钉、QQ 等等 —— 在聊天窗口里随手遥控 Hermes 干活，体验相当不错。但聊天驱动的工作流也存在一个天然的瓶颈：

- 一段对话是严格串行的，一次只能推进一件事。
- 中途想切换 session、切换 agent profile 都有些别扭，思路容易被打断，更谈不上并行执行。

当你手头真的攒了一堆事情想交给 Hermes —— 一批重构、一次代码审计、一个夜间数据清理、一堆零散的小脚本 —— 这种聊天式的用法反而成了效率天花板。你需要的是把待办一次性列清楚，标好优先级和依赖关系，然后让 Hermes 按顺序或者并行地去啃，自己则可以腾出手来做别的事。

本项目就是为此而生。**Hermes Task Board 把 Hermes 从"一次一件"的聊天助手，升级成能吃批量活的工作搭档**：从任务清单里按优先级自动派发、工具调用实时可见、逐个验收。目标只有一个 —— 让 Hermes 的每一小时运行时间都发挥出成倍的价值。

### 这是什么

Hermes Agent 本身负责调用工具、编辑文件、执行 shell 命令。本项目给它加了一个 6 列看板，让你可以：

1. **定义**任务 —— markdown 描述、标签、优先级、依赖、指定使用哪一个 Hermes Server 和 model。
2. **分派**到 Hermes —— 每张卡可以并行开多个 **Attempt**（调度器自动触发，或手动点"开始"）。
3. **实时观察** Hermes 的思考流与工具调用 —— 落盘为 NDJSON 事件日志，通过 SSE 推送到浏览器。
4. **验证**结果，在同一个 Session 里继续追问，确认后把卡片拖到"完成"或"归档"。

### 主要功能

前端：

- **6 列看板**，列内拖动排序、跨列拖动迁移（手机端把卡片拖到顶部 tab 即可跨列）。
- **任务描述支持 Markdown**，可粘贴/拖拽图片（需先在设置里配 Aliyun OSS）。
- **中英双语**实时切换。
- **暗 / 亮主题**切换。
- **标签 System Prompt** —— 每个标签可以挂一段系统提示，跑挂了该标签的任务时自动注入。
- **定时执行** —— 友好的预设模式选择器（每 N 分钟、每天、每周、每月、高级 cron），生成的 cron 实时预览。
- **Attempt 实时面板** —— 聊天式流式显示、工具卡片可折叠、消息时间戳、复制 Markdown 按钮、跳到底部、加载更早分页、手动刷新重连。
- **多行自适应聊天框**，Ctrl/⌘+Enter 发送。
- **PWA** —— iOS / Android 可作为独立 App 安装，已内置图标。
- **页内帮助（?）** —— 右下角弹出双语操作手册。
- **左下角版本号** —— bug 报告时方便指认实际加载的前端版本。

后端：

- **单一 Go 二进制**（`-data ./data`），同时提供 Docker 镜像（`successage/hermes-taskboard`）。
- **三层并发闸门** —— 全局、单个 Server、单个 (server, profile) 三级独立配额。
- **重连孤儿 Attempt** —— taskboard 重启后自动通过 `/v1/runs/{id}/events` 重连进行中的 Hermes run；只有 Hermes 那边确实结束的才标 failed。
- **标签 Prompt 注入** —— 每轮都通过 `instructions`（Hermes 上等价于 `role=system`）发出。
- **API key 加密存储**（AEAD + `data/.secret`）。
- **文件清扫** —— 超过保留期的 `data/attempt/{id}/` 目录自动清掉。
- **Network-first SW + `Cache-Control: no-cache`** —— 部署后用户刷新就拿新代码，不用清缓存。

### 截图

| | |
|---|---|
| ![看板英文](docs/screenshots/board-en.png) | ![看板中文](docs/screenshots/board-zh.png) |
| 桌面端 6 列看板 · 英文界面 | 同一界面切换到中文后的样子 |
| ![执行面板](docs/screenshots/attempt-live.png) | ![设置页](docs/screenshots/settings-servers.png) |
| 真实 Hermes 调用中的 SSE 事件流 | Hermes Servers 管理页 |
| ![手机端](docs/screenshots/board-mobile.png) | ![登录页](docs/screenshots/login.png) |
| 手机端一次展示一列，顶部状态 tab 切换 | 登录页（默认管理员 admin / admin123） |

### 普通用户快速上手（下载 release 包运行）

```bash
# 1. 从 GitHub Releases 下载对应平台的压缩包：
#    https://github.com/ahkimkoo/hermes-taskboard/releases
curl -LO https://github.com/ahkimkoo/hermes-taskboard/releases/latest/download/hermes-taskboard-v0.1.0-linux-amd64.tar.gz
tar -xzf hermes-taskboard-v0.1.0-linux-amd64.tar.gz
cd hermes-taskboard-v0.1.0-linux-amd64

# 2. 带上 API_SERVER_KEY 启动 Hermes gateway
API_SERVER_ENABLED=true API_SERVER_KEY=你的强密钥 hermes gateway run

# 3. 启动看板
./hermes-taskboard -data ./data
# 然后在浏览器里打开 http://127.0.0.1:1900
```

首次打开页面会直接进到登录页。系统内置默认管理员 —— **用户名 `admin` / 密码 `admin123`**。登录后请立刻在 **⚙ 设置 → 访问控制** 里改掉密码；万一忘记密码，停掉服务后执行 `./hermes-taskboard -data ./data --reset-admin` 即可把 admin 重置回默认密码。

登录后点 **⚙ 设置 → Hermes Servers → 新增 server**，把 `base_url` 填成 `http://127.0.0.1:8642`，`api_key` 填刚才那个强密钥，点 **测试连接**，绿了就 OK。回到看板，新建一张任务，点 **▶ 开始** 即可。

#### 登录与多用户隔离

**登录是必须的**。没有匿名模式、没有只读首页 —— 每次访问都先到 `/login`，签发出会话 cookie 之后才能看到看板。系统首次启动且没有任何用户时，会自动创建一个默认管理员（**`admin` / `admin123`**）；登录后请立刻在 **⚙ 设置 → 访问控制** 里改掉默认密码。万一忘了，停服后执行 `./hermes-taskboard -data ./data --reset-admin` 可把 admin 拉回默认。

密码以 **bcrypt 哈希**（cost 12）存在每个用户自己的 `config.yaml` 里，磁盘上从不留明文。Hermes 的 API key 也存在同一个文件，但是 **AEAD 加密** 后的密文 —— 加密用的对称密钥在首次启动时自动生成到 `data/.secret`（权限 0600）；想换密钥就把这个文件删掉，但所有 Hermes API key 都得重新输入。

**用户文件夹规范。** 每个用户独占一个目录，清理一个用户只要 `rm -rf data/{用户名}/` —— 完全不会牵连其他人：

```
data/
  config.yaml                          # 全局：监听地址、调度、归档、OSS、session
  .secret                              # AEAD 主密钥，给 Hermes API key 加密用（chmod 0600）
  admin/
    config.yaml                        # 用户级：bcrypt 密码哈希、是否管理员、偏好、hermes_servers[]
    disabled                           # 哨兵文件 —— 存在即代表该账号被禁用（管理员可在界面切换）
    db/taskboard.db                    # 此用户的 tasks / deps / attempts / schedules（SQLite, WAL）
    task/{task-id}.json                # 任务描述（markdown 原文）+ 元数据
    attempt/{attempt-id}/              # 每次尝试一个目录
      events.ndjson                    # 完整的 Hermes 事件流，可以回放
      meta.json                        # 会话 ID、运行 ID、所用 server、model、token 计数
    tags/{name}.private                # 私有标签（只有自己可见）
    tags/{name}.public                 # 共享标签（其他用户只读可见）
  tony/                                # 上面的所有字段全部隔离到 Tony 自己的目录
    config.yaml
    db/…
    task/…
    attempt/…
    tags/…
```

**严格隔离。** 每个用户只能看到自己的看板、自己的尝试、自己的标签。**管理员也不能跨用户查看** —— 想以别人的视角操作，只能登出再用对方账号登入。系统把"用户名 == 目录名"视为唯一身份，所以重命名 = 删了重建。

**共享机制。** 标签和 Hermes server 都可以打 **共享** 标记。共享之后会出现在其他用户的列表里，**只读、可用于派发任务，但只有所有者能编辑 / 删除**。标签的共享状态写在文件后缀里（`.public` vs `.private`）；server 的共享靠 YAML 里的 `shared: true` 字段。

**管理员专享功能**（仅当 `is_admin: true` 可见）：

- **⚙ 设置 → 用户管理** —— 新增用户、重置密码、切换 `disabled` 哨兵、把其他用户升降为管理员。
- **⚙ 设置 → 全局** —— 调度器 tick 间隔、最大并发尝试数（全局 / 每 server）、OSS 图片上传集成、归档清理周期、"从文件重新加载配置"。

**从 v0.3.0 之前的单库版本迁移。** 旧版看板（单一 DB 布局）第一次跑新二进制时，会触发一次性迁移：所有任务 / 标签 / Hermes server 全部归到 `admin` 名下、搬进 `data/admin/`。旧的 `data/db/taskboard.db` 在数据复制完之后会被删掉（AEAD 密钥 `data/.secret` 保留，运行时还要用）。**需要兜底就自己 `tar` 一份**，迁移过程不会自动保留副本。

### Hermes 侧配置

Hermes 自带的 gateway 提供了一个 OpenAI 兼容的 HTTP API，默认端口 **8642**。完整的上游文档：**[Hermes API Server 官方文档](https://hermes-agent.nousresearch.com/docs/user-guide/features/api-server)**。

开启方式三选一：

- **环境变量（临时使用）**：
  ```bash
  API_SERVER_ENABLED=true \
  API_SERVER_KEY=一个至少 8 位的强密钥 \
  API_SERVER_PORT=8642 \
  API_SERVER_HOST=127.0.0.1 \
    hermes gateway run
  ```
- **`~/.hermes/.env`（持久化）**：把上面四行写进去即可。
- **`~/.hermes/config.yaml` 的 `platforms.api_server` 段**：所有可配置字段详见 [API Server 官方文档](https://hermes-agent.nousresearch.com/docs/user-guide/features/api-server)。

验证：

```bash
curl -H "Authorization: Bearer 你的强密钥" http://127.0.0.1:8642/health/detailed
curl -H "Authorization: Bearer 你的强密钥" http://127.0.0.1:8642/v1/models
```

如果需要局域网里其他机器也能访问，把 host 改成 `0.0.0.0`，并且 `API_SERVER_KEY` 至少 8 位 —— Hermes 会拒绝"空密钥 + 公网绑定"这种危险组合。

### 从源码构建

```bash
git clone git@github.com:ahkimkoo/hermes-taskboard.git
cd hermes-taskboard
./build.sh                           # 当前平台
GOOS=linux GOARCH=arm64 ./build.sh   # 交叉编译
VERSION=v0.1.0 ./release.sh          # 一次性打齐所有平台到 dist/
```

`release.sh` 默认产出 linux/amd64、linux/arm64、darwin/amd64、darwin/arm64、windows/amd64 五种压缩包，每个包里都包含可执行文件、`config.example.yaml`、空的 `data/` 骨架目录（`db/`、`task/`、`attempt/`），以及 `README.md`、`CHANGELOG.md`、`LICENSE`。

### Docker 部署

Docker Hub 上有现成的镜像：**`successage/hermes-taskboard`**，直接拉来跑：

```bash
docker run -d --name taskboard \
  -p 1900:1900 \
  -v taskboard-data:/data \
  --add-host=host.docker.internal:host-gateway \
  successage/hermes-taskboard:latest
```

或者从源码构建：

```bash
docker build -t hermes-taskboard:dev .
docker run -d --name taskboard \
  -p 1900:1900 \
  -v "$PWD/tb-data:/data" \
  --add-host=host.docker.internal:host-gateway \
  hermes-taskboard:dev
```

#### docker compose

把宿主机的 `./taskboard-data` 目录映射到容器里的 `/data` —— 这样所有数据都落在你自己的文件夹,备份 / 迁移 / 清理都直接在宿主机操作即可。把下面的 `docker-compose.yml` 放到任意目录,`docker compose up -d` 就行:

```yaml
services:
  taskboard:
    image: successage/hermes-taskboard:latest
    container_name: taskboard
    ports:
      - "1900:1900"
    environment:
      TZ: Asia/Shanghai                   # 定时任务的 cron 按这个时区解释（默认 UTC）
    volumes:
      - ./taskboard-data:/data            # 宿主机目录 → 容器内的 /data
    extra_hosts:
      - "host.docker.internal:host-gateway"  # 让容器能访问到宿主机上的 Hermes
    restart: unless-stopped
```

> **时区说明**：镜像默认 `TZ=UTC`，所以 `0 9 * * *` 默认是「每天 UTC 9 点 = 北京 17 点」。把上面的 `TZ` 改成你所在的时区（东八区是 `Asia/Shanghai`）才会按当地的 9 点执行。

首次启动 —— 宿主机上只需要建个空目录就够了:

```bash
mkdir -p taskboard-data
docker compose up -d
```

容器启动时以 root 身份先把挂进来的 `/data` chown 给内部的 `taskboard` 用户(UID 1000),然后通过 `su-exec` 降权运行 —— 宿主机上不需要 `sudo chown`。

之后所有东西 —— `data/config.yaml`、各用户目录、SQLite 数据库、attempt 事件日志 —— 都落在 `./taskboard-data/` 里。清掉某个用户只需 `rm -rf taskboard-data/{用户名}/`(参见 [多用户支持(目录级别可插拔)](#多用户支持目录级别可插拔) 一节);整体清空则 `docker compose down && rm -rf taskboard-data`。

升级时 pull + 重建即可:

```bash
docker compose pull
docker compose up -d
```

重启不会破坏 per-user 布局。如果你是从 v0.3.0 之前的单 DB 版本升级过来,首次启动会跑一次性迁移,把全部内容挪到 `data/admin/` 下,旧 DB 被删除。需要安全兜底请在升级前手动 `tar czf taskboard-data.backup.tar.gz taskboard-data` 存一份。

#### 让 taskboard 连接 Hermes

在设置里把 Hermes Server 的 `base_url` 填成 `http://host.docker.internal:8642` —— 因为 Hermes 跑在宿主机上,不在容器里。

> **安全提示**：当 taskboard 和 Hermes 跑在同一个 OS 用户下（典型本地开发场景）时，Hermes agent 通过 `terminal` / `execute_code` 工具有读取 `data/{username}/db/taskboard.db` 的能力。API key 是加密存的，但任务描述和标签 system prompt 是明文。生产环境请把 Hermes 隔离到独立容器/独立用户,使其无法访问 taskboard 的数据目录。

### 架构概览

```
浏览器 (Vue 3)               Hermes Task Board (Go)            Hermes Gateway
-----------------            ---------------------------       ------------------
看板视图         ────SSE───► HTTP + SSE hub
执行面板         ────HTTP──► Board 状态机
                             │
                             ▼
                             调度器  ─┐
                             AttemptRunner ────HTTP + SSE────► /v1/responses
                             │                                 /v1/runs/{id}/events
                             ▼
                             SQLite + data/{task,attempt}/     model: hermes-agent
```

- **状态机**：`draft → plan → execute → verify → done → archive`。只有 `plan → execute`、`execute → verify`、`verify → execute` 是后端自动迁移，其余全部由用户拖拽触发。
- **1 Attempt = 1 Hermes conversation**：验证阶段追问并不新开 Attempt，只是在同一 conversation 下起一个新的 `run_id`。
- **三级并发闸门**：全局 / 单个 Server / `(server, model)` 三层上限，默认 50 / 10 / 5。

### 配置热加载

所有可配置项 —— 账号密码、已注册的 Hermes Servers、调度参数、偏好设置 —— 全都写在 `data/config.yaml` 里。可以直接 `vim` 改，改完在设置页点 **从文件重新加载配置**（或 `POST /api/config/reload`）即可生效，不用重启进程。

### 开发

```bash
go build ./...
go build -o bin/tb ./cmd/taskboard
./bin/tb -data ./data    # 前端通过 go:embed 打包进二进制，不用单独构建
```

前端源文件放在 `internal/webfs/web/`，没有 Vite / webpack 之类的构建链路，改完 `.js`/`.css`/`.html` 需要重编 Go 二进制。

### 测试

- `go build ./...`：类型 / 静态检查
- `./docs/smoke.sh`：后端 API 冒烟（创建 → 迁移 → 删除 一轮）
- **`tests/ui_test.py`**：Playwright UI 回归套件。逐条覆盖 CHANGELOG 里列的改动：拖拽手感、卡片排序、主题切换、中英纯净切换、设置窗反复打开、尝试列表折叠、归档才能删除、富文本编辑器、列副标题……每次改完界面都应该跑一下：
  ```bash
  pip install playwright && playwright install chromium
  TASKBOARD=http://127.0.0.1:1900 python3 tests/ui_test.py
  ```
  每个 case 都会先 `page.goto` 一次，保证不同 case 之间没有状态残留。

### License

MIT.
