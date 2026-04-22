"""CLI entry: apply runtime patches, then hand off to Hermes's gateway.

Drop-in replacement for ``hermes gateway run``. Use in pm2 / systemd:

    hermes-taskboard-bridge run

When Hermes is installed as a background service (via ``hermes gateway
install``), the generated systemd unit or launchd plist pins
``ExecStart`` to a direct ``python -m hermes_cli.main gateway run`` —
bypassing our wrapper. Patch that with:

    hermes-taskboard-bridge install-service

which rewrites only the ExecStart line in the existing unit file,
backing the original up first. ``uninstall-service`` restores it.

Any arguments after ``run`` are forwarded to ``start_gateway(...)`` if
upstream supports them; today start_gateway only honours ``--replace``
so we surface that as a CLI flag.
"""

from __future__ import annotations

import argparse
import asyncio
import logging
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

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

    inst = sub.add_parser(
        "install-service",
        help="Patch the Hermes systemd/launchd unit file to launch via this "
             "bridge instead of plain `hermes gateway run`.",
    )
    inst.add_argument(
        "--system",
        action="store_true",
        help="Target the system-wide unit (/etc/systemd/system/...) instead "
             "of the per-user unit (~/.config/systemd/user/...).",
    )
    inst.add_argument(
        "--dry-run",
        action="store_true",
        help="Print what would change without writing to disk.",
    )

    uninst = sub.add_parser(
        "uninstall-service",
        help="Restore the Hermes unit file's original ExecStart from the "
             ".bak-bridge backup created by install-service.",
    )
    uninst.add_argument("--system", action="store_true")

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

    if args.command == "install-service":
        return install_service(system=args.system, dry_run=args.dry_run)

    if args.command == "uninstall-service":
        return uninstall_service(system=args.system)

    parser.error(f"unknown command: {args.command}")
    return 2


# ---------------------------------------------------------------------------
# Service integration
# ---------------------------------------------------------------------------

_LINUX_USER_UNIT = Path.home() / ".config" / "systemd" / "user" / "hermes-agent.service"
_LINUX_SYSTEM_UNIT = Path("/etc/systemd/system/hermes-agent.service")
_MACOS_PLIST = Path.home() / "Library" / "LaunchAgents" / "com.nousresearch.hermes-agent.plist"


def _unit_path(system: bool) -> Path | None:
    """Return the service file to patch, or None if none exists yet."""
    if sys.platform == "darwin":
        return _MACOS_PLIST if _MACOS_PLIST.exists() else None
    target = _LINUX_SYSTEM_UNIT if system else _LINUX_USER_UNIT
    return target if target.exists() else None


def _our_bin() -> str:
    """Absolute path to our own CLI — what ExecStart should point at."""
    # sys.argv[0] is unreliable (can be a relative path); use the venv-
    # installed script at the same place this Python lives.
    exe = Path(sys.executable).with_name("hermes-taskboard-bridge")
    if exe.exists():
        return str(exe)
    # Fallback: shutil.which scans PATH.
    found = shutil.which("hermes-taskboard-bridge")
    return found or "hermes-taskboard-bridge"


def install_service(system: bool = False, dry_run: bool = False) -> int:
    """Rewrite the unit's ExecStart (or plist's Program args) to launch
    via the bridge wrapper."""
    path = _unit_path(system)
    if path is None:
        where = "system" if system else "user"
        sys.stderr.write(
            f"❌ No Hermes service found. Install it first:\n"
            f"   hermes gateway install{'' if not system else ' --system'}\n"
            f"Looking for ({where}):\n"
            f"   {_LINUX_SYSTEM_UNIT if system else _LINUX_USER_UNIT}\n"
            f"   {_MACOS_PLIST} (macOS)\n"
        )
        return 2

    if path.suffix == ".plist":
        return _install_launchd(path, dry_run=dry_run)
    return _install_systemd(path, system=system, dry_run=dry_run)


def _install_systemd(unit: Path, system: bool, dry_run: bool) -> int:
    text = unit.read_text(encoding="utf-8")
    bridge_bin = _our_bin()

    if f"ExecStart={bridge_bin}" in text:
        print(f"✓ {unit} already patched.")
        return 0

    # Match any ExecStart= line. Hermes's template has a single one;
    # re.MULTILINE + non-greedy end-of-line handles both LF and CRLF.
    pattern = re.compile(r"^ExecStart=.*$", re.MULTILINE)
    if not pattern.search(text):
        sys.stderr.write(f"❌ No ExecStart= line in {unit}; refusing to touch.\n")
        return 3

    # Preserve `--replace` flag if the original passed it — Hermes uses
    # it by default and systemd expects it to avoid stuck-PID startup.
    orig = pattern.search(text).group(0)
    replace_flag = " --replace" if "--replace" in orig else ""
    new_line = f"ExecStart={bridge_bin} run{replace_flag}"
    new_text = pattern.sub(new_line, text, count=1)

    if dry_run:
        print(f"would rewrite {unit}:")
        print(f"  -  {orig}")
        print(f"  +  {new_line}")
        return 0

    backup = unit.with_suffix(unit.suffix + ".bak-bridge")
    if not backup.exists():
        shutil.copy2(unit, backup)
        print(f"  → backup: {backup}")
    unit.write_text(new_text, encoding="utf-8")
    print(f"  → patched {unit}")
    print(f"  -  {orig}")
    print(f"  +  {new_line}")

    scope = "--system" if system else "--user"
    try:
        subprocess.run(["systemctl", scope, "daemon-reload"], check=False)
    except FileNotFoundError:
        print("  (systemctl not found — reload skipped, you may need to do it manually)")
    print(
        f"\nReload done. To apply:\n"
        f"   hermes gateway restart{'' if not system else ''}\n"
        f"or equivalently:\n"
        f"   systemctl {scope} restart hermes-agent.service\n"
    )
    return 0


def _install_launchd(plist: Path, dry_run: bool) -> int:
    text = plist.read_text(encoding="utf-8")
    bridge_bin = _our_bin()
    if bridge_bin in text:
        print(f"✓ {plist} already patched.")
        return 0

    # launchd plists use <array><string>…</string></array> under
    # ProgramArguments. Simplest sound patch: replace the first
    # <string>…hermes_cli.main…</string> with two strings (bridge_bin + "run")
    # and strip the "-m hermes_cli.main gateway run" trio. To keep the
    # scope small, we only do this when the template matches exactly.
    sys.stderr.write(
        "⚠  Automated launchd patching not implemented yet — the plist\n"
        "   schema varies too much. Manual edit: change the ProgramArguments\n"
        "   array so the first string is the full path to\n"
        f"   {bridge_bin}\n"
        "   and the next string is 'run'. Drop the python -m hermes_cli.main\n"
        "   arguments. Then:\n"
        "      launchctl unload {plist}\n"
        "      launchctl load   {plist}\n"
    )
    return 4


def uninstall_service(system: bool = False) -> int:
    path = _unit_path(system)
    if path is None:
        sys.stderr.write("❌ No Hermes service file found; nothing to restore.\n")
        return 2
    backup = path.with_suffix(path.suffix + ".bak-bridge")
    if not backup.exists():
        sys.stderr.write(f"❌ No backup at {backup}; can't restore.\n")
        return 3
    shutil.copy2(backup, path)
    print(f"  → restored {path} from {backup}")
    if path.suffix != ".plist":
        scope = "--system" if system else "--user"
        subprocess.run(["systemctl", scope, "daemon-reload"], check=False)
        print(f"  → systemctl {scope} daemon-reload")
    print("\nRestart Hermes to pick up the original command.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
