#!/usr/bin/env python3
"""Recoverably replace generated installer dependency archives on one filesystem."""

from __future__ import annotations

import argparse
import os
import shutil
import signal
from pathlib import Path

TRANSACTION_PREFIX = ".installer-dependencies."
BACKUP_NAME = ".charts.previous"
FAILURE_ENV = "KUBERPLOY_TEST_INSTALLER_REPLACE_FAILURE"


class ReplacementInterrupted(RuntimeError):
    pass


def interrupted(signum: int, _frame: object) -> None:
    raise ReplacementInterrupted(f"installer dependency replacement interrupted by signal {signum}")


def checked_directory(path: Path, description: str) -> None:
    if path.is_symlink() or not path.is_dir():
        raise SystemExit(f"{description} must be one real directory")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--installer-root", required=True, type=Path)
    parser.add_argument("--staged-charts", required=True, type=Path)
    args = parser.parse_args()

    installer_argument = args.installer_root.absolute()
    if installer_argument.is_symlink():
        raise SystemExit("installer root must not be a symlink")
    installer = installer_argument.resolve(strict=True)
    checked_directory(installer, "installer root")
    staged_argument = args.staged_charts.absolute()
    if staged_argument.is_symlink() or staged_argument.parent.is_symlink():
        raise SystemExit("staged installer dependencies must not be a symlink")
    staged = staged_argument.resolve(strict=True)
    checked_directory(staged, "staged installer dependencies")
    transaction = staged.parent
    if transaction.parent.resolve(strict=True) != installer or not transaction.name.startswith(TRANSACTION_PREFIX):
        raise SystemExit("staged installer dependencies must be inside one installer-local transaction")
    if staged.name != "new":
        raise SystemExit("staged installer dependency directory must use the exact new identity")
    if os.stat(installer).st_dev != os.stat(staged).st_dev:
        raise SystemExit("installer dependency replacement must remain on one filesystem")

    target = installer / "charts"
    backup = installer / BACKUP_NAME
    if target.is_symlink() or backup.is_symlink():
        raise SystemExit("installer dependency target and backup must not be symlinks")

    # Recover the only two durable crash states from a previous invocation.
    if backup.exists():
        checked_directory(backup, "installer dependency backup")
        if target.exists():
            checked_directory(target, "installer dependency target")
            shutil.rmtree(backup)
        else:
            os.replace(backup, target)

    injection = os.environ.get(FAILURE_ENV, "")
    if injection not in {"", "after-backup", "signal-after-backup"}:
        raise SystemExit("unsupported installer dependency replacement failure injection")

    for signum in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
        signal.signal(signum, interrupted)
    try:
        if target.exists():
            checked_directory(target, "installer dependency target")
            os.replace(target, backup)
        if injection == "after-backup":
            raise RuntimeError("injected installer dependency replacement failure")
        if injection == "signal-after-backup":
            os.kill(os.getpid(), signal.SIGTERM)
        os.replace(staged, target)
    except BaseException:
        # Rollback authority comes only from durable rename state. A signal can
        # arrive after target->backup and before any Python assignment.
        if backup.exists() and not target.exists():
            os.replace(backup, target)
        raise

    if backup.exists():
        shutil.rmtree(backup)


if __name__ == "__main__":
    main()
