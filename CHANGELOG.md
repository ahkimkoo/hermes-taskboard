# Changelog

All notable changes are tracked here, grouped by date.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## 2026-04-18

### Docs (later)
- `README.md` 中英两个版本各新增 **项目初衷 / Why this exists** 段落：说明 Hermes 的自我进化能力与"数字伙伴"定位、列出它支持的 IM 网关（Telegram / Discord / Slack / WhatsApp / 微信 / 飞书 / 钉钉 / QQ…），并阐明 IM-first 使用方式的天花板（只能一次一件、切 session / profile 不方便），从而论证本项目"让 Hermes 能按 backlog 批量 / 并行工作"的价值。

### Docs
- `docs/requirements.md` 升到 v0.2：在 §4.8.1 / §4.8.5 开头各加了一个"契约"引言块，把两条关键规则提升为明显红线：
  1. 每个接入的 Hermes Server 必须配置 Server 级并发上限（默认 10），每个 profile（如 `hermes-agent`）再配置自己的并发上限（**默认 5**），任一层级超限即拒绝新 Attempt。
  2. 所有系统设置（账号密码、Hermes Servers、各类开关）统一存 `data/config.yaml`；启动时读入内存、修改时先刷内存再原子写回、设置页必须提供"从文件重新加载配置"按钮（`POST /api/config/reload`），支持手改 YAML 后热刷新免重启。
- 文档顶部新增修订历史段。

### Added
- Initial implementation of the Hermes Task Board.
- Single Go binary with embedded Vue 3 frontend (`go:embed`) — no separate build step for the web app.
- SQLite + filesystem storage (`data/db/taskboard.db`, `data/task/*.json`, `data/attempt/{id}/events.ndjson`).
- YAML config with hot-reload (`POST /api/config/reload`) and AES-GCM-encrypted Hermes API keys at rest.
- Kanban with 6 fixed columns (`draft → plan → execute → verify → done → archive`), HTML5 drag-and-drop, priority P1–P5, tags, dependencies.
- Task state machine; only `plan → execute`, `execute → verify`, and `verify → execute` are backend-auto transitions.
- Scheduler: every 5 s (configurable) scans `plan + auto + deps-done` tasks, respects 3 concurrency gates (global / server / (server, model)).
- `AttemptRunner` with 1:1 mapping of Attempt ↔ Hermes conversation; handles message queueing, re-entry after Verify follow-ups, and SSE stream consumption.
- Hermes client (`internal/hermes`): `CreateResponse`, `StreamRunEvents`, `CancelRun`, `Health`, `Models`. Pool rebuild on config reload.
- REST API for tasks, attempts, tags, Hermes servers (CRUD + test connection + model import), preferences, settings, auth.
- SSE channels: `/api/stream/board` and `/api/stream/attempt/{id}` with `Last-Event-ID` resume from the on-disk NDJSON.
- Optional username/password login (bcrypt, HMAC-signed cookie). Off by default; enable via Settings → Access control.
- i18n: English + Simplified Chinese, switchable live; strings loaded from `/locales/*.json`.
- PWA: manifest + service worker with app-shell cache; network-first for API/SSE.
- Sound cues via Web Audio (`execute_start`, `needs_input`, `done`) — no audio asset shipping required.
- Responsive layouts: 6-column (≥1200 px), 3-column scroll (768–1199 px), single-column with top-tabs (<768 px).
- `build.sh`, `release.sh` (cross-platform tarballs + checksums), and a distroless `Dockerfile`.
- Screenshots captured against a live Hermes instance running on the same host (`docs/screenshots/`).
- Bilingual (English / 中文) README and this CHANGELOG.

### Known limitations
- No multi-user or RBAC (single user by design for v1).
- Tool-call `approval_required` events are surfaced but not interactively approved in the UI — they render as system events for now.
- Scheduler server-health short-circuit uses a 30 s cache; if the Hermes server goes down mid-tick you may see a short stream of failed attempts until the cache expires.
- `archive.auto_purge_days` is read by the scheduler config but the reaper goroutine is stubbed — files currently accumulate until you delete tasks manually.
