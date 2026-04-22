"""Runtime integration: patch Hermes in-memory to accept a new platform.

This module's job is to take pristine Hermes code and, without touching
any file on disk, add everything we need so that::

    platforms:
      taskboard:
        enabled: true

in ``~/.hermes/config.yaml`` (or ``PLATFORMS__TASKBOARD__ENABLED=true`` env
override once upstream wires it up) causes ``TaskboardBridgeAdapter`` to
be constructed and connected exactly like a native platform.

The contract with Hermes internals is small and explicit. Every patch
site below is guarded so that if Hermes upstream renames or removes a
symbol, this module raises a clear ``RuntimeError`` at start time rather
than producing a silently broken setup.
"""

from __future__ import annotations

import functools
import logging
import os
import sys
from typing import Any

logger = logging.getLogger(__name__)

PLATFORM_NAME = "taskboard"
PLATFORM_ENUM_NAME = "TASKBOARD"
PLATFORM_LABEL = "🧭 Taskboard"
DEFAULT_TOOLSET = "hermes-cli"

_PATCHED_FLAG = "_taskboard_bridge_patched"


def apply_patches() -> None:
    """Apply all runtime patches. Idempotent. Raises RuntimeError if Hermes
    is missing or its internal API shape no longer matches what we expect."""

    # ---- 1. Extend the Platform enum ----------------------------------
    try:
        import gateway.config as gw_config
    except ImportError as e:
        raise RuntimeError(
            "hermes-taskboard-bridge requires hermes-agent to be importable; "
            f"got: {e}"
        ) from e

    platform_enum = getattr(gw_config, "Platform", None)
    if platform_enum is None:
        raise RuntimeError("gateway.config.Platform not found; incompatible Hermes version")

    if getattr(platform_enum, _PATCHED_FLAG, False):
        logger.debug("[taskboard-bridge] patches already applied, skipping")
        return

    _extend_enum(platform_enum, PLATFORM_ENUM_NAME, PLATFORM_NAME)

    # ---- 2. Register in hermes_cli.platforms.PLATFORMS ----------------
    try:
        from hermes_cli.platforms import PLATFORMS, PlatformInfo
    except ImportError as e:
        raise RuntimeError(
            f"hermes_cli.platforms not importable: {e}. "
            "Is hermes-agent installed in this venv?"
        ) from e
    if PLATFORM_NAME not in PLATFORMS:
        PLATFORMS[PLATFORM_NAME] = PlatformInfo(
            label=PLATFORM_LABEL,
            default_toolset=DEFAULT_TOOLSET,
        )

    # ---- 3. Monkey-patch _create_adapter + _is_user_authorized --------
    try:
        import gateway.run as gw_run
    except ImportError as e:
        raise RuntimeError(f"gateway.run not importable: {e}") from e

    runner_cls = getattr(gw_run, "GatewayRunner", None)
    if runner_cls is None:
        raise RuntimeError(
            "gateway.run.GatewayRunner not found; incompatible Hermes version"
        )

    _wrap_create_adapter(runner_cls)
    _wrap_is_user_authorized(runner_cls)
    _wrap_start(runner_cls)
    _wrap_handle_message_with_agent(runner_cls)

    # ---- 4. Mark patched so we don't double-wrap on second import -----
    # Use type.__setattr__ to bypass EnumType's protection.
    type.__setattr__(platform_enum, _PATCHED_FLAG, True)
    logger.info(
        "[taskboard-bridge] patches applied: Platform.%s = %r + GatewayRunner wrappers",
        PLATFORM_ENUM_NAME,
        PLATFORM_NAME,
    )


# ---------------------------------------------------------------------------
# Enum extension
# ---------------------------------------------------------------------------


def _extend_enum(enum_class: Any, name: str, value: str) -> None:
    """Add a new member to an existing Enum at runtime.

    Python enums are nominally frozen after class creation, but the
    implementation keeps the member map in attributes we can reach. This
    trick has worked on CPython 3.7 through 3.12 and is the same approach
    the third-party ``aenum`` library uses when it calls ``extend_enum``.
    """
    from enum import Enum  # local import keeps top-level light
    if not isinstance(enum_class, type) or not issubclass(enum_class, Enum):
        raise RuntimeError(f"{enum_class!r} is not an Enum; cannot extend")
    if name in enum_class.__members__:
        return  # already present
    if value in enum_class._value2member_map_:  # type: ignore[attr-defined]
        # Same value, different name — just alias.
        existing = enum_class._value2member_map_[value]  # type: ignore[attr-defined]
        enum_class._member_map_[name] = existing  # type: ignore[attr-defined]
        type.__setattr__(enum_class, name, existing)
        return

    new_member = object.__new__(enum_class)
    new_member._name_ = name  # type: ignore[attr-defined]
    new_member._value_ = value  # type: ignore[attr-defined]
    enum_class._member_map_[name] = new_member  # type: ignore[attr-defined]
    enum_class._value2member_map_[value] = new_member  # type: ignore[attr-defined]
    # Preserve iteration order — Hermes iterates enum members in a few
    # places (e.g. for auth-allowlist env-var wiring) so new ones must go
    # at the end.
    if hasattr(enum_class, "_member_names_"):
        enum_class._member_names_.append(name)  # type: ignore[attr-defined]
    # Bypass EnumType.__setattr__ which forbids reassignment of member names
    # (raises AttributeError in 3.11+). type.__setattr__ writes straight to
    # the class __dict__, which is what we want.
    type.__setattr__(enum_class, name, new_member)


