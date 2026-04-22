"""Hermes Taskboard Bridge — runtime integration package.

Importing this package has side effects: it monkey-patches a handful of
Hermes internals so a new ``Platform.TASKBOARD`` entry becomes a fully
functional platform adapter. None of this touches Hermes's source files
on disk; patches live entirely in the running Python interpreter and go
away when the process exits.

Normal usage is via the ``hermes-taskboard-bridge`` CLI entry point,
which imports this package before handing off to ``start_gateway()``.
"""

from __future__ import annotations

from .runtime import apply_patches

__version__ = "0.1.1"
__all__ = ["apply_patches", "__version__"]
