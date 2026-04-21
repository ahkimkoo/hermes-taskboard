# Plugin Prototype Plan — Validating the "Taskboard as Hermes Platform" Hypothesis

**Status:** plan only, no implementation yet
**Scope:** one week of prototype work, aimed at *falsifying* the approach fast if it doesn't hold up
**Companion doc:** `hermes-plugin-feasibility.md` (architecture survey)

---

## Why a prototype first

The feasibility survey answered *"is the plugin surface rich enough to host us?"* (yes). It did **not** answer the three questions we actually need yes's on before refactoring anything:

1. **Does a Hermes task really keep running after its controlling client disconnects?**
   The whole premise of this refactor is that yes, it does. If Hermes turns out to tie the agent run to the platform adapter's in-memory state in some subtle way (e.g. streaming through a per-connection queue, cancelling on unexpected send-failure), the plugin model buys us nothing and we should not pay its costs.
2. **Is the Python↔Go bridge stable enough under real load to trust as the session transport?**
   WebSocket on localhost is not free — reconnect semantics, backpressure, framing errors, process-restart timing windows all matter. A thin prototype that we deliberately beat up will tell us this in days; arguing about it on paper will not.
3. **Can we cancel / interrupt a Hermes run through the plugin cleanly?**
   The reported 404 bug we just fixed was half-symptom / half-architecture. If the plugin lets us use Hermes's native interrupt mechanism (the `handle_message` "busy session" path the feasibility report flagged) instead of cancelling HTTP requests, the whole class of cancellation bugs goes away. If not, we're trading one set of edge cases for another.

**Go/no-go gate at the end of the prototype:** if all three answer yes with acceptable signal quality, we write a migration plan. If any one answers no, we document the failure mode and stay on the current HTTP/SSE architecture — it's good enough post the resume-orphans + self-heal fixes.

---

## Explicit non-goals for the prototype

These belong in the *migration* phase, not here. Hold the line:

- **Don't replace the existing `hermes.Client` codepath.** Run the plugin as a second, opt-in transport behind a server config flag. The default `/v1/responses` path stays as-is.
- **Don't ship UI for plugin transport.** The switch is a YAML flag. Operator experience can wait.
- **No auth, no TLS, no cross-host.** The plugin's WebSocket listens on `127.0.0.1` only. Anything else blows scope.
- **No migration of existing Attempts.** Old attempts keep using HTTP; new ones opt in via task-level flag or a fresh attempt.
- **No changes to Hermes upstream.** If we can't make it work without patching Hermes source, that's a no-go signal, not a todo item.
- **No new persistence layer.** Whatever `gateway/session.py` already writes to `~/.hermes/sessions.json` is what we get.

---

## Component sketch

```
┌──────────────────────────────────────┐                ┌──────────────────────────────────────┐
│  Hermes Gateway Process (Python)    │                │  Taskboard Process (Go)             │
│  (asyncio, single event loop)        │                │                                      │
│                                      │                │                                      │
│  ┌────────────────────────────────┐ │   WebSocket    │  ┌────────────────────────────────┐ │
│  │  TaskboardAdapter (NEW)        │◄┼──── /ws ──────►│  │  hermes.PluginClient (NEW)     │ │
│  │  - subclasses Base…Adapter    │ │  JSON frames   │  │  - reconnect loop               │ │
│  │  - owns WebSocket server       │ │                │  │  - mirrors hermes.Client API    │ │
│  │  - maps attempt_id ↔ chat_id  │ │                │  │  - used only when selected       │ │
│  └─────────────┬──────────────────┘ │                │  └─────────────┬──────────────────┘ │
│                │                      │                │                │                     │
│                │ self.handle_message()│                │                │ same events[] feed │
│                ▼                      │                │                ▼                     │
│         [Gateway dispatcher]          │                │         [existing Runner]           │
│                │                      │                │                                      │
│                ▼                      │                │                                      │
│         [Hermes agent]                │                │                                      │
│                │                      │                │                                      │
│                ▼                      │                │                                      │
│         agent events ─────────────────┘  adapter.send() delivers text; we extend with
│                                             a stream-frame variant (see protocol below)
└──────────────────────────────────────┘
```