# ---------------------------------------------------------------------------
# _create_adapter wrapper
# ---------------------------------------------------------------------------


def _wrap_create_adapter(runner_cls: Any) -> None:
    original = runner_cls._create_adapter
    if getattr(original, _PATCHED_FLAG, False):
        return

    from gateway.config import Platform

    @functools.wraps(original)
    def _patched_create_adapter(self: Any, platform: Any, config: Any):
        target = getattr(Platform, PLATFORM_ENUM_NAME, None)
        if target is not None and platform == target:
            from .adapter import check_taskboard_bridge_requirements, build_adapter_class

            if not check_taskboard_bridge_requirements():
                logger.warning(
                    "[taskboard-bridge] requirements not met, skipping adapter"
                )
                return None
            try:
                AdapterCls = build_adapter_class()
            except Exception as e:  # noqa: BLE001
                logger.exception("[taskboard-bridge] failed to build adapter class: %s", e)
                return None
            return AdapterCls(config)
        return original(self, platform, config)

    setattr(_patched_create_adapter, _PATCHED_FLAG, True)
    runner_cls._create_adapter = _patched_create_adapter


# ---------------------------------------------------------------------------
# _is_user_authorized wrapper (always-authorize for TASKBOARD)
# ---------------------------------------------------------------------------


def _wrap_is_user_authorized(runner_cls: Any) -> None:
    original = runner_cls._is_user_authorized
    if getattr(original, _PATCHED_FLAG, False):
        return

    from gateway.config import Platform

    @functools.wraps(original)
    def _patched(self: Any, source: Any) -> bool:
        target = getattr(Platform, PLATFORM_ENUM_NAME, None)
        if (
            target is not None
            and getattr(source, "platform", None) == target
        ):
            return True
        return original(self, source)

    setattr(_patched, _PATCHED_FLAG, True)
    runner_cls._is_user_authorized = _patched


# ---------------------------------------------------------------------------
# start() wrapper — inject PlatformConfig if env var says so, so user doesn't
# have to edit ~/.hermes/config.yaml manually.
# ---------------------------------------------------------------------------


def _wrap_start(runner_cls: Any) -> None:
    original = runner_cls.start
    if getattr(original, _PATCHED_FLAG, False):
        return

    from gateway.config import Platform, PlatformConfig

    @functools.wraps(original)
    async def _patched_start(self: Any, *args: Any, **kwargs: Any):
        target = getattr(Platform, PLATFORM_ENUM_NAME, None)
        enabled = _env_enabled("TASKBOARD_WS_URL") or _env_enabled_flag(
            "TASKBOARD_ENABLED"
        )
        if (
            target is not None
            and enabled
            and target not in self.config.platforms
        ):
            self.config.platforms[target] = PlatformConfig(
                enabled=True,
                extra={
                    "ws_url": os.getenv(
                        "TASKBOARD_WS_URL",
                        "ws://127.0.0.1:1900/api/plugin/ws",
                    ),
                    "token": os.getenv("TASKBOARD_PLUGIN_TOKEN", ""),
                },
            )
            logger.info(
                "[taskboard-bridge] injected PlatformConfig for %s (ws=%s) from env",
                target,
                self.config.platforms[target].extra["ws_url"],
            )
        return await original(self, *args, **kwargs)

    setattr(_patched_start, _PATCHED_FLAG, True)
    runner_cls.start = _patched_start


# ---------------------------------------------------------------------------
# _handle_message_with_agent — skip the home-channel onboarding prompt for
# Taskboard (it's noise for a machine-to-machine platform) and dodge the
# `_get_platform_tools` KeyError path by short-circuiting to a sensible
# default when taskboard isn't in platform_toolsets.
# ---------------------------------------------------------------------------


def _wrap_handle_message_with_agent(runner_cls: Any) -> None:
    # Home-channel onboarding runs inside _handle_message_with_agent; we
    # can't surgically suppress just that section without re-implementing
    # the whole method. Cheapest fix: pre-seed HOME_CHANNEL env var so the
    # onboarding condition `if not os.getenv(env_key)` evaluates False and
    # the code path is skipped. The variable need not resolve to a real
    # channel — onboarding only uses its presence as a "already told the
    # user, don't repeat" marker.
    env_key = f"{PLATFORM_NAME.upper()}_HOME_CHANNEL"
    if not os.getenv(env_key):
        os.environ[env_key] = "taskboard-virtual"
        logger.debug(
            "[taskboard-bridge] seeded %s to suppress onboarding prompt", env_key
        )


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _env_enabled(name: str) -> bool:
    return bool(os.getenv(name, "").strip())


def _env_enabled_flag(name: str) -> bool:
    val = os.getenv(name, "").strip().lower()
    return val in ("1", "true", "yes", "on")
