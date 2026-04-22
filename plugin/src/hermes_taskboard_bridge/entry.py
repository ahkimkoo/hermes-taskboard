"""CLI entry: apply runtime patches, then hand off to Hermes's gateway.

Drop-in replacement for ``hermes gateway run``. Use in pm2 / systemd:

    hermes-taskboard-bridge run

Any arguments after ``run`` are forwarded to ``start_gateway(...)`` if
upstream supports them; today start_gateway only honours ``--replace``
so we surface that as a CLI flag.
"""

from __future__ import annotations

import argparse
import asyncio
import logging
import os
import sys

# Apply patches AS EARLY AS POSSIBLE. Importing this module should have
# already triggered `hermes_taskboard_bridge.__init__` which defines
# `apply_patches`, but we call it explicitly at startup before touching
# Hermes internals.
from . import apply_patches

logger = logging.getLogger("hermes_taskboard_bridge.entry")


def _setup_logging(verbosity: int) -> None:
    level = logging.WARNING
    if verbosity == 1:
        level = logging.INFO
    elif verbosity >= 2:
        level = logging.DEBUG
    logging.basicConfig(
        level=level,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )


def main(argv: list[str] | None = None) -> int:
    argv = argv if argv is not None else sys.argv[1:]
    parser = argparse.ArgumentParser(
        prog="hermes-taskboard-bridge",
        description=(
            "Drop-in replacement for `hermes gateway run` that adds the "
            "Taskboard WebSocket bridge platform at startup. Zero changes "
            "to Hermes source files."
        ),
    )
    sub = parser.add_subparsers(dest="command", required=True)

    run = sub.add_parser("run", help="Start the Hermes gateway with the Taskboard bridge patched in.")
    run.add_argument(
        "--replace",
        action="store_true",
        help="Kill any running gateway before starting (propagated to Hermes).",
    )
    run.add_argument(
        "-v",
        dest="verbosity",
        action="count",
        default=0,
        help="Increase verbosity (repeatable).",
    )

    sub.add_parser(
        "doctor",
        help="Dry-run: verify Hermes is importable and our patches apply cleanly.",
    )

    args = parser.parse_args(argv)
    _setup_logging(getattr(args, "verbosity", 0))

    try:
        apply_patches()
    except Exception as e:  # noqa: BLE001
        sys.stderr.write(
            f"\n❌ hermes-taskboard-bridge failed to patch Hermes: {e}\n"
            f"   Check that `hermes-agent` is installed in the same venv, "
            f"and that its version is compatible.\n\n"
        )
        return 2

    if args.command == "doctor":
        print("✓ Hermes imports OK")
        print("✓ Platform enum extended")
        print("✓ GatewayRunner wrappers installed")
        print(f"  TASKBOARD_WS_URL = {os.getenv('TASKBOARD_WS_URL', '(unset)')}")
        print(f"  TASKBOARD_PLUGIN_TOKEN = {'(set)' if os.getenv('TASKBOARD_PLUGIN_TOKEN') else '(unset)'}")
        return 0

    if args.command == "run":
        from gateway.run import start_gateway  # noqa: WPS433

        ok = asyncio.run(start_gateway(replace=args.replace, verbosity=args.verbosity))
        return 0 if ok else 1

    parser.error(f"unknown command: {args.command}")
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
