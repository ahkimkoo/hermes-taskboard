# Hermes Plugin Feasibility: Building Taskboard as a Platform Plugin

## Executive Summary

**Can we do this?** Yes. Hermes provides a well-designed platform plugin system that allows Python adapters to receive messages independently of HTTP clients, handle session management, and survive process restarts through persistent session storage.

**Is it the right path?** Mostly yes, but with caveats. The platform plugin approach is architecturally superior to SSE clients because plugins receive messages directly from the gateway loop without client-connection coupling. However, building a Go service as a Python plugin requires a bridge (either synchronous Python subprocess or async IPC), which introduces complexity and potential failure modes you must carefully handle.

**Headline risk:** The gateway assumes single-threaded asyncio execution. If your Go service crashes or hangs, the Python plugin wrapper can deadlock the entire gateway. You must implement robust supervision, timeouts, and watchdogs. The existing Telegram/Discord/Slack plugins handle this by owning their network connections directly; a subprocess-based integration does not.

---

## 1. Gateway Architecture Overview

### Process Structure

The Hermes gateway (`hermes gateway run`, invoked via `gateway/run.py:start_gateway()`) is a **single Python asyncio process** that orchestrates:

1. **Platform adapters** — One per enabled platform (Telegram, Discord, Slack, etc.), registered at `/home/kasm-user/.hermes/hermes-agent/gateway/run.py:2383-2550`
2. **Session store** — Persists conversation state to disk (`gateway/session.py:353-398`, backed by `~/.hermes/sessions.json`)
3. **Message handler** — Central dispatcher that runs the Hermes agent for each incoming message
4. **Delivery router** — Routes cron job outputs to appropriate platforms
5. **Lifecycle watchers** — Background asyncio tasks for session expiry, platform reconnection, and process supervision

