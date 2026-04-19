# Changelog

All notable changes are tracked here, grouped by date.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## 2026-04-19

### Major round 7 — SSE fix, UX polish, tag prompts, scheduled tasks

**Autorefresh bug (was silent)**
Discovered and fixed an old SSE wiring bug: `writeSSE` on the backend always emitted `event: <name>` frames, but the frontend's `EventSource` only listened on `onmessage` (which doesn't fire on named events). Result: every board-level event — task.moved, attempt.created, attempt.state_changed, preferences_updated — was being silently dropped, and the board only refreshed when the user reloaded. Fix: strip the `event:` header and merge the event name into the JSON payload so everything arrives via `onmessage`. After the fix, the Verify → Execute transition (triggered automatically when the user sends a follow-up in Verify) now moves the card across columns in real time, and every other state change propagates without a reload.

**Card animations**
The Verify / needs-input animation is no longer a chase — it's now a slow gold ↔ warm-white *breathing* border (3.5 s ease-in-out), which reads as "paused, waiting" rather than "urgent running". Execute-column Running cards keep their electric green+red chase, so the two states are clearly distinct at a glance.

**Event stream: chat-style autoscroll**
EventStream now tracks whether the user is pinned to the bottom. While pinned, new output (streaming tokens, tool calls) auto-scrolls into view. If the user scrolls up to read history, the stream stops dragging them down and surfaces a "↓ new messages" pill at the bottom of the pane; clicking it (or scrolling back to the bottom) re-arms auto-scroll.

**Chat input**
Send is now **Ctrl/⌘ + Enter** instead of plain Enter, with a small hint ("Ctrl/⌘ + Enter to send") underneath the input — plain Enter can now be used to break lines without accidentally submitting. **Stop** is a two-click confirm: first click arms "Confirm stop?", second click actually cancels; auto-resets after 4 s if ignored.

**? help popover on Attempts heading**
A small `?` button next to "Attempts" pops a one-paragraph explanation that "Attempt = one execution; a task can be re-run but usually once is enough; send a message to continue an existing Attempt".

**Tag System Prompts**
New `tags.system_prompt` column (idempotent migration). Settings → **Tags** tab lets users maintain tags directly, including an optional `System Prompt` textarea. When a task is dispatched, every tag's system prompt is concatenated onto the base persona passed to Hermes's `/v1/responses` call. Use case from the requirement: a `notify-qq` tag with prompt "When this task finishes, post a short summary to QQ" — any task tagged this way automatically inherits that instruction. Multiple tags stack in order.

**Scheduled tasks (cron + interval)**
New `task_schedules` table + `internal/cron` worker (separate goroutine, ticks every 5 s):
- **Interval** kind: standard `time.ParseDuration` spec — `15m`, `2h`, `1h30m` — at least 10 s. Fires again N after the previous fire.
- **Cron** kind: standard 5-field `min hour dom month dow` (robfig/cron/v3).
- Any number of schedules per task, each independently enabled/disabled.
- On fire, creates a fresh Attempt via the normal Runner (so concurrency gates + tag prompts all apply).
- `POST /api/tasks/{id}/schedules` to create, `PATCH /api/schedules/{id}` to toggle enabled, `DELETE /api/schedules/{id}` to remove.
- New `SchedulePicker` component renders inside the task modal showing kind, spec, next fire, on/off toggle.

**Tests**
5 new Playwright cases (`test_tag_system_prompt`, `test_schedule_roundtrip`, `test_schedule_picker_ui`, `test_input_hints`, `test_attempt_help_popover`). Suite is **29/29** green.

### Polish (round 6) — animated card borders
- **Running tasks (Execute column)** now carry an **electric green+red "chase" border**: two narrow arcs of green→red gradient rotate around the card's perimeter at 3 s/revolution, with transparent gaps between them so the chase reads clearly.
- **Verify / needs-input tasks** get the **same chase, but in orange+red** — signalling the card wants your attention without shouting as loud as an alert.
- Implemented with `conic-gradient(from var(--glow-angle), transparent, color, color, transparent, …)` painted into a 2-px transparent border via `background-clip: padding-box, border-box`. `--glow-angle` is registered via `@property` so the browser can interpolate it smoothly at 60 fps.
- Backend: `tasks` API now exposes `needs_input_attempts` alongside `active_attempts`, so the frontend can tell running-but-OK apart from running-but-blocked-on-input.
- `prefers-reduced-motion: reduce` drops back to a static coloured border.
- New regression test asserts Verify cards receive `.needs-input`, Execute cards with active attempts receive `.running`, and no card has both; computed `animationName` / `animationDuration` are non-zero. Suite is **24/24** green.

