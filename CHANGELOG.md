# Changelog

All notable changes are tracked here, grouped by date.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## 2026-04-24 — v0.3.8

### Tag files drop the YAML wrapper — contents are just the prompt

v0.3.7 wrote each tag as a small YAML file with `name:` / `color:` / `system_prompt:` keys. Overkill: the filename already encodes the name, color had no UI ever, and wrapping the prompt text in YAML made `cat tags/foo.public` harder to read than it needed to be.

v0.3.8 stores each tag as **just the raw system_prompt text**. File name = tag name (spaces → `-`) + `.private` / `.public`. File body = the prompt, verbatim. Nothing else. `Tag.Color` is gone from the Go struct + the frontend stopped sending it. Existing YAML-wrapped files from v0.3.7 need a one-liner to rewrite as plain text:

```bash
python3 -c '
import os, yaml
for fn in os.listdir("tags"):
    if not fn.endswith((".private",".public")): continue
    with open(f"tags/{fn}") as f: d = yaml.safe_load(f.read())
    if isinstance(d, dict) and "system_prompt" in d:
        with open(f"tags/{fn}","w") as f: f.write(d.get("system_prompt") or "")
'
```

(Run this once per user's `data/{username}/tags/` dir; harmless on already-plain files.)

## 2026-04-24 — v0.3.7

### Tags move out of `config.yaml` into per-file storage

Tag `system_prompt` bodies can run into hundreds of lines — keeping them inline in `data/{username}/config.yaml` made the file awkward to read. v0.3.7 gives each tag its own file:

```
data/admin/tags/
  企微通知.public            # shared (visible to every user)
  浏览器.private             # private
  Browser-Skill.public       # display name "Browser Skill" → filename "Browser-Skill"
```

- Filename: `{display-name-with-spaces-replaced-by-hyphens}.{private|public}`. The extension encodes visibility so `ls` tells you at a glance; spaces → `-` gives uniform basenames for shell globbing.
- File content: a tiny YAML carrying the authoritative `name:` (so round-tripping "Browser Skill" doesn't lose the space) + `color:` + `system_prompt:`.
- Operators can `cat data/admin/tags/企微通知.public` to see the prompt as raw text. Editing the file directly works for power-user tweaks — the next mutation through the UI rewrites it.

Legacy upgrade path: any user with a `tags: [...]` list still inline in their `config.yaml` gets pulled forward on boot (`MigrateAllInlineTags`), with individual files written under `tags/` and the YAML key dropped. Idempotent — re-runs don't clobber files already on disk.

### Global uniqueness enforced on `.public` tags

A shared tag name must be unique across every user. Create + rename + flip-to-public all check — if the name already exists as another user's public tag, the operation is refused with a specific error:

> a shared tag named "notes" already exists (owner: admin) — rename yours before making it public

Private tag names may still overlap freely — two users can each have their own `notes` as long as both stay private.

### Same rule for shared Hermes servers

Server IDs were previously always required to be globally unique. That was overly strict: private servers route through their owner's api_key regardless of id. v0.3.7 relaxes it to "only **shared** server ids must be globally unique". Trying to create / flip-to-shared an id already in use by another shared server returns 409 with an owner hint.

Also fixed a latent bug where `userdir.FindServer` would hit map-iteration order and pick another user's shared server over the viewer's own private one with the same id. Viewer's own row now always wins the lookup, so a flip-to-shared properly surfaces "id taken" against the collision.

### Frontend: tag editor now allows renaming

The tag name input was hard-disabled during edit. Now the input is free-form, and the frontend sends `old_name` alongside the new name so the server can remove the stale file + run the public-uniqueness gate. Same form works for pure content edits — if the name didn't change, server treats it as a no-op rename.

## 2026-04-24 — v0.3.6

### Legacy migration now carries `tags` across too

Earlier v0.3.x migration code only pulled `hermes_servers` out of the old global `config.yaml`. The central DB's `tags` table — which held each tag's name, color, and **system_prompt** — was never read, so tag prompts (e.g. the "notify me on QQ when finished" instruction attached to a tag) quietly disappeared when a pre-v0.3.0 install was upgraded. Fixed:

- New `readLegacyTags` step runs before admin's per-user config is written; rows are injected into `adminCfg.Tags` with `system_prompt` + `color` + `shared` preserved.
- Schema-aware: detects whether the legacy `tags` table has an `owner_id` / `shared` column (v0.3.0 era added them) and honours `shared` when present; defaults to `shared=false` on older schemas.
- Re-run safe: when `data/admin/config.yaml` already exists (e.g. migration fired on a previous boot then admin added more tags), the merge prefers the existing row on name collisions and appends new ones from the legacy DB underneath.

### Refuse to overwrite a corrupt per-user config

`userdir.LoadAll` silently skips any `data/{username}/config.yaml` that fails to parse. That was dangerous — `ensureDefaultAdmin` could then see "no admin in cache" and create a fresh one, **overwriting the unreadable file and destroying the real password hash, Hermes servers, and tags in the process.**

`userdir.Create` now checks disk before writing: if `data/{username}/config.yaml` already exists, it bails with a clear "refusing to overwrite — fix or remove the file first" error. The legacy-migration path has the same guard at admin's config write step. Operators see a loud startup failure instead of silent data loss.

## 2026-04-24 — v0.3.5

### Drop the per-user `id` — username is the sole identifier

`data/{username}/config.yaml` used to carry a UUID `id:` field alongside `username:`. Since the directory name is already the unique key — every subsystem that looks up a user (auth middleware, session cookie, API paths like `/api/users/{username}`, per-user store routing) keys off `username` — the separate id was dead weight. It also made operators glancing at the YAML ask "which one is the real identifier?".

v0.3.5 removes the `UserConfig.ID` field, `userdir.newID()`, and every code path that populated or preserved it. The view structs (`ServerView`, `TagView`) lose their `owner_id` field too; shared servers/tags still tell you who owns them via `owner_username`. The login + auth-status + list-users API responses no longer include the `user.id` key — callers that only read `username`/`is_admin` (which is what the bundled frontend does) are unaffected.

Existing per-user config files are silently forward-compatible: the `id:` key is ignored when loaded, and the next write (change password, add a server, etc.) rewrites the file without it. If you want to tidy your existing files in one pass, a stopped-server `sed -i '/^id: /d' data/*/config.yaml` does it cleanly — the binary doesn't care either way.

## 2026-04-24 — v0.3.4

### User creation lays down the full directory skeleton

Previously `userdir.Manager.Create` only wrote `data/{username}/config.yaml`, and the `db/`, `task/`, `attempt/` subdirectories appeared lazily on first use — `db/` when the scheduler opened the per-user store, `task/` when the user created their first task, `attempt/` when their first Attempt ran. That left `ls data/tony/` looking like a half-built workspace immediately after admin ran "Add user", which was confusing.

v0.3.4 creates all three subdirectories eagerly inside `Create`, so every user — admin on bootstrap, or any user invited via `POST /api/users` — lands with a complete `data/{username}/ {config.yaml, db/, task/, attempt/}` skeleton. The per-user SQLite file is still opened lazily by `store.Manager` (that hasn't changed); we just lay down the empty folders so operators see a consistent tree.

## 2026-04-24 — v0.3.3

### AEAD key moves to `data/.secret` — `data/db/` is gone

In the per-user layout, every SQLite database lives under `data/{username}/db/taskboard.db`. The top-level `data/db/` directory served no purpose except to hold the AEAD master key (`.secret`), which was easy to mistake for "where the database is" and caused the reported `load secret: mkdir /data/db: permission denied` error because the binary was still trying to create that legacy directory on boot.

v0.3.3 relocates the key:

- **New location:** `data/.secret` (top-level dotfile, same 0600 perms).
- **Idempotent auto-migration:** on every boot, if `data/db/.secret` exists and `data/.secret` doesn't, the binary renames it and rmdir's the now-empty `data/db/`. A fresh install never creates `data/db/` at all.
- **Legacy migration updated:** the one-shot migration that turns a pre-v0.3.0 install into the per-user layout no longer has a special "keep `data/db/.secret`" clause — the relocation step handles it, and migration now wipes the whole `data/db/` dir when it finishes.

Post-upgrade tree on a clean install:

```
data/
  .secret                  # AEAD key (was data/db/.secret)
  config.yaml              # global config
  admin/
    config.yaml
    db/taskboard.db        # per-user DB
```

README + operator manual updated in both languages to reference the new `data/.secret` path.

## 2026-04-24 — v0.3.2

### Docker: bind-mounted `/data` now works without a host-side `chown`

v0.3.0 / v0.3.1 shipped a distroless image that ran as UID 65532 and required operators to `sudo chown 65532:65532 taskboard-data` on the host folder before `docker compose up`. Anyone who skipped that step hit:

```
taskboard  | level=ERROR msg="load config" err="load secret: mkdir /data/db: permission denied"
```

Switched the final image to `alpine:3.20` + `su-exec` + `tini`, and added a tiny `docker-entrypoint.sh` that runs at container start:

1. Starts as root just long enough to `chown -R taskboard:taskboard /data` (uid/gid 1000 inside the container).
2. `exec`s the Go binary via `su-exec` so the server actually runs unprivileged.
3. `tini` sits at PID 1 so `docker compose down` gets a clean shutdown.

Operators only need `mkdir taskboard-data && docker compose up -d` now — no `sudo`, no pre-flight chown. The `chown` on an already-fixed folder is a no-op, so restarts are cheap. README (EN + ZH) updated to remove the old chown step.

## 2026-04-24 — v0.3.1

### Legacy migration no longer leaves an archive directory

v0.3.0's migration path moved the old central DB into `data/_migrated-YYYYMMDD-HHMMSS/db/` as a safety copy. In practice operators found the leftover `taskboard.db` sitting next to `data/admin/db/taskboard.db` confusing — easy to mistake for the live database. v0.3.1 removes the legacy DB files after the rows have been copied into admin's per-user store. The AEAD key at `data/.secret` stays put because it's still the runtime encryption key for API credentials.

**If you want a pre-migration safety net, back up the host `data/` directory yourself before upgrading** — e.g. `tar czf taskboard-data.backup.tar.gz taskboard-data`. The migration is one-way.

Everything else in v0.3.0 stays as-is; the CHANGELOG entry for v0.3.0 has been corrected to describe this behaviour.

## 2026-04-23 — v0.3.0

### Multi-user support with folder-level data isolation

Every user now has their own directory under `data/` containing *everything* that belongs to them — password, preferences, Hermes servers, tags, tasks, attempts, schedules. There is no shared central DB anymore. The layout:

```
data/
  config.yaml                 # global: listen, scheduler, archive, OSS, session_secret
  admin/
    config.yaml               # per-user: id, password_hash, is_admin, preferences, hermes_servers[], tags[]
    disabled                  # sentinel file — presence means the account is disabled
    db/taskboard.db           # this user's tasks, attempts, deps, schedules
    task/{task-id}.json
    attempt/{attempt-id}/
  tony/
    config.yaml
    db/…
    task/…
    attempt/…
```

This satisfies the stated design goal — **folder-level pluggability**: wiping one user is `rm -rf data/{username}/` with zero DB cleanup needed. Nothing leaks between users because the SQL layer never sees two users at once; each HTTP request resolves its per-user `*sql.DB` from a small registry keyed by authenticated username.

**Login is now always on.** The board ships with a default `admin` / `admin123` on first boot — change it immediately in **Settings → Access control**. Forgot it? Stop the server and run `./taskboard -data ./data --reset-admin` to reset the admin password and clear any disabled flag.

**No cross-user view even for admin.** Admins see only their own tasks on the board. To work as another user, log out and log back in with that user's password — there is no impersonation. Admins do get extra panels that regular users don't see: **Users** (invite, reset password, disable/enable, delete), **Global / Scheduler**, **Integrations (OSS)**, **Archive**, and the **Reload config from file** button.

**Shared Hermes servers + tags.** Creating either with the "Shared" checkbox ticked makes it read-only-visible to every other user — they can see it and use it, but cannot edit or delete.

**Disabled sentinel.** Admin → Users → Disable writes an empty `data/{username}/disabled` file. Existing sessions for that user fail the very next request with 401; login attempts return `account disabled`. Re-enable removes the sentinel.

**One-shot migration from the single-DB layout.** When the new binary detects a legacy `data/db/taskboard.db` or a `hermes_servers` field in the global `data/config.yaml`, it runs once on startup:

1. Reassigns every task, attempt, dependency, tag link, and schedule to the `admin` user (copied into `data/admin/db/taskboard.db`).
2. Pulls `hermes_servers` out of the global config and inlines them into `data/admin/config.yaml` with API keys re-encrypted under the same `data/.secret` AEAD key.
3. Moves `data/task/` → `data/admin/task/` and `data/attempt/` → `data/admin/attempt/`.
4. Removes the legacy `data/db/taskboard.db` (and WAL/SHM companions) after the copy succeeds. `data/.secret` stays put — it's still the runtime AEAD key. (In v0.3.0 the old DB was archived to `data/_migrated-YYYYMMDD-HHMMSS/db/`; v0.3.1 drops that archive because it was easy to mistake for the live DB. Back up `data/` yourself before upgrading if you want a safety net.)
5. Rewrites `data/config.yaml` stripped of the per-user fields.

### Delete individual attempts

The task modal now has a "✕" button next to each attempt in the list (with an inline 2-click confirm). Deleting an attempt removes the SQL row + its filesystem event log. Running / needs-input attempts must be cancelled first (the UI gates the button until state is terminal). Deleting a task still cascade-deletes every attempt belonging to it.

### API / storage changes — things that moved

Most of these are invisible after migration; listed for operators who manage the files by hand:

| Before | After |
|---|---|
| `data/config.yaml: auth.enabled / username / password_hash` | removed — per-user `config.yaml` + bcrypt |
| `data/config.yaml: hermes_servers[]` | `data/{username}/config.yaml: hermes_servers[]` |
| `data/config.yaml: preferences` | `data/{username}/config.yaml: preferences` |
| central `tasks.owner_id` column | dropped — one SQLite DB per user |
| central `tags` SQL table | removed — tags live in user config.yaml |
| `users` table in DB | removed — users are directories |
| `POST /api/auth/enable` / `/disable` | removed — login is always on |
| `DELETE /api/users/{id}` | `DELETE /api/users/{username}` |
| `PATCH /api/users/{id}/disabled` | `PATCH /api/users/{username}/disabled` |
| `POST /api/users/{id}/password` | `POST /api/users/{username}/password` |
| `DELETE /api/attempts/{id}` | **new** — delete a terminal attempt |

Session cookies now carry `username` instead of a UUID; existing cookies from older builds are rejected on next request (users just log in again).

## 2026-04-21 — v0.2.0

### `previous_response_id` no longer 404s after the user hits Stop

Reported by a user who cancelled a run mid-stream and then typed a follow-up: `✗ hermes responses: 404 Not Found: {"error":{"message":"Previous response not found: resp_3d95fc5de65041d2adc7eefb40f9",…}}`. Root cause was a mismatch between *when* taskboard saved the response id and *when* Hermes treats it as durable. Taskboard was writing `meta.Session.LatestResponseID` on the `response.created` SSE event (the very first event of the stream), but Hermes only retains responses that reach `response.completed` — cancelled or errored responses are discarded. So after a cancel, taskboard still held an id that Hermes no longer knew about, and any follow-up turn posted `previous_response_id=<dead id>` and hit 404.

Fix is in two parts:

1. **Persist only completed response ids as chain anchors.** The `LatestResponseID` write moves from `response.created` → `response.completed`. `CurrentRunID` is still captured on `response.created` because ResumeOrphans needs it to reconnect to an in-flight stream after a taskboard restart — that's a different job than "anchor the next turn's chain". Also guarded the post-`CreateResponse` meta update so a streaming call's empty `res.ResponseID` can no longer silently wipe the prior turn's anchor.

2. **404 self-heal on the next request.** If Hermes rejects the `previous_response_id` we sent (matched via the specific `"Previous response not found"` error body), the runner clears the local id, logs a `sys:previous_response_id_stale` event for audit, and retries the call as a cold start. The `conversation` tag still ties the new turn to the same Attempt, so the user sees no interruption. This also self-heals any meta.json left in a bad state by an older build.

Verified end-to-end against the local Hermes gateway: before the fix, a cancel → follow-up reproduces the exact reported 404 with the real error body; after the fix, the same sequence succeeds. Integration test at `internal/attempt/runner_real_hermes_test.go` (gated behind `-tags integration_real_hermes` so it doesn't run in the default suite). Offline tests using a fake SSE server at `internal/attempt/runner_cancel_chain_test.go` cover the same scenarios without a live gateway.

### Hermes request shape — `conversation` + `previous_response_id` are mutually exclusive

Discovered via the session-continuity work above: Hermes rejects `POST /v1/responses` with HTTP 400 "Cannot use both 'conversation' and 'previous_response_id'" when both are set. Client now prefers `previous_response_id` when it has one (pins the exact ancestor) and falls back to `conversation` only on the very first turn where there's nothing to chain from.

### Markdown: GFM pipe tables + thematic breaks render in event stream

Assistant replies that include tables or horizontal rules were rendering as raw pipes and dashes. Added GoldMark's GFM table extension and thematic-break support to the markdown pipeline that feeds the event-stream bubbles.

### Uploads: preserve the real file extension when MIME is ambiguous

The upload handler was deriving the saved filename's extension from the `Content-Type` header, which some clients send as `application/octet-stream` for anything non-obvious. Result: a `.zip` arriving with `octet-stream` saved as `.bin` and the OSS preview URL refused to render. Now the original filename's extension wins when the MIME type would otherwise produce a useless generic extension.

### Accept audio / video / documents in uploads, not just images

The attach-file control whitelisted only images. Expanded to accept audio, video, and common document types (PDFs, Office, text, archives). Storage layout and OSS key generation unchanged.

### UI: Hermes server label shows name, not id

Card headers and the attempt detail pane were labelling the selected Hermes server with the internal `server_id` (`local`, `office`) instead of the human-facing `name` (`Local Hermes`, `Office PC`). Flipped to `name`, with the id kept as a tooltip for operators who still care. The English "Server:" label now also has its Chinese translation.

### Resume orphan runs + manual reconnect + paginated events

If taskboard crashes while an Attempt is mid-stream, Hermes keeps the run alive — we just lose the SSE subscription. New `Runner.ResumeOrphans()` (called at boot, before the scheduler fires) re-attaches to `/v1/runs/{id}/events` using the `CurrentRunID` persisted in `meta.json`. If the reconnect succeeds the attempt continues as if nothing happened; if the run expired or Hermes no longer knows it, the attempt is marked Failed with a clear system event.

Frontend gets a **Reconnect** button on the attempt pane for the rare case where the client-side EventSource drops but the server is fine, plus event-stream pagination: the pane now loads the most recent events on open and lazily pulls older ones as you scroll up, so attempts that streamed thousands of lines don't lock the UI.

### Mobile polish (PWA, touch, cross-column drop)

Everything a phone user hit since v0.1.0 got tightened:
- Proper PWA manifest + icon set → iOS and Android "Add to Home Screen" now installs a standalone-wi window instead of a Safari shortcut.
- `touch-action: none` on draggable cards so iOS Safari stops hijacking drags as page pans.
- Mobile cross-column drop lands via the tab strip rather than requiring the user to reach a column that isn't on screen.
- Version chip + scroll gutters + mobile-tuned padding throughout.
- EventStream gains scroll controls ("↓ new messages" pill on the bottom) + pagination affordance.
- Service worker switches to network-first for HTML with explicit `Cache-Control` on static assets, so the shell updates without a manual cache bust.

### Bilingual operator manual served in-app via help (?) button

`docs/manual.en.md` + `docs/manual.zh-CN.md` ship inside the binary. The `?` button on the Attempts pane (plus a new one in the settings drawer) opens the manual in an inline panel — the locale follows the UI's current language.

## 2026-04-20

### Fallback to Hermes's built-in `hermes-agent` when a server has no `models`

Config entries for Hermes servers let users declare their own model profiles (name + concurrency cap). If the list was empty — which is the default after a fresh install because the gateway auto-advertises only one built-in model — the dispatcher had nothing to pick and attempts silently stalled in Queued. Fix: when a server's `models` list is empty, dispatch falls back to the string `hermes-agent` (the one model Hermes's `/v1/models` actually returns out-of-the-box). Users who later register additional model profiles still take precedence over the fallback.

### Docker: bump build stage to `golang:1.25-alpine`, pre-own `/data` at UID 65532

The `/data` volume directive in the old Dockerfile left the directory owned by root, which broke the distroless/nonroot runtime's ability to `mkdir` its subdirs on first start (no shell in distroless means a `RUN chown` at the final stage isn't possible). Fix: build stage now creates a `/skel-data` skeleton and copies it into the final image with `--chown=65532:65532`, so both named volumes and bind-mounts work without a host-side chown. Build stage also bumped to `golang:1.25-alpine` to match the toolchain the project now requires.

## 2026-04-21 (earlier)

### Session continuity with Hermes — experimentally verified via `cmd/hermesprobe`

Taskboard sends follow-up messages on an existing Attempt by POSTing to Hermes's `/v1/responses`. Two orthogonal fields decide whether Hermes picks up prior context: `conversation` (a stable tag) and `previous_response_id` (a specific ancestor). Running the live probe at `cmd/hermesprobe/` against the `local` Hermes server pinned down exactly what each does:

**S1 Linear chain `a → b → c`** (every step carries `previous_response_id=prev`) — PASS. Hermes correctly recalls all facts injected across the three turns.

**S2 Skip the middle** (`a → b`, then a third turn with `previous_response_id=a` instead of `b`) — *mixed*. Hermes's text reply cites facts from both `a` and `b` even though `b` isn't in the chain. Looking at `tools=[memory memory memory …]` on every prior turn explains it: Hermes's agent profile calls the `memory` skill to persist facts model-side, so those facts survive a broken `previous_response_id` chain. The conversation-level chain is linear-by-id; the agent's memory store is not.

**S3 Fork** (`a → b` and `a → c` independently, using the same `previous_response_id=a`) — PASS. Both children see the parent's context, neither sees the sibling's. No error from Hermes when a parent already has another child. This is the answer to "`a → b → c`, but `c` got lost; can we rewind and continue from `b`?" — yes, reusing any recorded ancestor id is safe; Hermes just branches. The orphaned response is effectively dropped.

**S4 Invalid `previous_response_id`** — Hermes returns **HTTP 404**, refusing to silently cold-start. Good — we surface the error instead of making up a fresh session the user didn't ask for.

**S6 `conversation` tag alone** (same string across two turns, no `previous_response_id`) — PASS. Hermes links the turns by the conversation tag. (An earlier hypothesis that Hermes ignored the `conversation` field came from a flawed test that generated a fresh random tag per turn.)

Practical consequence for taskboard: we now send **both** fields on every `runOnce`. `Conversation: att.ID` keeps Hermes-tag-level continuity through the whole Attempt, `previous_response_id: meta.Session.LatestResponseID` pins the exact ancestor. First turn has no `previous_response_id` (cold start, intended). Each turn's `sys:run_start` event now records `previous_response_id` so the audit trail shows whether a given turn was a chained continuation or a cold start.

The probe stays in-repo as `cmd/hermesprobe/`; run against any registered server with:

```bash
go run ./cmd/hermesprobe -server <id> -only s1,s2,s3,s4,s6
```

S5 (30-second gap, session survival across idle) takes a full minute and is skipped by default; add `s5` to the `-only` list to include it.

## 2026-04-19

### Release (round 7.2 → tagged v0.1.0) — schedule picker UX, orphan reaper, drag/click fix

**Schedule picker now speaks plain language, backend is cron-only.**
Previously the per-task schedule picker exposed two kinds (`interval` and `cron`) and required users to type raw specs like `15m` or `0 9 * * 1-5`. Since cron already expresses intervals (`*/15 * * * *`), the second kind was redundant *and* unfriendly. Redesigned the picker to be preset-driven: "every N minutes / hours" (N is a free-form number), "daily at HH:MM", "weekly on picked weekdays at HH:MM", "monthly on day D at HH:MM", plus an Advanced escape hatch for raw cron. Saved schedules render back as human prose ("Every 15 minutes", "Weekly Mon, Wed, Fri at 09:00") with the raw cron underneath for inspection. The picker shows a live preview of the cron it will save, so users know exactly what's going to disk.

Backend rewritten to accept only `kind='cron'` (API rejects anything else). One-shot DB migration on startup converts any legacy `interval` rows (`time.ParseDuration` string) to a best-effort cron approximation: `N` minutes up to 59 → `*/N * * * *`; full-hour multiples up to 23 → `0 */H * * *`; anything past a day collapses to daily at midnight. Migrated rows have their `next_run_at` cleared and the worker rehydrates them on boot via a new `ListEnabledNullNextSchedules` sweep, so no schedule is silently missed after the upgrade.

**Orphan Attempt reaper at boot**
Before this release, if the taskboard process crashed or was killed while an Attempt was mid-stream, the Attempt's DB row would stay `running` forever — no process owned it, no code flipped it. The UI would spin, the concurrency slot would leak, and nothing ever reaped it. Fix: new `Runner.ReapOrphans()` called from `main.go` *before* the scheduler/cron worker boot. It sweeps `state IN ('queued','running')` Attempts, writes a system event `error: "process restart — attempt reaped as failed (no active runner)"` with `prior_state`, flips state to `failed`, broadcasts a state-change over SSE, and calls `Board.MaybeAdvanceAfterAttempt`. `needs_input` Attempts are left alone — they legitimately wait for user input and `SendMessage` already restarts their loop when input arrives.

**Drag no longer opens the card modal**
The card's click handler used `this.$el.style.display === 'none'` as a "drag in progress" guard, but `drag.js`'s `end()` restores `display = ''` *before* the browser fires the synthetic click, so the guard always missed the click fired at drag end → the task modal popped open every time you moved a card. Replaced with a robust `_dragStarted` flag: reset to `false` at the start of each `pointerdown`, set to `true` the moment `drag.start()` is invoked (on movement > 5 px), consumed in `onClick` if set. Works regardless of DOM timing.

**v0.1.0 tag cuts the first public release.** See `docs/release-notes/v0.1.0.md` for the full shipping summary.

### Hotfix (round 7.1) — user's own messages were invisible in the event stream
The SSE fix from earlier today (wrapping every payload with an `event` key to bypass the addEventListener/onmessage mismatch) was clobbering the inner `event` field that the AttemptRunner set on each NDJSON line — so `user_message`, `run_start`, `run_end` etc. all arrived on the wire as `event: "event"` and the UI couldn't discriminate. Result: typing a follow-up message showed the assistant's reply but the user's own bubble was silently dropped.
Fix: only merge the outer wrapper name when the payload doesn't already carry one, preserving the runner's inner event names verbatim. Added `test_sse_preserves_event_name` so this specific failure mode can't slip back in. Suite is 30/30 green.

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
- **Column subtitles**: each of the six columns now has a small gray one-liner explaining its meaning (e.g. Plan → "Queued and ready for execution."). Translations are loaded from the per-locale dictionaries. (Requirement #10)
- **Settings page**: now includes an explicit helper paragraph under *Models* explaining that each row corresponds to a Hermes agent profile (same thing the Hermes API calls "model"). (Requirement #11)
- **Settings modal reopen bug** — fixed. Now always goes through a `showSettings = false → true` transition to avoid a stale-state window where the second click was a no-op. (Requirement #12)
- **i18n rewritten to be reactive** (Vue ref) — no more language-mixing after toggle. The `$t(key)` lookup consistently resolves against exactly one dictionary. Missing keys fall back to English, never to a leftover Chinese string. (Requirement #9)

### Behind the scenes
- JSON tags on all `config.Preferences`/`Sound`/`Scheduler`/`Archive`/`Server`/`OSS` struct fields, so the API now returns `{ language, theme, sound: {…} }` rather than Pascal-cased keys. This was a silent API break for the frontend; fixed together here.
- `POST /api/tasks/{id}/transition` accepts `after_id` / `before_id` to request a specific drop slot; the backend computes a new `position` mid-way between neighbours and renumbers the column if needed to recover from collisions.
- New module layout on the frontend: `i18n.js`, `markdown.js`, `drag.js`, `description-editor.js`, `event-stream.js` are now their own files imported by `app.js`.
- New Playwright suite `tests/ui_test.py` with 15 cases — run it any time after a UI change.

### Docs (later)
- Added a direct link to the Hermes API Server docs (<https://hermes-agent.nousresearch.com/docs/user-guide/features/api-server>) inside the "Set up Hermes for this board" section of `README.md`. Readers can jump straight to the upstream reference for every configurable field after reading our minimal setup.
- Rewrote `README.md` so the English and Chinese halves each read like native-language prose rather than mutual translations:
  - English passages no longer smuggle in CJK glyphs (e.g. `WeChat / Feishu (Lark) / DingTalk / QQ` instead of the native spelling).
  - Chinese passages cut awkward English jargon in favour of natural phrasing.
  - Screenshot captions and section headings reordered to each language's conventions.
- Hermes link corrected to `https://github.com/NousResearch/hermes-agent` (the previous `https://github.com/hermes-agent` didn't exist).
- Tightened the top tagline, dropped the awkward language-toggle suffix that used to hang off the end.
- Both language variants of `README.md` gained a new **Why this exists** section (same day, earlier) explaining Hermes's self-evolving-agent positioning, the messaging gateways it supports, and the friction that pushed this project into existence.

### Docs
- `docs/requirements.md` bumped to v0.2: §4.8.1 / §4.8.5 each gained a "contract" lead-in paragraph promoting two rules to bright red lines:
  1. Every registered Hermes Server must declare a server-level concurrency cap (default 10) and each profile (e.g. `hermes-agent`) must declare its own cap (**default 5**); breaching either layer rejects the next Attempt.
  2. All settings (auth, Hermes Servers, various toggles) live in `data/config.yaml`; the process loads it into memory on boot and writes atomically on edit, and the settings page must expose a "Reload config from file" button (`POST /api/config/reload`) so hand-edited YAML can hot-reload without a restart.
- Added a revision-history section at the top of the doc.

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
- Bilingual (English / Simplified Chinese) README; this CHANGELOG is English-only.

### Known limitations
- No multi-user or RBAC (single user by design for v1).
- Tool-call `approval_required` events are surfaced but not interactively approved in the UI — they render as system events for now.
- Scheduler server-health short-circuit uses a 30 s cache; if the Hermes server goes down mid-tick you may see a short stream of failed attempts until the cache expires.
- `archive.auto_purge_days` is read by the scheduler config but the reaper goroutine is stubbed — files currently accumulate until you delete tasks manually.