**Key architectural insight:** Platform adapters are concurrent in the event-loop sense (they all share one asyncio loop and don't block each other), but they do not run in separate threads or processes. They cooperatively yield control at `await` points. This is efficient but means a blocking operation in one adapter stalls the entire gateway.

### Startup and Shutdown

From `gateway/run.py:1701-1950`:

1. `GatewayRunner.start()` loads all enabled platforms' configs
2. For each platform, it calls `_create_adapter()` (line 2383) to instantiate the adapter class
3. Each adapter is wired with:
   - `set_message_handler()` — points to `_handle_message()` (the gateway's central agent dispatcher)
   - `set_session_store()` — grants read access to session metadata
   - `set_fatal_error_handler()` — allows adapters to report unrecoverable errors
   - `set_busy_session_handler()` — callback for custom handling of messages during active sessions
4. Each adapter calls `await adapter.connect()` to start listening
5. If connection fails and the error is retryable, the platform is queued in `_failed_platforms` for periodic reconnection attempts (lines 1869, 1885)

### Shutdown

From `gateway/run.py:1451-1454`:

- `_stop_all_adapters()` calls `await adapter.disconnect()` on each adapter
- The gateway waits up to `_restart_drain_timeout` (default 5 seconds) for in-flight agent runs to complete before exiting
- Active sessions are suspended (marked for auto-reset on next incoming message) to prevent stuck-resume loops on restart

**Diagram (text form):**

```
┌────────────────────────────────────────────────────────────┐
│  Hermes Gateway (Single asyncio Process)                   │
├────────────────────────────────────────────────────────────┤
│                                                              │
│  ┌─────────────────────────────────────────────────────┐   │
│  │  GatewayRunner                                      │   │
│  │  - adapters: Dict[Platform, Adapter]               │   │
│  │  - session_store: SessionStore                     │   │
│  │  - delivery_router: DeliveryRouter                 │   │
│  │  - _message_handler: fn(MessageEvent) -> str       │   │
│  └─────────────────────────────────────────────────────┘   │
│         ▲                                                    │
│         │ calls & manages                                   │
│         │                                                    │
│  ┌──────┴────────┬──────────────┬──────────────┐           │
│  ▼               ▼               ▼               ▼            │
│ ┌────┐       ┌────────┐     ┌────────┐     ┌────────┐       │
│ │TG  │       │Discord │     │Slack   │     │WhatsApp│ ...   │
│ │Adap│       │ Adapter│     │Adapter │     │Adapter │       │
│ └────┘       └────────┘     └────────┘     └────────┘       │
│   │              │              │              │             │
│   │ async        │ async        │ async        │ async        │
│   │ connect()    │ connect()    │ connect()    │ connect()    │
│   │              │              │              │             │
│   ▼              ▼              ▼              ▼             │
│ [Telegram]   [Discord]      [Slack]       [WhatsApp]        │
│  API         API             API            API              │
│                                                              │
│  ┌─────────────────────────────────────────────────────┐   │
│  │  Background Tasks (asyncio.create_task)             │   │
│  │  - _session_expiry_watcher()                        │   │
│  │  - _platform_reconnect_watcher()                    │   │
│  │  - _run_process_watcher() [for each background job] │   │
│  └─────────────────────────────────────────────────────┘   │
│                                                              │
└────────────────────────────────────────────────────────────┘
```

---

## 2. Platform Plugin Contract

### Minimal Interface (from `gateway/platforms/base.py:779-950`)

All platform adapters inherit from `BasePlatformAdapter` and implement:

#### Required Methods

```python
async def connect(self) -> bool:
    """Start listening for messages. Return True on success."""

async def disconnect(self) -> None:
    """Stop listening, close connections."""

async def send(self, chat_id: str, content: str, 
               reply_to: Optional[str] = None,
               metadata: Optional[Dict[str, Any]] = None) -> SendResult:
    """Send a text message. Return SendResult with success flag and optional message_id."""

async def send_typing(self, chat_id: str, metadata=None) -> None:
    """Send a typing indicator (platform-dependent)."""

async def send_image(self, chat_id: str, image_url: str, 
                     caption: Optional[str] = None, 
                     reply_to: Optional[str] = None,
                     metadata: Optional[Dict[str, Any]] = None) -> SendResult:
    """Send an image natively."""

async def get_chat_info(self, chat_id: str) -> Dict[str, Any]:
    """Return {'name': str, 'type': 'dm'|'group'|'channel'}."""
```

#### Optional Methods (have default stubs in base)

- `send_document(chat_id, path, caption)`
- `send_voice(chat_id, path)`
- `send_video(chat_id, path, caption)`
- `send_animation(chat_id, path, caption)`
- `send_image_file(chat_id, path, caption)`
- `edit_message(chat_id, message_id, content)`
- `stop_typing(chat_id)`

#### Message Ingestion

From `gateway/platforms/base.py:1482-1570`:

```python
async def handle_message(self, event: MessageEvent) -> None:
    """
    Called when a platform receives a message.
    This is the integration point — adapters call self.handle_message(event)
    to inject messages into the gateway.
    """
```

The protocol is:

1. Platform (adapter) receives incoming message from its API/WebSocket/whatever
2. Adapter normalizes it into a `MessageEvent` (defined at line 655) with:
   - `text: str` — message content
   - `message_type: MessageType` — TEXT, PHOTO, VIDEO, AUDIO, VOICE, DOCUMENT, etc.
   - `source: SessionSource` — identifies the platform, chat, user (see below)
   - `media_urls: List[str]` — local file paths of media (cached from platform URLs)
   - `reply_to_message_id: Optional[str]` — if replying to another message
   - `timestamp: datetime`
3. Adapter calls `await self.handle_message(event)`
4. The base class checks if a session is already processing this chat and either:
   - **If idle:** spawns a background task (`_process_message_background()` at line 1593) that runs the agent and sends the response
   - **If busy:** queues the message as pending and signals an interrupt event (line 1550)
5. Message handler never blocks the adapter loop

#### SessionSource (Session Identity)

From `gateway/session.py:65-138`:

```python
@dataclass
class SessionSource:
    platform: Platform          # Which platform (TELEGRAM, DISCORD, etc.)
    chat_id: str                # Unique identifier within platform (e.g., Telegram user ID)
    chat_name: Optional[str]    # Display name (e.g., "Alice", "dev-channel")
    chat_type: str              # "dm", "group", "channel", "thread"
    user_id: Optional[str]      # For groups, which user is speaking (e.g., Slack user_id)
    user_name: Optional[str]    # Display name of sender in groups
    thread_id: Optional[str]    # Discord thread ID, Slack thread timestamp, etc.
    chat_topic: Optional[str]   # Channel topic/description (Discord, Slack)
    user_id_alt: Optional[str]  # Signal UUID (alternative to phone)
    chat_id_alt: Optional[str]  # Signal group internal ID
```

Helper method on adapters (line 1905):

```python
def build_source(
    self,
    chat_id: str,
    chat_name: Optional[str] = None,
    chat_type: str = "dm",
    user_id: Optional[str] = None,
    user_name: Optional[str] = None,
    thread_id: Optional[str] = None,
    chat_topic: Optional[str] = None,
    user_id_alt: Optional[str] = None,
    chat_id_alt: Optional[str] = None,
) -> SessionSource:
    """Construct a SessionSource for this platform."""
```

#### Lifecycle Hooks

From `gateway/platforms/base.py:1618`:

```python
await self._run_processing_hook("on_processing_start", event)
# ... agent runs ...
await self._run_processing_hook("on_processing_complete", event, outcome)
```

Optional registration in subclasses:

```python
self.register_processing_hook(
    "on_processing_start",
    async fn(event: MessageEvent) -> None
)
```

#### Registration

**No dynamic entry points or auto-discovery.** Platforms are:

1. Enumerated in `gateway/config.py:Platform` enum (line 57 in ADDING_A_PLATFORM.md)
2. Wired explicitly in `gateway/run.py:_create_adapter()` (lines 2399-2550)
3. Configuration loaded from environment variables in `gateway/config.py:_apply_env_overrides()`

Example enum entry (from ADDING_A_PLATFORM.md):

```python
class Platform(Enum):
    TELEGRAM = "telegram"
    DISCORD = "discord"
    YOUR_PLATFORM = "your_platform"
```

### Concrete Reference: Telegram Adapter

From `gateway/platforms/telegram.py`, the closest match to what taskboard needs:

- Owns a long-lived `telegram.ext.Application` (connection to Telegram API)
- Registers message handlers with grammY/python-telegram-bot that fire on incoming updates
- Each handler calls `self.handle_message(MessageEvent)` to inject into the gateway
- Handles session lifecycle: groups messages by chat_id → session_key, tracks active agents per session
- Survives connection drops via built-in reconnection (the library handles this)

**Key insight:** Telegram adapter doesn't poll or block. It registers handlers and the library's event loop fires them asynchronously. The adapter yields control immediately after calling `handle_message()`.

---

## 3. Session Management

### Session Lifecycle

From `gateway/session.py:200-450` and `gateway/run.py:3380-3450`:

**Session creation:**
```python
session_entry = self.session_store.get_or_create_session(source)
# Returns SessionEntry with:
# - session_key: str (unique within platform, based on chat_id/user_id/thread_id)
# - session_id: str (UUID, stable across agent reruns in same session)
# - created_at: datetime (persisted to disk)
# - updated_at: datetime (updated on each message)
```

**Session persistence:**
- Sessions are stored in JSON at `~/.hermes/sessions.json` (from `gateway/session.py:_SESSIONS_PATH`)
- Per-session conversation history is stored in `~/.hermes/sessions/{session_id}/transcript.json`
- On gateway restart, sessions are loaded from disk and auto-resume if recent activity exists
- Sessions can have reset policies (idle timeout, daily reset, manual reset) defined per platform

**Reset policies:**
From `gateway/config.py:SessionResetPolicy`:
```python
class SessionResetPolicy(Enum):
    NEVER = "never"           # keep forever
    IDLE = "idle"             # after N hours of inactivity
    DAILY = "daily"           # reset at midnight
    EXPLICIT = "explicit"     # user must manually /reset
```

**Session suspension (for stuck-loop prevention):**
From `gateway/run.py:1772-1804`:
- On startup, recently-active sessions are marked as `suspended=True` (in memory and persisted to sessions.json)
- When a suspended session receives a new message, `get_or_create_session()` automatically creates a new `session_id`
- This breaks the infinite resume loop without losing old history (users can still access `/history`)

### Session Isolation

Each session has:
1. **Isolated message history** — transcript is per-session_id, not per-message
2. **Isolated agent cache** — the gateway caches `AIAgent` instances per session_key to preserve prompt caching (line 584)
3. **Isolated interrupt events** — each session has its own `asyncio.Event` for signaling message interrupts (line 1558)

When a message arrives:
1. Adapter builds a `SessionSource` (platform, chat_id, user_id, etc.)
2. Gateway computes `session_key = build_session_key(source, ...)` (from `gateway/session.py`)
3. Gateway checks if `session_key in self._active_sessions` — if yes, message is queued; if no, a new background task is spawned
4. Each task has exclusive access to that session's agent and history

**Multi-session-per-chat support:**
From `gateway/run.py:2390-2397`:
- Config option `group_sessions_per_user` — if True (default), each user in a group gets their own session
- Config option `thread_sessions_per_user` — if True, each user in a Discord/Slack thread gets their own session
- Otherwise, entire group/channel shares one session

---

## 4. Message Flow and Streaming Output

### Inbound (Platform → Agent → Platform)

From `gateway/run.py:3350-4250`:

1. **Platform adapter receives message**
   - e.g., Telegram handler fires with an `Update` object
   - Adapter normalizes to `MessageEvent` and calls `self.handle_message(event)`

2. **Gateway routes to message handler** (`_handle_message()` at line 3350)
   ```python
   async def _handle_message(self, event: MessageEvent) -> str:
       """
       Main message handler. Called by all platform adapters.
       Returns optional response string.
       """
   ```

3. **Agent is spawned** (lines 3380-3450)
   - Check if session exists; create or resume
   - Load conversation history from `session_store.load_transcript(session_id)`
   - Instantiate `AIAgent(history=..., session_id=..., ...)` or reuse cached agent
   - Inject dynamic system prompt with session context (from `build_session_context_prompt()`)

4. **Agent streams response** (lines 3500-3750)
   - `stream_consumer.StreamConsumer(agent)` captures tokens as they arrive
   - Tokens are either:
     - **Text deltas** — accumulated and sent to platform in chunks
     - **Tool calls** — executed, results injected back into agent
     - **Final message** — returned from `_handle_message()`

5. **Response sent to platform** (lines 3750-3900)
   - Platform adapter receives response string from `_handle_message()`
   - If streaming was enabled, partial messages already sent via `_send_with_retry()`
   - Adapter calls `await self.send(chat_id, response, ...)` or `send_image()`, `send_voice()`, etc.
   - Result stored in session transcript

### Streaming Architecture (StreamConsumer)

From `gateway/stream_consumer.py:1-100`:

The `StreamConsumer` is a callback-based listener that captures agent output:

```python
class StreamConsumer:
    def __init__(self, agent: AIAgent):
        self.agent = agent
        self.callbacks = {
            'on_text_delta': [],     # token arrived
            'on_tool_start': [],      # tool call about to execute
            'on_tool_result': [],     # tool result available
            'on_message_done': [],    # agent finished
        }
```

The adapter can register callbacks:

```python
consumer.on(
    'on_text_delta',
    lambda token: asyncio.create_task(self.send(chat_id, token))
)
```

This allows **streaming without blocking the event loop** — tokens flow through the callback, and the adapter sends them asynchronously.

### Token Streaming for Platforms

From `gateway/platforms/base.py:1640-1750`:

When a response is generated and `stream_enabled` is True on the platform config:

```python
# In _process_message_background():
if response:
    media_files, response = self.extract_media(response)
    images, text_content = self.extract_images(response)
    
    # Send text in chunks
    for chunk in self.truncate_message(text_content, max_length=4096):
        result = await self._send_with_retry(
            chat_id=event.source.chat_id,
            content=chunk,
            reply_to=event.message_id,
        )
```

If the platform doesn't support streaming, it just sends the final response.

---

## 5. Closest Reference Platform: Discord Adapter

From `gateway/platforms/discord.py` (best match for taskboard's use case):

**Why Discord?** Like taskboard, it needs to:
- Maintain long-lived bidirectional communication (WebSocket)
- Handle multiple concurrent conversations (per-channel + per-DM)
- Survive guild/connection drops and reconnect automatically
- Support both real-time updates and queued messages

**Key patterns from Discord adapter:**

1. **Connection management** (lines ~200-400):
   ```python
   async def connect(self) -> bool:
       bot = commands.Bot(...)  # Long-lived WebSocket connection
       @bot.event
       async def on_message(message):
           if message.author.bot:
               return
           event = self._build_message_event(message)
           await self.handle_message(event)  # Inject into gateway
       await bot.start(self.config.token)  # Blocks, runs event loop
   ```

2. **Session tracking** (lines ~1000-1200):
   ```python
   self._active_sessions = {}  # per-session interrupt signals
   self._pending_messages = {}  # queued while agent runs
   # When message arrives:
   if session_key not in self._active_sessions:
       asyncio.create_task(self._process_message_background(...))
   ```

3. **Interrupt handling** (lines ~1300-1400):
   ```python
   if session_key in self._active_sessions:
       # Signal the running task to interrupt
       self._active_sessions[session_key].set()
       # Queue this message for processing after current task finishes
       self._pending_messages[session_key] = event
   ```

4. **Output streaming** (lines ~1500-1700):
   ```python
   async def send(self, chat_id: str, content: str, ...) -> SendResult:
       channel = bot.get_channel(int(chat_id))
       msg = await channel.send(content)
       return SendResult(success=True, message_id=str(msg.id))
   ```

---

## 6. The API Server Platform

From `gateway/platforms/api_server.py:1-200`:

**Is it itself a platform? Yes.** It's registered in the enum as `Platform.API_SERVER` and instantiated like any other adapter.

**What does it do?**

- Runs an `aiohttp` web server (default port 8642, configurable)
- Exposes HTTP endpoints:
  - `POST /v1/chat/completions` — OpenAI-compatible Chat Completions API
  - `POST /v1/responses` — OpenAI Responses API (stateful with `previous_response_id`)
  - `GET /v1/responses/{response_id}` — retrieve stored response
  - `GET /v1/runs/{run_id}/events` — SSE stream of lifecycle events
  - `POST /v1/runs` — start a run, get `run_id` immediately (202 Accepted)

**Key insight:** API server is a **translation layer**, not a true platform plugin in the sense you need. It wraps Hermes' async agent execution in HTTP verbs, but:

1. **Clients still drive the message loop** — each HTTP request starts an agent run; responses are tied to client connection
2. **No session continuity across HTTP requests** — unless the client explicitly passes `X-Hermes-Session-Id` header or `previous_response_id` parameter
3. **If HTTP client disconnects, run continues** (for `/v1/runs`), but taskboard as an HTTP client gets no feedback

**Why it's not a solution for taskboard:**
- Taskboard already uses this model (SSE at `/v1/responses` with streaming)
- Problem: if taskboard process dies, Hermes doesn't know and keeps running the agent
- Solution sought: a mechanism where the control surface (taskboard-go) can reliably check in with a live Hermes session without HTTP polling

---

## 7. Concurrency and Isolation

### Event Loop Structure

From `gateway/run.py:1701-2000`:

**Single asyncio event loop** — all adapters and background tasks run in one thread, yielding at `await` points:

```python
# In gateway/run.py:
asyncio.run(self._run_gateway_main_loop())

async def _run_gateway_main_loop(self) -> int:
    # All adapters' connect() coroutines run here
    tasks = [adapter.connect() for adapter in self.adapters.values()]
    await asyncio.gather(*tasks)  # Concurrently in same loop
    
    # Background watchers
    asyncio.create_task(self._session_expiry_watcher())
    asyncio.create_task(self._platform_reconnect_watcher())
    
    # Wait for shutdown signal
    await self._shutdown_event.wait()
```

**Blocking operations = deadlock:** If any adapter calls a blocking function without `await asyncio.to_thread()`, it stalls the entire gateway loop. Existing adapters (Telegram, Discord, Slack) avoid this by using async libraries (python-telegram-bot, discord.py, slack-bolt all have async APIs).

### Multi-Adapter Concurrency

From `gateway/platforms/base.py:1482-1570`:

Each adapter maintains:
```python
self._active_sessions: Dict[str, asyncio.Event] = {}  # per-session guard
self._pending_messages: Dict[str, MessageEvent] = {}   # queued during agent run
self._background_tasks: set = set()                    # outstanding tasks
```

When a message arrives:
1. Adapter checks `if session_key in self._active_sessions` (synchronous check)
2. If not present, spawns `asyncio.create_task(self._process_message_background(event))`
3. Task runs concurrently with other adapters' tasks
4. All tasks access shared `self.session_store` and `self.delivery_router`, protected by:
   - `threading.Lock` on agent cache (line 585)
   - Lock on session store's internal `_entries` dict (line 1606)

**Isolation guarantee:** Each session's agent run is exclusive (guarded by `_active_sessions[session_key]`), but multiple sessions (different chats) can run agents concurrently.

### Shared State

From `gateway/run.py:551-600`:

```python
self.session_store = SessionStore(...)       # shared, threadsafe
self.delivery_router = DeliveryRouter(...)   # shared
self._agent_cache: Dict[str, tuple] = {}     # shared, protected by _agent_cache_lock
self._running_agents: Dict[str, Any] = {}    # shared
self._pending_approvals: Dict[str, Dict] = {} # shared
```

All access is guarded by:
- `threading.Lock` (for agent cache, PII redaction)
- `SessionStore._lock` (for session mutations)
- Asyncio's implicit GIL (no true parallelism in CPython anyway)

**Risk:** If a platform adapter crashes (unhandled exception in `connect()` or `handle_message()`), it doesn't deadlock the gateway. Exceptions in `_process_message_background()` are caught and logged (line 3750+). However, if an exception is raised in `send()` and not caught, the task fails silently.

---

## 8. Configuration and Enablement

### How Platforms Are Enabled

From `gateway/config.py` and `gateway/run.py:1812-1820`:

```python
# Load config from ~/.hermes/config.yaml or env vars
config = load_gateway_config()

# Env vars override config (from _apply_env_overrides in run.py:100-210)
TELEGRAM_TOKEN = os.getenv("TELEGRAM_TOKEN")
if TELEGRAM_TOKEN:
    config.platforms[Platform.TELEGRAM] = PlatformConfig(
        enabled=True,
        token=TELEGRAM_TOKEN,
    )

# Startup iterates all platforms
for platform, platform_config in config.platforms.items():
    if not platform_config.enabled:
        continue
    adapter = self._create_adapter(platform, platform_config)
    success = await adapter.connect()
```

### To Add a New Platform

From `ADDING_A_PLATFORM.md` (lines 10-50):

1. Create `gateway/platforms/your_platform.py` with adapter class inheriting `BasePlatformAdapter`
2. Add enum entry to `gateway/config.py:Platform`
3. Add env var loading to `gateway/config.py:_apply_env_overrides()`
4. Add factory case to `gateway/run.py:_create_adapter()` (lines 2399-2550)
5. Add entry to authorization allowlists in `gateway/run.py:_is_user_authorized()` (if needed)
6. Update system prompt hints in `agent/prompt_builder.py:PLATFORM_HINTS` (optional but recommended)

**No fork required.** If you build the adapter as a standard Python package, users can:
- Monkeypatch at runtime
- Or you can convince Hermes maintainers to merge it
- Or you fork the `hermes-agent` repo and build your own distribution

**Egg-info entry points** are mentioned in ADDING_A_PLATFORM.md but not actually used in the codebase — all platforms are hardcoded in `_create_adapter()`.

---

## 9. What Hermes Persists

### Session Storage

From `gateway/session.py:450-600`:

**Path:** `~/.hermes/sessions.json` (JSON index) + `~/.hermes/sessions/{session_id}/` (per-session directory)

**Index entry:**
```json
{
  "session_key": "telegram:12345",
  "session_id": "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx",
  "created_at": "2025-04-21T10:00:00",
  "updated_at": "2025-04-21T10:05:30",
  "platform": "telegram",
  "display_name": "Alice",
  "chat_type": "dm",
  "input_tokens": 450,
  "output_tokens": 1200,
  "cache_read_tokens": 100,
  "cache_write_tokens": 50,
  "memory_flushed": true,
  "suspended": false,
  "origin": { "platform": "telegram", "chat_id": "12345", ... }
}
```

**Per-session directory:**
- `transcript.json` — full conversation history with tool calls and results
- `memory.md`, `user.md` — persisted memories (from memory tool)

**Lifetime:** Sessions are never deleted by the gateway (unless explicitly `/reset` by user or dropped after idle timeout per reset policy)

### Conversation History

From `gateway/run.py:3522` and `gateway/session.py` storage:

Each message is appended to `transcript.json`:
```json
{
  "role": "user|assistant",
  "content": "...",
  "tool_calls": [
    {"id": "...", "type": "function", "function": {"name": "...", "arguments": "..."}}
  ],
  "tool_results": [
    {"tool_use_id": "...", "content": "..."}
  ]
}
```

History is loaded fresh on each message (not cached in memory), allowing gateway restarts without losing context.

### Tool Output Persistence

From `tools/` and `cron/` directories:

- **Cron job outputs** → `~/.hermes/cron/output/{job_id}/{run_id}/` (files, stdout, stderr)
- **Session execution logs** → `~/.hermes/sessions/{session_id}/` (implicitly, via transcript)
- **Memory snapshots** → `~/.hermes/memory/` and `~/.hermes/user_profiles/`

---

## 10. Gaps and Risks for Taskboard-as-Plugin

### Risk #1: Python-Go Integration

**Problem:** The gateway is pure Python. Taskboard is pure Go. To make taskboard a platform plugin requires bridging two runtimes.

**Options:**

A. **Subprocess with IPC** — Taskboard runs as a subprocess, exchanges messages with the Python plugin via stdin/stdout or Unix socket
   - **Pro:** Clear process boundary; if taskboard crashes, plugin restarts it
   - **Con:** Extra latency, serialization overhead, potential hangs if taskboard is unresponsive
   - **Risk:** Plugin must implement timeouts and watchdogs; blocking I/O kills the gateway

B. **HTTP + plugin wrapper** — Taskboard exposes HTTP/WebSocket; Python plugin proxies to it
   - **Pro:** Taskboard can run anywhere; no subprocess coupling
   - **Con:** Same as current SSE model — if taskboard dies, plugin doesn't know
   - **Risk:** Not fundamentally different from current approach

C. **gRPC + async Python stub** — Taskboard exposes gRPC; Python plugin uses `asyncio` gRPC client
   - **Pro:** Efficient binary protocol; async-native; can timeout cleanly
   - **Con:** Extra dependency; still requires taskboard reachability
   - **Risk:** Similar to HTTP but better error handling

**Recommendation:** Option A (subprocess) if you want true in-process coupling. Requires:
- Robust subprocess supervision (restart on crash, timeout on hang)
- Non-blocking IPC (asyncio library for the subprocess protocol, e.g., `asyncio.subprocess`)
- Explicit message framing (e.g., JSON-per-line or protobuf)
- Watchdog task in the plugin to detect hung taskboard and restart

### Risk #2: Deadlock Hazards

**If your subprocess Go service blocks on I/O** (e.g., waiting for a database), the Python plugin task that's waiting for a response will block the entire asyncio loop, stalling all other adapters and background tasks.

**Mitigations:**
1. Use `asyncio.wait_for(timeout=...)` on all subprocess communication
2. Implement a watchdog in the plugin that detects hung taskboard and kills/restarts it
3. Ensure taskboard never blocks indefinitely on external services; use its own timeout/retry logic
4. Use `asyncio.to_thread()` if you must use blocking library calls, so blocking doesn't stall the loop

### Risk #3: Plugin Crashes Kill Gateway

If the Python plugin throws an unhandled exception and crashes:
- The platform adapter is removed from `self.adapters`
- Other adapters continue (they're still in the loop)
- Taskboard messages stop being processed
- On restart, the platform reconnection watcher (line 1990) retries connection every 30 seconds

This is actually OK — it's the same behavior as any adapter (Telegram, Discord) losing connection. But your subprocess must be resilient to restarts.

### Risk #4: Session Continuity

If taskboard process dies and restarts, it has no way to know which Hermes sessions are active. The plugin can enumerate them via:

```python
# In plugin code:
session_keys = list(self.session_store._entries.keys())  # Requires access to _session_store
```

But this is internal API. If you need to enumerate sessions, you should:
- Add a public method to `SessionStore`: `def get_active_sessions() -> List[str]`
- Or query `~/.hermes/sessions.json` directly (hacky but works)

### Risk #5: Message Ordering

If taskboard processes messages concurrently (multiple goroutines), it might send responses out of order. The gateway expects responses from `_handle_message()` to be in order. If your plugin gets responses back from taskboard out of order, it can't tell the gateway to reorder them.

**Mitigation:** Ensure message processing in taskboard is sequential per session, or implement a sequence-number protocol.

### Risk #6: Configuration and Restart Overhead

Taskboard can't be configured via env vars like other platforms (there's no `TASKBOARD_TOKEN`, etc.). You'd need to:
- Either hardcode taskboard's spawn command in the plugin
- Or add a new config section to `~/.hermes/config.yaml` for taskboard
- Gateway doesn't support dynamic plugin loading — restarting the gateway to reconfigure taskboard

---

## 11. Recommended Next Steps

1. **Build a simple prototype** — Python plugin that spawns a Go subprocess and exchanges JSON messages over stdin/stdout. Implement:
   - Non-blocking message send/receive with timeout
   - Subprocess supervision (restart on crash, detect hang via watchdog)
   - Integration with `BasePlatformAdapter.handle_message()` so Hermes messages flow to taskboard

2. **Test session persistence** — Verify that if the plugin (and taskboard) restart, active sessions are properly resumed. Specifically:
   - Check that `session_store` is correctly accessible from the plugin
   - Confirm transcript history is preserved across restarts

3. **Load test concurrency** — Send messages to multiple chats concurrently and verify:
   - No deadlocks (agent runs in 3+ sessions simultaneously)
   - Proper interrupt handling (message to busy session queues correctly)
   - Responses are not interleaved

4. **Implement watchdog and timeouts** — Add a background task in the plugin that:
   - Detects if taskboard process has hung (e.g., no response to ping in 10 seconds)
   - Kills and restarts the process gracefully
   - Retries queued messages after restart

5. **Consider the "thin plugin" approach** — Instead of taskboard-as-plugin, what if you built a minimal Python shim that:
   - Registers as a platform plugin
   - Delegates all actual logic to taskboard-go via HTTP/gRPC
   - Acts as a thin adapter, not a protocol translator
   This trades session coupling for simplicity; you lose some benefits but avoid the subprocess complexity.

---

## Appendix: File References

### Platform Plugin System
- **Base class:** `/home/kasm-user/.hermes/hermes-agent/gateway/platforms/base.py:779-950`
- **Integration checklist:** `/home/kasm-user/.hermes/hermes-agent/gateway/platforms/ADDING_A_PLATFORM.md`
- **Factory method:** `/home/kasm-user/.hermes/hermes-agent/gateway/run.py:2383-2550`

### Message Routing
- **Handler signature:** `/home/kasm-user/.hermes/hermes-agent/gateway/platforms/base.py:1482-1570`
- **Session dispatch:** `/home/kasm-user/.hermes/hermes-agent/gateway/run.py:3350-3450`

### Session Persistence
- **Session model:** `/home/kasm-user/.hermes/hermes-agent/gateway/session.py:65-450`
- **Transcript storage:** `/home/kasm-user/.hermes/hermes-agent/gateway/session.py:500-600`

### Concurrency
- **Event loop setup:** `/home/kasm-user/.hermes/hermes-agent/gateway/run.py:1701-1950`
- **Interrupt handling:** `/home/kasm-user/.hermes/hermes-agent/gateway/platforms/base.py:1500-1570`

### Reference Implementations
- **Telegram adapter (simple):** `/home/kasm-user/.hermes/hermes-agent/gateway/platforms/telegram.py:1-200`
- **Discord adapter (complex):** `/home/kasm-user/.hermes/hermes-agent/gateway/platforms/discord.py:1-300`
- **API Server (translation layer):** `/home/kasm-user/.hermes/hermes-agent/gateway/platforms/api_server.py:1-400`