### Fixes (round 5)
- **Attempt list toggle now actually toggles**. Previous logic was `attemptListVisible = listOpen || attempts.length > 1`, so once a task had 2+ attempts the list stayed permanently visible no matter how many times you clicked Hide. Now a single `listOpen` flag drives visibility; it defaults to open when `attempts.length > 1` and false otherwise, and the toggle button shows whenever there is at least one attempt.
- **Sound preview buttons** next to each event toggle in Settings → Preferences. The ▶ button plays the corresponding tone regardless of whether that specific event is enabled (so you can still audition a sound before deciding to turn it on), using the current volume draft.
- Two new Playwright cases; suite is 23/23 green.

### UX additions (round 4)
- **Image upload now requires an image host**: verified by reading Hermes's `gateway/platforms/api_server.py` that the server forwards `input` text verbatim to the upstream LLM (DashScope, etc.) and silently drops `image_url` content parts. Since the LLM can't reach `http://127.0.0.1:1900/uploads/*`, local storage is useless in any realistic setup. The Insert image button and paste/drop handlers are now hidden unless Aliyun OSS is configured; `POST /api/uploads` returns `503 image_upload_disabled` in the same case. A helpful hint underneath the description editor explains why and points to Settings → Integrations.
- **Task modals no longer close on overlay click**: the task-open and new-task modals now only close via the explicit × in the header (or Cancel for new-task). Accidentally clicking the dimmed area around the modal while editing a long description no longer discards the whole thing. Settings and confirmation modals keep their existing overlay-click behaviour since they don't risk losing user input.
- Tests: 3 new cases (`test_uploads_gated`, `test_task_modal_overlay_noclose`, `test_new_task_overlay_noclose`); existing `test_editor_controls` now asserts the Insert-image button is absent without OSS. Suite is 21/21 green.

### UX additions (round 3)
- **Tag input**: tags are now a chip-based control with autocomplete backed by the `tags` table. Every tag ever used on any task becomes a suggestion for future tasks. Commit via Enter, `,`, or Tab; remove via chip-× or backspace on an empty input. (New `TagInput` component.)
- **Dependencies**: new-task / edit-task forms now have a **Depends on** picker. Each dependency is `{task_id, required_state}` where state is either **Verify** (start once the target's attempts finished, even if the user hasn't accepted yet) or **Done** (wait for human acceptance). Scheduler's `AllDependenciesDone` honours the state per edge. (New `DependencyPicker` component, schema migration for `task_deps.required_state`.)
- **Required vs optional** — forms now show a red ★ after **Title** and an inline `(optional)` marker after every other label. Title is the only required field.
- API: create/patch `/api/tasks` accept dependencies as either `["id", ...]` (legacy, implicit `done` gate) or `[{task_id, required_state}, ...]`; the backend normalises and stores in the richer shape.
- Tests: 3 new Playwright cases — tag-input autocomplete + chip remove; dependency-picker round-trip with `required_state=verify`; optional-marker audit on the new-task form. Suite is 18/18 green.