Key property of this layout: the **agent run is owned by the gateway process**. The Go side is free to disconnect, restart, or sit silent for hours; the agent keeps running, writes to session state, and is reachable again the moment the WebSocket reconnects.

---

## Protocol (v0 — deliberately minimal)

One WebSocket, JSON frames, one message per frame. No multiplexing gymnastics.

### Go → plugin

| `type`            | Fields                                                | Semantics                                                         |
|-------------------|-------------------------------------------------------|-------------------------------------------------------------------|
| `hello`           | `taskboard_version`, `client_id`                      | Sent on connect. Plugin replies with `hello_ack` + current session list. |
| `start_attempt`   | `attempt_id`, `title`, `system_prompt`, `initial_input` | Creates a new session identified by `attempt_id`.                |
| `send_message`    | `attempt_id`, `text`                                  | Inject a user turn into an existing session.                      |
| `cancel`          | `attempt_id`                                          | Interrupt the currently-running turn (uses Hermes's native busy-session path). |
| `resume`          | `attempt_id`                                          | Request a replay of any events produced while Go was disconnected. |
| `ping`            | `ts`                                                  | Liveness; plugin replies `pong`.                                  |

### Plugin → Go

| `type`            | Fields                                                 | Semantics                                                       |
|-------------------|--------------------------------------------------------|-----------------------------------------------------------------|
| `hello_ack`       | `gateway_version`, `active_attempts: [attempt_id,…]`  | Plugin's view of live sessions; lets Go reconcile.              |
| `agent_event`     | `attempt_id`, `seq`, `event: <hermes SSE payload>`    | One-to-one with what the existing `/v1/responses` SSE yields.   |
| `agent_done`      | `attempt_id`, `summary`, `final_response_id`          | Turn completed cleanly.                                          |
| `agent_error`     | `attempt_id`, `message`                               | Turn failed / was cancelled / hit an internal error.            |
| `pong`            | `ts`                                                  | Liveness.                                                        |

### Reconnect contract

On reconnect, Go sends `hello` with `client_id` matching the previous connection. Plugin replies with `hello_ack` including `active_attempts`. For each attempt Go cares about, Go sends `resume` and plugin replays buffered events since the last ack'd `seq`. Events produced while Go was offline sit in a bounded per-attempt ring buffer inside the plugin (size = 1024 or so — one turn's worth of tokens).

**This ring buffer is the load-bearing mechanism for the whole promise.** If it turns out to be unreliable (events lost, seq gaps, memory bloat), the go/no-go gate fails on question 2.

### What we deliberately do *not* put in v0

- **Auth.** localhost only; ship auth in migration.
- **Compression / binary frames.** Hermes SSE is already compact; measure before optimising.
- **Multi-client.** One Go connection at a time. Second `hello` evicts the first.
- **Attachments / images.** Kept on HTTP for now; the plugin path handles text turns only.

---

## Python side: minimal plugin skeleton

Target file: `gateway/platforms/taskboard_bridge.py` inside the user's Hermes checkout (not this repo). ~300 lines.

Implementation beats, mapped to `ADDING_A_PLATFORM.md`:

1. **New `Platform.TASKBOARD_BRIDGE` enum value** (`gateway/config.py`). Requires editing Hermes — this is the one upstream touch we can't avoid. If that's a dealbreaker for the user, fall back to piggybacking on `Platform.API_SERVER` and multiplexing. (It isn't a dealbreaker for a prototype on the user's own box.)
2. **Subclass `BasePlatformAdapter`:**
   ```python
   class TaskboardBridgeAdapter(BasePlatformAdapter):
       async def connect(self) -> bool:
           self._server = await websockets.serve(self._handle_ws, "127.0.0.1", 19100)
           return True

       async def disconnect(self) -> None:
           self._server.close()
           await self._server.wait_closed()

       async def send(self, chat_id, content, reply_to=None, metadata=None):
           # Route agent text output to whoever's connected as chat_id=attempt_id
           await self._push({"type": "agent_event", "attempt_id": chat_id,
                             "event": {"type": "text", "content": content}})
           return SendResult(success=True, message_id=str(uuid4()))
   ```
