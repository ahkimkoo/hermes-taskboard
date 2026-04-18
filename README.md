# Hermes Task Board

> A Trello-style kanban for driving [Hermes Agent](https://github.com/hermes-agent) — define tasks, dispatch them to one or more Hermes sessions, watch the agent think + tool-call in real time, then verify and archive.
>
> **Single Go binary · SQLite + filesystem · Vue 3 (no build step) · PWA · i18n (中 / EN)**

[English](#english) · [简体中文](#简体中文)

![Board view](docs/screenshots/board-en.png)

---

## English

### What it does

Hermes Agent executes tools, edits files, and runs shell commands. This project gives you a 6-column kanban to:

1. **Define** tasks (markdown description, tags, priority, dependencies, preferred Hermes server + model).
2. **Dispatch** each task to a Hermes conversation as one or more parallel **Attempts** (auto-trigger via scheduler or click *Start*).
3. **Watch** Hermes think and tool-call live — NDJSON event stream → SSE → browser.
4. **Verify** the result, ask follow-up questions (still the same session), then move to Done / Archive.

### Screenshots

| | |
|---|---|
| ![Board EN](docs/screenshots/board-en.png) | ![Board 中文](docs/screenshots/board-zh.png) |
| 6-column desktop board, English | Same board, switched to 中文 live |
| ![Attempt live](docs/screenshots/attempt-live.png) | ![Settings](docs/screenshots/settings-servers.png) |
| Live SSE stream during a real Hermes attempt | Hermes Servers settings page |
| ![Mobile](docs/screenshots/board-mobile.png) | ![Login](docs/screenshots/login.png) |
| Phone: one column + swipe-tab per status | Optional password login page |

### Quick start for end users (download a release)

```bash
# 1. Grab the binary for your platform from the GitHub releases page
#    https://github.com/ahkimkoo/hermes-taskboard/releases
curl -LO https://github.com/ahkimkoo/hermes-taskboard/releases/latest/download/hermes-taskboard-v0.1.0-linux-amd64.tar.gz
tar -xzf hermes-taskboard-v0.1.0-linux-amd64.tar.gz
cd hermes-taskboard-v0.1.0-linux-amd64

# 2. (Re)start your Hermes gateway with the API server enabled
API_SERVER_ENABLED=true API_SERVER_KEY=your-strong-key hermes gateway run

# 3. Start the board
./hermes-taskboard -data ./data
# → open http://127.0.0.1:1900 in your browser
```

On first visit: click **⚙ Settings → Hermes Servers → New server**, point it at `http://127.0.0.1:8642` and paste the same `API_SERVER_KEY`. Hit **Test Connection**. Done — create a task and click **▶ Start**.

### Set up Hermes for this board

Hermes (gateway) exposes an OpenAI-compatible HTTP API at port **8642**. Enable it with either:

- **env vars** (one-off):
  ```bash
  API_SERVER_ENABLED=true \
  API_SERVER_KEY=choose-a-strong-key \
  API_SERVER_PORT=8642 \
  API_SERVER_HOST=127.0.0.1 \
    hermes gateway run
  ```
- **`~/.hermes/.env`** (persistent): add the same four lines without the trailing command.
- **`~/.hermes/config.yaml`** → `platforms.api_server` block (see Hermes docs for the precise schema).

Confirm it's up:

```bash
curl -H "Authorization: Bearer your-strong-key" http://127.0.0.1:8642/health/detailed
curl -H "Authorization: Bearer your-strong-key" http://127.0.0.1:8642/v1/models
```

If you want to expose the API to your LAN, set `API_SERVER_HOST=0.0.0.0` **and** give Hermes a ≥8-char key — it will refuse network binds otherwise.

### Build from source

```bash
git clone git@github.com:ahkimkoo/hermes-taskboard.git
cd hermes-taskboard
./build.sh                       # host platform
GOOS=linux GOARCH=arm64 ./build.sh  # cross-compile
VERSION=v0.1.0 ./release.sh      # cross-platform tarballs under dist/
```

`release.sh` builds linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64 by default, each archive shipping the binary, `config.example.yaml`, the empty `data/` skeleton, and `README.md` + `CHANGELOG.md` + `LICENSE`.

### Docker

```bash
# Build
docker build -t hermes-taskboard:dev .

# Run (persist state to a host folder)
docker run -d --name taskboard \
  -p 1900:1900 \
  -v "$PWD/tb-data:/data" \
  --add-host=host.docker.internal:host-gateway \
  hermes-taskboard:dev
```

Then in the app's **Settings → Hermes Servers** use `http://host.docker.internal:8642` (since Hermes runs on the host, not inside the container).

### Architecture

```
Browser (Vue 3)              Hermes Task Board (Go)             Hermes Gateway
-----------------            ---------------------------        ------------------
Kanban view    ────SSE──────► HTTP + SSE hub
Execute modal  ────HTTP─────► Board service (state machine)
                              │
                              ▼
                              Scheduler ──┐
                              AttemptRunner ─────HTTP + SSE───► /v1/responses
                              │                                  /v1/runs/{id}/events
                              ▼
                              SQLite + data/{task,attempt}/      model: hermes-agent
```

- **State machine**: `draft → plan → execute → verify → done → archive`. Only `plan → execute`, `execute → verify`, and `verify → execute` are auto-transitions; everything else is a user drag.
- **1 Attempt = 1 Hermes conversation**. Verify-stage follow-ups stay in the same conversation — they just spawn a new `run_id`.
- **Concurrency** is gated at 3 levels: global, server, and (server, model) profile. Defaults: 50 / 10 / 5.

### Config hot-reload

Everything configurable lives in `data/config.yaml`. Edit it by hand, then either hit *Reload config from file* in the Settings page or `POST /api/config/reload` — no restart.

### Development

```bash
go build ./...          # type-check
go build -o bin/tb ./cmd/taskboard
./bin/tb -data ./data   # dev run (frontend is embedded via go:embed)
```

Frontend lives at `internal/webfs/web/` and is served directly out of the binary. Editing `.js/.css/.html` in that folder needs a rebuild; there's no Vite / Rollup.

### Testing

- `go build ./...` — static checks.
- Manual API smoke test: `./docs/smoke.sh` (create → transition → delete cycle).
- Browser tests: open the UI, create a task, click Start, watch events stream. (`playwright` screenshots under `docs/screenshots/` were generated live against a real Hermes instance.)

### License

MIT.

---

## 简体中文

### 这是什么

Hermes Agent 能跑工具、改文件、执行 shell 命令。本项目给它加一个 6 列看板：

1. **定义**任务（markdown 描述、标签、优先级、依赖、指定 Hermes server + model）。
2. **分派**到 Hermes：每张卡可以并行开多个 **Attempt**（调度器自动触发，或手动点 *Start*）。
3. **实时观察**：Hermes 的思考、工具调用流、最终回复 —— NDJSON → SSE → 浏览器。
4. **验证**结果，在同一个 Session 里追问，然后拖到 Done / Archive。

### 截图

| | |
|---|---|
| ![看板 English](docs/screenshots/board-en.png) | ![看板 中文](docs/screenshots/board-zh.png) |
| 桌面 6 列看板 · English | 同一界面实时切到中文 |
| ![执行面板](docs/screenshots/attempt-live.png) | ![设置](docs/screenshots/settings-servers.png) |
| 真实 Hermes 调用的 SSE 流 | Hermes Servers 设置页 |
| ![移动端](docs/screenshots/board-mobile.png) | ![登录](docs/screenshots/login.png) |
| 手机端单列 + 顶栏状态 tab | 可选开启的账号密码登录页 |

### 普通用户快速上手（下载 release 包运行）

```bash
# 1. 从 GitHub Releases 下载对应平台的压缩包：
#    https://github.com/ahkimkoo/hermes-taskboard/releases
curl -LO https://github.com/ahkimkoo/hermes-taskboard/releases/latest/download/hermes-taskboard-v0.1.0-linux-amd64.tar.gz
tar -xzf hermes-taskboard-v0.1.0-linux-amd64.tar.gz
cd hermes-taskboard-v0.1.0-linux-amd64

# 2. 带上 API_SERVER_KEY 启动你的 Hermes gateway
API_SERVER_ENABLED=true API_SERVER_KEY=你的强密钥 hermes gateway run

# 3. 启动本看板
./hermes-taskboard -data ./data
# 打开浏览器：http://127.0.0.1:1900
```

首次进页面：**⚙ 设置 → Hermes Servers → New server**，把 `base_url` 填 `http://127.0.0.1:8642`，`api_key` 填你刚才那个强密钥，点 **测试连接** 通过即可。回看板，新建一张任务，点 **▶ 开始**。

### Hermes 侧配置（必读）

Hermes 的 gateway 自带一个 OpenAI 兼容的 HTTP API（默认端口 **8642**）。有三种启用方式，三选一：

- **环境变量（临时）**：
  ```bash
  API_SERVER_ENABLED=true \
  API_SERVER_KEY=一个至少 8 位的强密钥 \
  API_SERVER_PORT=8642 \
  API_SERVER_HOST=127.0.0.1 \
    hermes gateway run
  ```
- **`~/.hermes/.env`（持久）**：把上面四行放进去。
- **`~/.hermes/config.yaml` 的 `platforms.api_server` 段**（具体字段见 Hermes 官方文档）。

验证：

```bash
curl -H "Authorization: Bearer 你的强密钥" http://127.0.0.1:8642/health/detailed
curl -H "Authorization: Bearer 你的强密钥" http://127.0.0.1:8642/v1/models
```

如果要让局域网里的其他机器访问，把 host 改成 `0.0.0.0`，同时 `API_SERVER_KEY` 必须 ≥8 位 —— Hermes 会自己拒绝空密钥 + 公网绑定的组合。

### 从源码构建

```bash
git clone git@github.com:ahkimkoo/hermes-taskboard.git
cd hermes-taskboard
./build.sh                        # 当前平台
GOOS=linux GOARCH=arm64 ./build.sh   # 交叉编译
VERSION=v0.1.0 ./release.sh       # 一次打所有平台的 release 包到 dist/
```

`release.sh` 默认打 linux/amd64、linux/arm64、darwin/amd64、darwin/arm64、windows/amd64 五种，每个压缩包里都包含：可执行文件、`config.example.yaml`、空的 `data/` 骨架目录、`README.md`、`CHANGELOG.md`、`LICENSE`。

### Docker 方式

```bash
# 打镜像
docker build -t hermes-taskboard:dev .

# 运行（数据挂到宿主机目录）
docker run -d --name taskboard \
  -p 1900:1900 \
  -v "$PWD/tb-data:/data" \
  --add-host=host.docker.internal:host-gateway \
  hermes-taskboard:dev
```

然后在设置里 Hermes Server 的 `base_url` 填 `http://host.docker.internal:8642`（因为 Hermes 跑在宿主机，不在容器里）。

### 架构概览

```
浏览器 (Vue 3)               Hermes Task Board (Go)            Hermes Gateway
-----------------            ---------------------------       ------------------
看板视图        ────SSE────► HTTP + SSE hub
执行面板        ────HTTP───► Board 状态机
                             │
                             ▼
                             Scheduler ─┐
                             AttemptRunner ────HTTP + SSE────► /v1/responses
                             │                                 /v1/runs/{id}/events
                             ▼
                             SQLite + data/{task,attempt}/     model: hermes-agent
```

- **状态机**：`draft → plan → execute → verify → done → archive`。只有 `plan → execute`、`execute → verify`、`verify → execute` 三条是后端自动迁移，其余全部由用户拖拽触发。
- **1 Attempt = 1 Hermes conversation**：在 Verify 阶段追问不开新 Attempt，只在同一 conversation 下起一个新 `run_id`。
- **并发三级闸门**：全局 / server 级 / (server, model) 级，默认 50 / 10 / 5。

### 配置热加载

所有配置都在 `data/config.yaml`。你可以 `vim` 直接改，然后在设置页顶部点 **从文件重新加载配置**（或 `POST /api/config/reload`），不用重启进程。

### 开发

```bash
go build ./...
go build -o bin/tb ./cmd/taskboard
./bin/tb -data ./data    # 前端通过 go:embed 打包，直接用就行
```

前端源文件在 `internal/webfs/web/`，没有 webpack/Vite，改完 `.js/.css/.html` 需要重编 Go 二进制。

### 测试

- `go build ./...`：类型/静态检查
- API 冒烟：`./docs/smoke.sh`（创建 → 迁移 → 删除）
- 浏览器冒烟：打开 UI → 新建任务 → 点 Start → 看 SSE 流。`docs/screenshots/` 下的截图都是用 playwright 对真实 Hermes 实例跑出来的。

### License

MIT.
