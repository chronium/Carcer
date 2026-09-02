"""Private run-local evidence for one fresh generation's planning phase."""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any


_SCHEMA_VERSION = 2


class PlanningEvidenceError(RuntimeError):
    """Generation planning evidence could not be recorded safely."""


@dataclass(frozen=True, slots=True)
class PlanningResponseIdentity:
    sha256: str
    size: int


class PlanningEvidenceStore:
    """Allocate exactly one immutable planning record for a generation."""

    def __init__(self, run_directory: str | Path) -> None:
        self._root = Path(run_directory) / "planning-evidence"

    def begin(self, generation: int, thread_id: str) -> "PlanningEvidence":
        if type(generation) is not int or generation < 0:
            raise ValueError("planning generation must be a non-negative integer")
        if not isinstance(thread_id, str) or not thread_id:
            raise ValueError("planning thread ID must not be empty")
        directory = self._root / f"generation-{generation:04d}"
        try:
            self._root.mkdir(parents=True, exist_ok=True)
            if self._root.is_symlink() or not self._root.is_dir():
                raise PlanningEvidenceError("planning evidence root is unsafe")
            directory.mkdir()
        except FileExistsError as error:
            raise PlanningEvidenceError(
                f"planning evidence already exists for generation {generation}"
            ) from error
        except OSError as error:
            raise PlanningEvidenceError(
                f"cannot allocate planning evidence: {error}"
            ) from error
        manifest: dict[str, Any] = {
            "schema_version": _SCHEMA_VERSION,
            "kind": "generation_plan",
            "generation": generation,
            "thread_id": thread_id,
            "turn_id": None,
            "stage": "allocated",
            "outcome": "incomplete",
            "attempts": [],
        }
        _atomic_json(directory / "manifest.json", manifest)
        return PlanningEvidence(directory, manifest)


class PlanningEvidence:
    def __init__(self, directory: Path, manifest: dict[str, Any]) -> None:
        self._directory = directory
        self._manifest = manifest

    @property
    def generation(self) -> int:
        return int(self._manifest["generation"])

    def record_started(self, turn_id: str) -> None:
        if not isinstance(turn_id, str) or not turn_id:
            raise ValueError("planning turn ID must not be empty")
        if self._manifest["stage"] not in {"allocated", "awaiting_resume"}:
            raise PlanningEvidenceError("planning evidence cannot start another attempt")
        attempts = self._attempts()
        attempts.append(
            {
                "attempt": len(attempts) + 1,
                "turn_id": turn_id,
                "outcome": "active",
            }
        )
        self._manifest["turn_id"] = turn_id
        self._manifest["stage"] = "started"
        _atomic_json(self._directory / "manifest.json", self._manifest)

    def complete(
        self,
        outcome: str,
        response: str | None,
    ) -> PlanningResponseIdentity:
        if outcome not in {"completed", "interrupted"}:
            raise ValueError("planning completion outcome is invalid")
        attempt = self._active_attempt()
        exact_response = "" if response is None else response
        encoded = exact_response.encode("utf-8")
        identity = PlanningResponseIdentity(
            hashlib.sha256(encoded).hexdigest(),
            len(encoded),
        )
        response_file = (
            "response.txt"
            if outcome == "completed"
            else f"attempt-{attempt['attempt']:04d}-response.txt"
        )
        _atomic_bytes(self._directory / response_file, encoded)
        attempt.update(
            {
                "outcome": outcome,
                "response_file": response_file,
                "response_present": response is not None,
                "response_bytes": identity.size,
                "response_sha256": identity.sha256,
            }
        )
        if outcome == "completed":
            self._manifest.update(
                {
                    "stage": "completed",
                    "outcome": "completed",
                    "response_file": response_file,
                    "response_present": response is not None,
                    "response_bytes": identity.size,
                    "response_sha256": identity.sha256,
                }
            )
        else:
            self._manifest["stage"] = "awaiting_resume"
            self._manifest["outcome"] = "incomplete"
        _atomic_json(self._directory / "manifest.json", self._manifest)
        return identity

    def fail(self) -> None:
        if self._manifest["outcome"] in {"completed", "failed"}:
            return
        attempts = self._attempts()
        if self._manifest["stage"] == "started":
            self._active_attempt()["outcome"] = "failed"
        else:
            attempts.append(
                {
                    "attempt": len(attempts) + 1,
                    "turn_id": None,
                    "outcome": "failed",
                }
            )
        self._manifest["stage"] = "completed"
        self._manifest["outcome"] = "failed"
        _atomic_json(self._directory / "manifest.json", self._manifest)

    def record_retryable_failure(self) -> None:
        """Record one failed attempt without claiming the final plan failed."""
        attempt = self._active_attempt()
        attempt["outcome"] = "failed"
        self._manifest["stage"] = "awaiting_resume"
        self._manifest["outcome"] = "incomplete"
        _atomic_json(self._directory / "manifest.json", self._manifest)

    def _attempts(self) -> list[dict[str, Any]]:
        attempts = self._manifest.get("attempts")
        if not isinstance(attempts, list):
            raise PlanningEvidenceError("planning evidence attempts are invalid")
        return attempts

    def _active_attempt(self) -> dict[str, Any]:
        attempts = self._attempts()
        if self._manifest["stage"] != "started" or not attempts:
            raise PlanningEvidenceError("planning evidence is not active")
        attempt = attempts[-1]
        if not isinstance(attempt, dict) or attempt.get("outcome") != "active":
            raise PlanningEvidenceError("planning evidence has no active attempt")
        return attempt


def _atomic_json(path: Path, value: object) -> None:
    _atomic_bytes(
        path,
        (json.dumps(value, indent=2, sort_keys=True) + "\n").encode("utf-8"),
    )


def _atomic_bytes(path: Path, value: bytes) -> None:
    temporary: Path | None = None
    try:
        descriptor, name = tempfile.mkstemp(prefix=f".{path.name}-", dir=path.parent)
        temporary = Path(name)
        with os.fdopen(descriptor, "wb") as output:
            output.write(value)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
        temporary = None
    except OSError as error:
        raise PlanningEvidenceError(
            f"cannot write planning evidence {path.name}: {error}"
        ) from error
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)