3. **`_handle_ws`** — one coroutine per connected Go client. On `send_message` → `MessageEvent` + `self.handle_message(event)`. That single call drops the message into the normal gateway dispatcher, which handles session routing, busy-state, interrupts — all for free.
4. **Event interception for streaming.** `send()` is called once per final message, but we want token-level streaming. The base class exposes hook points for partial tokens — `api_server.py` already does this (see `gateway/platforms/api_server.py:1-400`); copy that pattern.
5. **Cancel = inject a magic message.** Hermes's busy-session path interprets any new message during a running turn as an interrupt signal. The plugin can synthesize a "cancel" `MessageEvent` with a sentinel string and let the existing interrupt flow do its job.
6. **Ring buffer + seq.** Maintain `dict[attempt_id -> deque[(seq, event)]]` with 1024 slot cap. Purge on `agent_done`. On `resume`, replay from the last acked seq.

---

## Go side: minimal plugin client

Target: a new file `internal/hermes/plugin_client.go` plus a wrapper that picks HTTP vs plugin based on server config. Don't touch the existing `hermes.Client`; add alongside.

Shape:

```go
type PluginClient struct {
    addr   string            // 127.0.0.1:19100
    conn   *websocket.Conn   // nil when disconnected
    out    chan Event        // same Event type the runner already consumes
    subs   map[string]chan<- Event  // attempt_id -> runner's events channel
    ...
}

// API mirrors the parts of hermes.Client the runner uses:
func (p *PluginClient) CreateResponse(ctx, req ResponseRequest) (*ResponseCreateResult, error)
func (p *PluginClient) CancelRun(ctx, attemptID string) error
```

Reconnect loop uses exponential backoff (1s → 30s cap, jitter). On reconnect it sends `hello` and then one `resume` per attempt that's still in `running` state in the store. `hello_ack.active_attempts` is the authority for "which attempts actually exist in Hermes" — attempts present locally but absent in ack are marked Failed with reason `session_lost`.

Selection: add `hermes_servers[].transport: http|plugin` to the config schema. Default `http`. `Pool.Get()` returns either `*Client` or `*PluginClient` under a common interface (`type HermesBackend interface{ … }`).

---

## Bench scenarios (the actual tests)

Each scenario is a manual script plus a pass/fail assertion. No automated framework — this is a prototype.