### UX overhaul (round 2)
- **Drag & drop** rewritten on top of pointer events: the source card hides with a dotted placeholder, a styled floating clone follows the cursor, and the drop commits to an exact slot (between neighbours, not just "the column"). Feels more like Trello, far less like a browser HTML5 drag-ghost. (Requirement #1)
- **Task ordering is now user-controlled**: added a `position` column to the `tasks` table with an automatic migration for existing DBs (positions seeded from `created_at`). New tasks land at the end of the Draft column; drag-to-reorder persists and survives reloads. The list API no longer sorts by priority — it returns rows by `(status, position)` and the client simply iterates. (Requirements #7, #8)
- **Rich description editor**: title is required, body is optional Markdown with a Write/Preview toggle. Paste, drop, or pick images — they upload via `POST /api/uploads` and a Markdown image reference is spliced at the caret. (Requirement #2)
- **Image hosting**: new `internal/uploads` package. Local disk by default (`data/uploads/`, served at `/uploads/{name}`), or Aliyun OSS if configured. OSS credentials (`oss.access_key_id` + `access_key_secret`) live in `data/config.yaml`; the secret is AES-GCM-encrypted at rest alongside Hermes API keys. Settings page gains an "Integrations" tab.
- **Attempt list**: now shows local-formatted start time + short ID per attempt; collapses to a single-pane layout when there's only one (or zero) attempts; the "+ New Attempt" button shrank and now gates behind a confirmation dialog explaining that a new attempt consumes a separate concurrency slot. (Requirement #3)
- **Event stream is now semantic** — each Hermes event is grouped into a user message, an assistant bubble (with Markdown rendering), or a collapsible tool-call card showing name / args / output. No more raw JSON dumps. (Requirement #4)
- **Light/dark theme toggle** in the top bar (☾/☀), persisted to `preferences.theme` in `data/config.yaml`. A full light-theme palette is defined in CSS variables. (Requirement #5)
- **Delete gating**: the *Delete task* button only appears when a card sits in the Archive column; clicking once reveals a second "Confirm delete?" button. (Requirement #6)
- **Column subtitles**: each of the six columns now has a small gray one-liner explaining its meaning (e.g. Plan → "Queued and ready for execution." / 计划 → "排队准备执行"). (Requirement #10)
- **Settings page**: now includes an explicit helper paragraph under *Models* explaining that each row corresponds to a Hermes agent profile (same thing the Hermes API calls "model"). (Requirement #11)
- **Settings modal reopen bug** — fixed. Now always goes through a `showSettings = false → true` transition to avoid a stale-state window where the second click was a no-op. (Requirement #12)
- **i18n rewritten to be reactive** (Vue ref) — no more language-mixing after toggle. The `$t(key)` lookup consistently resolves against exactly one dictionary. Missing keys fall back to English, never to a leftover Chinese string. (Requirement #9)

### Behind the scenes
- JSON tags on all `config.Preferences`/`Sound`/`Scheduler`/`Archive`/`Server`/`OSS` struct fields, so the API now returns `{ language, theme, sound: {…} }` rather than Pascal-cased keys. This was a silent API break for the frontend; fixed together here.
- `POST /api/tasks/{id}/transition` accepts `after_id` / `before_id` to request a specific drop slot; the backend computes a new `position` mid-way between neighbours and renumbers the column if needed to recover from collisions.
- New module layout on the frontend: `i18n.js`, `markdown.js`, `drag.js`, `description-editor.js`, `event-stream.js` are now their own files imported by `app.js`.
- New Playwright suite `tests/ui_test.py` with 15 cases — run it any time after a UI change.

### Docs (later)
- 在 `README.md` 的 "Set up Hermes for this board / Hermes 侧配置" 两节各加上 Hermes 官方 API Server 文档直链：<https://hermes-agent.nousresearch.com/docs/user-guide/features/api-server>。用户读完本项目的最小化配置说明后，可以直接跳到上游文档查所有可配置字段。
- 重写 `README.md`，让中英两个版本各自读起来像母语原生写的文档而不是相互的对照翻译：
  - 英文段落里不再混入中文字符（之前把 "微信/飞书/钉钉/QQ" 直接塞进了英文段，现在换成 `WeChat / Feishu (Lark) / DingTalk / QQ`）。
  - 中文段落减少生硬的 English 术语，改用地道表达（如 "backlog" → "待办清单"、"IM-first" → "聊天驱动的工作流"）。
  - 截图 caption、小节标题也按各自语言的习惯顺一遍。
- Hermes 链接更正为 `https://github.com/NousResearch/hermes-agent`（之前指向不存在的 `https://github.com/hermes-agent`）。
- 顶部 tagline 精简，去掉看起来奇怪的 `i18n (中 / EN)`。
- `README.md` 中英两个版本各新增 **项目初衷 / Why this exists** 段落（同日早先）：说明 Hermes 的自我进化能力与"数字伙伴"定位、列出它支持的消息网关，并阐明聊天驱动工作流的瓶颈。

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
