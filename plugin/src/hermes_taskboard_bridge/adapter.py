"""Platform adapter that bridges Hermes sessions to an external taskboard
over a single outbound WebSocket.

Hermes runs on the local machine; taskboard runs the WebSocket *server*
(typically at ``ws://127.0.0.1:1900/api/plugin/ws``). The plugin dials
out and holds the connection open for as long as Hermes lives, with
exponential backoff + jitter on reconnect. Session state stays inside
Hermes, so even if taskboard crashes and restarts, in-flight agent runs
keep going — the ring buffer in this adapter replays events produced
during the outage when taskboard comes back.
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import random
import socket
import time
import uuid
from collections import deque
from dataclasses import asdict, is_dataclass
from datetime import datetime
from typing import Any, Deque, Dict, Optional

try:
    # Pin to the asyncio client explicitly. The generic `websockets.connect`
    # can be monkey-patched by the Feishu adapter (lark_oapi's ws client
    # replaces it with a coroutine-returning wrapper), which then breaks
    # ``async with``. The asyncio path can't be shimmed the same way.
    from websockets.asyncio.client import ClientConnection, connect as ws_connect
    from websockets.exceptions import ConnectionClosed, WebSocketException
    WEBSOCKETS_AVAILABLE = True
except ImportError:  # pragma: no cover
    WEBSOCKETS_AVAILABLE = False
    ws_connect = None  # type: ignore[assignment]
    ClientConnection = None  # type: ignore[assignment]

logger = logging.getLogger(__name__)

RING_CAPACITY = 1024


def check_taskboard_bridge_requirements() -> bool:
    """Factory-time probe: return True iff the plugin is allowed to start."""
    if not WEBSOCKETS_AVAILABLE:
        logger.warning(
            "[Taskboard] `websockets` package not installed. "
            "Run: pip install 'websockets>=11.0'"
        )
        return False
    if not os.getenv("TASKBOARD_WS_URL"):
        logger.warning(
            "[Taskboard] TASKBOARD_WS_URL not set — plugin will not start. "
            "Add it to ~/.hermes/.env or set via env before launch."
        )
        return False
    return True


def build_adapter_class():
    """Return a subclass of Hermes's BasePlatformAdapter.

    We lazy-import Hermes so that this module can be imported without the
    Hermes symbols being resolvable yet (e.g. during package introspection
    by pip). Called from ``runtime.apply_patches`` after we're confident
    Hermes is installed in the same environment.
    """
    from gateway.config import Platform, PlatformConfig  # noqa: WPS433
    from gateway.platforms.base import (
        BasePlatformAdapter,
        MessageEvent,
        MessageType,
        SendResult,
    )
    from gateway.session import SessionSource

    class TaskboardBridgeAdapter(BasePlatformAdapter):
        """Bridge Hermes sessions to an external taskboard WS server."""

        # Tell Hermes's stream_consumer that we accept edit_message calls.
        # That unlocks token-level streaming: the agent's partial output is
        # sent via repeated edits to the same message_id, finalised with a
        # send() when the turn completes.
        SUPPORTS_MESSAGE_EDITING = True

        def __init__(self, config: PlatformConfig):
            super().__init__(config, Platform.TASKBOARD)  # type: ignore[attr-defined]
            extra = config.extra or {}
            self._ws_url: str = (
                extra.get("ws_url")
                or os.getenv("TASKBOARD_WS_URL", "ws://127.0.0.1:1900/api/plugin/ws")
            )
            self._token: str = extra.get("token") or os.getenv("TASKBOARD_PLUGIN_TOKEN", "")
            self._reconnect_min: float = float(
                extra.get("reconnect_min", os.getenv("TASKBOARD_RECONNECT_MIN", "1.0"))
            )
            self._reconnect_max: float = float(
                extra.get("reconnect_max", os.getenv("TASKBOARD_RECONNECT_MAX", "30.0"))
            )
            # Stable identity used by taskboard to route tasks to this Hermes
            # host. Priority: explicit env var → config.yaml extra.hermes_id →
            # socket.gethostname(). Taskboard keys its connected-plugin table
            # on this value; two plugins announcing the same hermes_id means
            # the later one replaces the earlier.
            self._hermes_id: str = (
                os.getenv("TASKBOARD_HERMES_ID")
                or extra.get("hermes_id")
                or socket.gethostname()
                or "unknown"
            )
            self._hostname: str = socket.gethostname() or "unknown"
            # Per-connection random nonce — lets the server disambiguate
            # reconnects from the same host.
            self._client_id: str = f"{self._hermes_id}-{uuid.uuid4().hex[:8]}"
            self._conn: Optional[ClientConnection] = None
            self._dial_task: Optional[asyncio.Task] = None
            self._seq: int = 0
            self._buffers: Dict[str, Deque[Dict[str, Any]]] = {}
            self._last_ack: Dict[str, int] = {}

        # -----------------------------------------------------------------
        # Platform lifecycle
        # -----------------------------------------------------------------

        async def connect(self) -> bool:
            if not WEBSOCKETS_AVAILABLE:
                logger.error("[Taskboard] websockets missing, cannot connect")
                return False
            self._running = True
            self._dial_task = asyncio.create_task(
                self._dial_loop(), name="taskboard-dial"
            )
            logger.info(
                "[Taskboard] adapter started; dialing %s (hermes_id=%s client_id=%s)",
                self._ws_url,
                self._hermes_id,
                self._client_id,
            )
            self._mark_connected()
            return True

        async def disconnect(self) -> None:
            self._running = False
            if self._dial_task:
                self._dial_task.cancel()
                try:
                    await self._dial_task
                except asyncio.CancelledError:
                    pass
            if self._conn is not None:
                try:
                    await self._conn.close()
                except Exception:
                    pass
                self._conn = None
            self._mark_disconnected()
            logger.info("[Taskboard] adapter stopped")

        # -----------------------------------------------------------------
        # Outbound: agent → taskboard
        # -----------------------------------------------------------------

        async def send(
            self,
            chat_id: str,
            content: str,
            reply_to: Optional[str] = None,
            metadata: Optional[Dict[str, Any]] = None,
        ) -> SendResult:
            """Hermes calls this with assistant output. Push it to taskboard."""
            self._seq += 1
            event = {
                "type": "agent_event",
                "attempt_id": chat_id,
                "seq": self._seq,
                "event": {
                    "kind": "assistant_message",
                    "content": content,
                    "reply_to": reply_to,
                    "metadata": metadata or {},
                    "ts": time.time(),
                },
            }
            self._buffer(chat_id, event)
            await self._push(event)

            done = {
                "type": "agent_done",
                "attempt_id": chat_id,
                "summary": content[:400],
                "ts": time.time(),
            }
            self._buffer(chat_id, done)
            await self._push(done)
            return SendResult(success=True, message_id=f"tb-{self._seq}")

        async def send_typing(
            self, chat_id: str, metadata: Optional[Dict[str, Any]] = None
        ) -> None:
            return

        async def send_image(self, *args: Any, **kwargs: Any) -> SendResult:
            return SendResult(
                success=False, error="images not supported on taskboard bridge"
            )

        async def edit_message(
            self,
            chat_id: str,
            message_id: str,
            content: str,
        ) -> SendResult:
            """Streaming update: push the growing assistant content as a
            `stream_update` frame so the taskboard UI can overwrite the
            in-progress bubble instead of appending noise. Hermes's
            stream_consumer calls this as the model produces tokens when
            streaming is enabled in the Hermes streaming config + the
            model provider actually returns SSE deltas."""
            self._seq += 1
            frame = {
                "type": "agent_event",
                "attempt_id": chat_id,
                "seq": self._seq,
                "event": {
                    "kind": "stream_update",
                    "message_id": message_id,
                    "content": content,
                    "ts": time.time(),
                },
            }
            self._buffer(chat_id, frame)
            await self._push(frame)
            return SendResult(success=True, message_id=message_id)

        async def get_chat_info(self, chat_id: str) -> Dict[str, Any]:
            return {"name": f"attempt {chat_id}", "type": "dm", "chat_id": chat_id}

        # -----------------------------------------------------------------
        # Inbound: taskboard → agent
        # -----------------------------------------------------------------

        async def _inject_user_message(self, attempt_id: str, text: str) -> None:
            event = MessageEvent(
                text=text,
                message_type=MessageType.TEXT,
                source=SessionSource(
                    platform=Platform.TASKBOARD,  # type: ignore[attr-defined]
                    chat_id=str(attempt_id),
                    chat_name=f"attempt {attempt_id}",
                    chat_type="dm",
                    user_id="taskboard",
                    user_name="Taskboard",
                ),
                message_id=f"{attempt_id}-{uuid.uuid4().hex[:8]}",
                timestamp=datetime.now(),
            )
            logger.debug(
                "[Taskboard] inject user_message attempt_id=%s len=%d",
                attempt_id,
                len(text),
            )
            await self.handle_message(event)

        async def _inject_cancel(self, attempt_id: str) -> None:
            """Route the cancel through Hermes's native /stop — the busy-session
            guard in BasePlatformAdapter interprets /stop as interrupt."""
            event = MessageEvent(
                text="/stop",
                message_type=MessageType.TEXT,
                source=SessionSource(
                    platform=Platform.TASKBOARD,  # type: ignore[attr-defined]
                    chat_id=str(attempt_id),
                    chat_name=f"attempt {attempt_id}",
                    chat_type="dm",
                    user_id="taskboard",
                    user_name="Taskboard",
                ),
                message_id=f"{attempt_id}-cancel-{uuid.uuid4().hex[:8]}",
                internal=True,
                timestamp=datetime.now(),
            )
            logger.info("[Taskboard] cancel attempt_id=%s", attempt_id)
            await self.handle_message(event)

        # -----------------------------------------------------------------
        # Connection management
        # -----------------------------------------------------------------

        async def _dial_loop(self) -> None:
            delay = self._reconnect_min
            while self._running:
                try:
                    async with ws_connect(  # type: ignore[misc]
                        self._ws_url,
                        ping_interval=20,
                        ping_timeout=10,
                        max_size=4 * 1024 * 1024,
                    ) as ws:
                        self._conn = ws
                        delay = self._reconnect_min
                        await self._on_connected(ws)
                except asyncio.CancelledError:
                    raise
                except (ConnectionClosed, WebSocketException, OSError) as e:
                    logger.warning(
                        "[Taskboard] dial failed / dropped: %s; retry in %.1fs",
                        e,
                        delay,
                    )
                except Exception as e:  # noqa: BLE001
                    logger.exception("[Taskboard] unexpected dial error: %s", e)
                finally:
                    self._conn = None
                if not self._running:
                    return
                jitter = random.uniform(0, delay * 0.3)
                await asyncio.sleep(delay + jitter)
                delay = min(self._reconnect_max, delay * 2)

        async def _on_connected(self, ws: "ClientConnection") -> None:
            logger.info("[Taskboard] connected to %s", self._ws_url)
            await self._send_frame(
                ws,
                {
                    "type": "hello_ack",
                    "gateway_version": os.getenv("HERMES_VERSION", "unknown"),
                    "plugin_version": "0.1.0",
                    "hermes_id": self._hermes_id,
                    "hostname": self._hostname,
                    "client_id": self._client_id,
                    "token": self._token,
                },
            )

            # Replay buffered events for anything taskboard hasn't ack'd.
            for attempt_id, buf in self._buffers.items():
                last = self._last_ack.get(attempt_id, 0)
                for frame in list(buf):
                    if frame.get("seq", 0) > last:
                        await self._send_frame(ws, frame)

            try:
                async for raw in ws:
                    await self._on_frame(raw)
            except ConnectionClosed:
                logger.info("[Taskboard] connection closed by remote")
                return

        async def _send_frame(
            self, ws: "ClientConnection", payload: Dict[str, Any]
        ) -> None:
            try:
                await ws.send(json.dumps(payload, default=_json_default))
            except ConnectionClosed:
                pass

        async def _push(self, payload: Dict[str, Any]) -> None:
            ws = self._conn
            if ws is None:
                return
            try:
                await ws.send(json.dumps(payload, default=_json_default))
            except ConnectionClosed:
                pass

        async def _on_frame(self, raw: Any) -> None:
            try:
                msg = json.loads(raw)
            except (ValueError, TypeError):
                logger.warning("[Taskboard] dropping non-JSON frame")
                return
            mtype = msg.get("type")
            if mtype == "hello":
                logger.info(
                    "[Taskboard] hello from client_id=%s taskboard_version=%s",
                    msg.get("client_id"),
                    msg.get("taskboard_version"),
                )
            elif mtype == "send_message":
                attempt_id = str(msg.get("attempt_id") or "")
                text = msg.get("text") or ""
                if not attempt_id or not text:
                    logger.warning("[Taskboard] send_message missing fields: %s", msg)
                    return
                await self._inject_user_message(attempt_id, text)
            elif mtype == "cancel":
                attempt_id = str(msg.get("attempt_id") or "")
                if attempt_id:
                    await self._inject_cancel(attempt_id)
            elif mtype == "ping":
                await self._push({"type": "pong", "ts": msg.get("ts")})
            elif mtype == "ack":
                attempt_id = str(msg.get("attempt_id") or "")
                seq = int(msg.get("seq") or 0)
                if attempt_id and seq:
                    prev = self._last_ack.get(attempt_id, 0)
                    self._last_ack[attempt_id] = max(prev, seq)
            else:
                logger.debug("[Taskboard] ignoring unknown frame type: %s", mtype)

        # -----------------------------------------------------------------
        # Ring buffer
        # -----------------------------------------------------------------

        def _buffer(self, attempt_id: str, frame: Dict[str, Any]) -> None:
            buf = self._buffers.get(attempt_id)
            if buf is None:
                buf = deque(maxlen=RING_CAPACITY)
                self._buffers[attempt_id] = buf
            buf.append(frame)

    return TaskboardBridgeAdapter


def _json_default(obj: Any) -> Any:
    if is_dataclass(obj):
        return asdict(obj)
    if isinstance(obj, datetime):
        return obj.isoformat()
    if isinstance(obj, set):
        return list(obj)
    return str(obj)