### B1 — Baseline parity
Create an attempt via plugin transport, send a prompt, receive streamed tokens, observe `agent_done`. Same prompt via HTTP transport. Transcripts should match semantically (exact token equality isn't expected; same answer structure is).
**Pass:** both return a sensible answer; no event drops in the plugin transcript.
**Fail signal:** plugin stream is shorter, truncated, or misordered vs HTTP. → question 2 fails.

### B2 — Client crash mid-stream
Start a long prompt. At ~3 seconds into streaming, `SIGKILL` the Go taskboard. Wait 60 seconds. Check:
  1. Hermes agent output is still being appended to `~/.hermes/sessions/<session_id>/transcript.json` (session write continues while no client is attached).
  2. Restart taskboard. It reconnects, calls `resume`, receives the tail of events produced while it was offline, reaches `agent_done`.
**Pass:** attempt ends `completed` with a summary that includes material generated during the disconnect window.
**Fail signal:** session file stops growing on disconnect → question 1 fails, abort the refactor.
**Fail signal:** session continues but ring buffer dropped events → question 2 fails, rework the buffering.

### B3 — Cancel is honored server-side
Start a long prompt. After 3 seconds, send `cancel` from Go. Verify:
  1. Plugin reports `agent_error` or `agent_done` within 2 seconds.
  2. Hermes log shows the agent actually stopped invoking tools / generating tokens (check `~/.hermes/sessions/<session_id>/transcript.json` has no growth past the cancel mark).
  3. Follow-up `send_message` on the same attempt works without any 404-style error.
**Pass:** all three hold.
**Fail signal:** Hermes keeps running the cancelled turn in the background → we've replaced 404 with a "zombie turn" problem; not obviously better. Decide whether that's acceptable.

### B4 — Concurrent sessions
Start three attempts in parallel, each with a 30-second prompt. All three should stream concurrently. Neither should starve the others (token interleave by timestamp ≈ round-robin).
**Pass:** all three complete with no cross-talk (attempt A never receives events tagged with attempt B's id), total wall time ≈ max(individual times) not sum.
**Fail signal:** one session blocks others → gateway asyncio loop is being stalled by our plugin. Review: is `send()` doing anything blocking? Is our WebSocket push serialized behind a mutex it shouldn't be?

### B5 — Reconnect under load
Connect, start 3 attempts, disconnect WebSocket mid-stream, reconnect after 5 seconds, `resume` all 3. Expected: each gets the events it missed in order; none is reset.
**Pass:** all three reach completion with no `seq` gaps.
**Fail signal:** `seq` gap or stream termination → ring buffer sizing / eviction bug; iterate on the plugin.

### B6 — Plugin-process memory growth
Run B4 in a loop for 30 minutes. Watch RSS of the gateway Python process.
**Pass:** flat or slow growth (≤ 50MB over 30m).
**Fail signal:** linear growth → buffer isn't purging after `agent_done`. Fixable but material.

---

## Timeboxing

Rough budget — one engineer-week, interruptible.

| Day | Deliverable |
|-----|-------------|
| 1   | Python plugin skeleton connects, echoes text between Go and the Hermes dispatcher. No streaming yet. |
| 2   | Streaming event forwarding end-to-end; B1 passes. |
| 3   | Reconnect + ring buffer + resume; B5 passes. |
| 4   | Cancel / interrupt; B3 passes. Go-side `PluginClient` behind config flag. |
| 5   | B2 (client crash) + B4 (concurrency). This is the critical day — answers question 1 definitively. |
| 6–7 | B6 (soak), polish, write up the go/no-go memo with attached recordings. |

If we're still stuck on B2 at the end of day 5 and haven't gotten a clean "Hermes keeps running after client leaves" observation, that's the failure signal. Write up the finding, revert scope to the current HTTP architecture, and move on.

---

## Deliverables from the prototype phase

Regardless of go/no-go outcome:

1. A go/no-go memo summarising each bench scenario with pass/fail + evidence (log excerpts, screenshots of session transcripts).
2. The prototype code pushed to a **separate branch** (`proto/plugin-bridge`), not merged. If go, it becomes the base for the migration plan. If no-go, it's reference for the next time this comes up.
3. Any upstream Hermes patches necessary captured as a diff against the user's local Hermes checkout, in case they later want to upstream them.

Explicitly **not** deliverable from this phase:

- Production-quality code
- Docs beyond this plan + the go/no-go memo
- Migration of existing Attempts
- UI changes

---

## Open questions to resolve before day 1

Ask the user (i.e. the actual operator) these before starting day 1:

1. **Where does the Python plugin live on the host?** Dropping a file into the user's `~/.hermes/hermes-agent/gateway/platforms/` directory vs. vendoring a Hermes fork. The former is faster; the latter is cleaner long-term.
2. **Are you OK with one upstream edit** to `gateway/config.py` to add the `TASKBOARD_BRIDGE` enum value? (Alternative: impersonate `API_SERVER` and demultiplex, which is uglier.)
3. **What's the acceptable interrupt latency?** B3 expects ≤2s; if "mostly-immediate" isn't good enough and you want sub-500ms, the interrupt path may need to bypass the MessageEvent inbox.
4. **Parallel concurrency cap** — existing HTTP path respects per-server `max_concurrent`. Plugin path will inherit whatever the gateway's asyncio scheduler decides. Is that acceptable for the prototype? (Migration phase will bring limits back.)

Answers before day 1 save a day of rework.
