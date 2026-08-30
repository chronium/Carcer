"""Trusted operator commands and the line-oriented console frontend."""

from __future__ import annotations

import argparse
import os
import sys
import threading
import time
from collections.abc import Callable, Sequence
from pathlib import Path
from typing import TextIO

from .codex_activity import CodexActivityStream
from .codex_generation_worker import (
    CONTINUE_PROMPT,
    DEFAULT_INTERRUPT_TIMEOUT_SECONDS,
    RESUME_PROMPT,
    CodexGenerationResult,
    CodexGenerationSession,
    CodexGenerationSessionMode,
    CodexGenerationWorkerError,
)
from .generation_git import GenerationGitRecorder, GenerationGitRecorderError
from .generation_runtime import ArchivedGeneration, CodexOSRun, RuntimeState
from .exit_interview_transcript import (
    ExitInterviewArtifactStore,
    ExitInterviewTranscriptError,
)
from .observability import (
    ExperimentObservability,
    ExperimentObservabilityError,
)


class OperatorConsole:
    def __init__(
        self,
        runtime: CodexOSRun,
        input_stream: TextIO | None = None,
        output_stream: TextIO | None = None,
        *,
        codex_executable: str = "codex",
        codex_auth_file: str | Path | None = None,
        objective: str | None = None,
        reviewer_codex_executable: str = "codex",
        reviewer_auth_file: str | Path | None = None,
        git_recorder: GenerationGitRecorder | None = None,
        interview_repository: str | Path | None = None,
        interrupt_timeout_seconds: float = DEFAULT_INTERRUPT_TIMEOUT_SECONDS,
        output_handler: Callable[[str], None] | None = None,
        confirmation_handler: Callable[[str], bool] | None = None,
    ) -> None:
        self._runtime = runtime
        self._input = input_stream if input_stream is not None else sys.stdin
        self._output = output_stream if output_stream is not None else sys.stdout
        self._codex_executable = codex_executable
        self._codex_auth_file = codex_auth_file
        self._objective = objective
        self._reviewer_codex_executable = reviewer_codex_executable
        self._reviewer_auth_file = reviewer_auth_file
        self._git_recorder = git_recorder
        self._interview_store = (
            None
            if interview_repository is None
            else ExitInterviewArtifactStore(
                interview_repository,
                runtime.run_directory,
            )
        )
        self._output_handler = output_handler
        self._confirmation_handler = confirmation_handler
        observed = getattr(runtime, "observability", None)
        self._observability = (
            observed if isinstance(observed, ExperimentObservability) else None
        )
        self._interrupt_timeout_seconds = interrupt_timeout_seconds
        self._session: CodexGenerationSession | None = None
        self._turn_thread: threading.Thread | None = None
        self._session_generation: int | None = None
        self._agent_unavailable_generation: int | None = None
        self._interview_open = False
        self._persisted_interview_session: CodexGenerationSession | None = None
        self._resume_agent_after_pause = False
        self._agent_lock = threading.RLock()
        self._output_lock = threading.Lock()
        self._shutdown_lock = threading.Lock()
        self._shutdown = False

    def run(self) -> None:
        self.show_startup()

        try:
            while True:
                try:
                    if self.execute_line(self._readline(self.input_prompt)):
                        return
                except EOFError:
                    self._print("Input closed; stopping CodexOS run.")
                    return
                except KeyboardInterrupt:
                    self._print()
                    self._print("Interrupted; stopping CodexOS run.")
                    return
        finally:
            self.shutdown()

    def show_startup(self) -> None:
        """Print startup state through the configured frontend."""
        self._print("CodexOS operator console")
        self._print()
        self._print(f"Run directory: {self._runtime.run_directory}")
        self._reconcile_git()
        if self._runtime.state is RuntimeState.AWAITING_NEXT_GENERATION:
            self._print_gate()
        else:
            self._print_running_summary()
        self._print()
        self._print("Type 'help' for commands.")

    def execute_line(self, line: str) -> bool:
        """Execute one command using the console's authoritative semantics."""
        command_line = line.rstrip("\r\n")
        words = command_line.strip().split()
        if not words:
            return False
        try:
            with self._agent_lock:
                interview_open = self._interview_open
            if interview_open:
                if words == ["end"] or words == ["end-interview"]:
                    self._end_exit_interview()
                elif words == ["quit"]:
                    return self._quit()
                else:
                    question = _question_from_command(command_line, words)
                    self._ask_exit_interview(question or command_line)
                return False
            return self._execute(words, command_line)
        except (OSError, RuntimeError, ValueError) as error:
            self._print(f"Error: {error}")
            return False

    def shutdown(self) -> None:
        """Idempotently stop the active session and runtime."""
        with self._shutdown_lock:
            if self._shutdown:
                return
            self._shutdown = True
            try:
                self._terminate_agent_session(interrupt=True)
            finally:
                self._runtime.stop()

    @property
    def codex_turn_state(self) -> str:
        with self._agent_lock:
            if self._turn_thread is not None:
                return "working"
            if self._session is not None:
                return "idle"
            return "stopped"

    @property
    def input_prompt(self) -> str:
        with self._agent_lock:
            return "interview> " if self._interview_open else "codexos> "

    @property
    def exit_interview_state(self) -> str:
        with self._agent_lock:
            session = self._session
            turn_active = self._turn_thread is not None
            interview_open = self._interview_open
            generation = self._session_generation
        if session is None or generation != self._runtime.generation_number:
            return "unavailable"
        if interview_open and turn_active:
            return "answering"
        if (
            session.mode is CodexGenerationSessionMode.RETAINED_AT_GATE
            and session.healthy
        ):
            return "idle" if interview_open else "available"
        return "unavailable"

    def _execute(self, words: list[str], command_line: str | None = None) -> bool:
        command = words[0]
        if command == "help":
            if len(words) != 1:
                self._print("Usage: help")
            else:
                self._print_help()
        elif command == "status":
            if len(words) != 1:
                self._print("Usage: status")
            else:
                self._print_status()
        elif command == "history":
            if len(words) != 1:
                self._print("Usage: history")
            else:
                self._print_history()
        elif command == "inspect":
            generation = self._generation_argument(words, "inspect")
            if generation is not None:
                self._print_inspection(
                    self._runtime.inspect_generation(generation)
                )
        elif command == "features":
            if len(words) != 1:
                self._print("Usage: features")
            else:
                self._print_features()
        elif command == "feature":
            request_id = self._generation_argument(words, "feature")
            if request_id is not None:
                self._print_feature(request_id)
        elif command == "feature-approve":
            request_id = self._generation_argument(words, "feature-approve")
            if request_id is not None:
                self._approve_feature(request_id)
        elif command == "feature-deny":
            request_id = self._generation_argument(words, "feature-deny")
            if request_id is not None:
                self._deny_feature(request_id)
        elif command == "agent":
            if len(words) != 1:
                self._print("Usage: agent")
            else:
                try:
                    self._start_agent()
                except Exception:
                    self._record_operator("agent", "failed")
                    raise
                self._record_operator("agent", "success")
        elif command == "interview":
            if len(words) != 1:
                self._print("Usage: interview")
            else:
                self._begin_exit_interview()
        elif command == "ask":
            question = _question_from_command(command_line, words)
            if question is None:
                self._print("Usage: ask <text>")
            else:
                self._ask_exit_interview(question)
        elif command == "end-interview":
            if len(words) != 1:
                self._print("Usage: end-interview")
            else:
                self._end_exit_interview()
        elif command == "git-record":
            if len(words) != 1:
                self._print("Usage: git-record")
            else:
                self._record_git()
        elif command == "pause":
            if len(words) != 1:
                self._print("Usage: pause")
            else:
                self._pause()
        elif command == "resume":
            if len(words) != 1:
                self._print("Usage: resume")
            else:
                self._resume()
        elif command == "abort":
            if len(words) != 1:
                self._print("Usage: abort")
            else:
                self._abort()
        elif command == "continue":
            if len(words) != 1:
                self._print("Usage: continue")
            else:
                self._continue_generation()
        elif command == "rollback":
            generation = self._generation_argument(words, "rollback")
            if generation is not None:
                self._rollback(generation)
        elif command == "quit":
            if len(words) != 1:
                self._print("Usage: quit")
            else:
                return self._quit()
        else:
            self._print("Unknown command. Type 'help'.")
        return False

    def _print_help(self) -> None:
        self._print("help        show these commands")
        self._print("status      show current runtime state")
        self._print("history     show archived generation lineage")
        self._print("inspect N   show archived generation N")
        self._print("features    list external feature requests")
        self._print("feature N   show external feature request N")
        self._print("feature-approve N  approve a pending request at the gate")
        self._print("feature-deny N     deny a pending request at the gate")
        self._print("agent       start or continue the generation's Codex session")
        self._print("interview   enter a retained post-generation exit interview")
        self._print("ask TEXT    ask one retrospective exit-interview question")
        self._print("end-interview  end the interview and close retained Sol")
        self._print("git-record  reconcile local generation Git provenance")
        self._print("pause       pause the running generation")
        self._print("resume      resume the paused generation")
        self._print("abort       permanently abort the running/paused generation")
        self._print("continue    start the cooperatively selected successor")
        self._print("rollback N  fork from completed generation N")
        self._print("quit        end the run")

    def _print_status(self) -> None:
        generation = self._runtime.generation_number
        pid = self._runtime.active_pid
        self._print(
            f"Generation: {generation if generation is not None else 'none'}"
        )
        self._print(f"State: {self._runtime.state.name}")
        self._print(f"QEMU PID: {pid if pid is not None else 'none'}")
        if self._runtime.state in {RuntimeState.RUNNING, RuntimeState.PAUSED}:
            self._print(
                f"Hardware profile: {self._runtime.hardware_profile.profile}"
            )
        self._print(
            "Selected successor: "
            + ("yes" if self._runtime.pending_generation_finish else "no")
        )
        pending_requests = sum(
            request.status == "pending"
            for request in self._runtime.feature_requests()
        )
        self._print(f"Pending feature requests: {pending_requests}")
        if self._observability is not None:
            if self._observability.healthy:
                self._print("Observability: healthy")
            else:
                self._print(
                    "Observability: degraded - "
                    + str(self._observability.degraded_reason)
                )
        with self._agent_lock:
            session = self._session
            running = self._turn_thread is not None
        session_state = "active" if session is not None else "none"
        self._print(f"Codex session: {session_state}")
        if running:
            turn_state = "running"
        elif session is not None:
            turn_state = "idle"
        else:
            turn_state = "none"
        self._print(f"Codex turn: {turn_state}")
        self._print(f"Exit interview: {self.exit_interview_state}")
        handoff = self._runtime.previous_handoff
        if handoff is None:
            self._print("Previous handoff: none")
        else:
            label = (
                "Handoff"
                if self._runtime.state
                is RuntimeState.AWAITING_NEXT_GENERATION
                else "Previous handoff"
            )
            self._print(f"{label}:")
            self._print_indented(handoff)

    def _print_history(self) -> None:
        generations = self._runtime.archived_generations()
        if not generations:
            self._print("No archived generations.")
            return
        self._print("GEN   PARENT   TRANSITION   OUTCOME")
        for item in generations:
            parent = "-" if item.parent_generation is None else str(
                item.parent_generation
            )
            self._print(
                f"{item.generation:<5} {parent:<8} "
                f"{item.transition:<12} {item.outcome}"
            )

    def _print_inspection(self, item: ArchivedGeneration) -> None:
        parent = "-" if item.parent_generation is None else str(
            item.parent_generation
        )
        self._print(f"Generation: {item.generation}")
        self._print(f"Parent: {parent}")
        self._print(f"Transition: {item.transition}")
        self._print(f"Outcome: {item.outcome}")
        self._print(f"Archive: {item.archive_path}")
        hardware = item.hardware
        self._print("Hardware:")
        self._print(f"  Profile: {hardware.profile}")
        self._print(f"  Machine: {hardware.machine}")
        self._print(f"  CPU: {hardware.cpu_model} x {hardware.vcpus}")
        self._print(f"  RAM: {hardware.memory_mib} MiB")
        self._print(f"  Graphics: {hardware.graphics}")
        self._print(f"  Network: {hardware.network}")
        writable = ", ".join(hardware.writable_block_devices) or "none"
        self._print(f"  Writable disks: {writable}")
        if item.outcome == "completed":
            self._print("Handoff:")
            self._print_indented(item.handoff or "")
            self._print("Artifacts:")
            self._print("  boot ISO")
            self._print("  hardware manifest")
            self._print("  source snapshot")
            self._print("  materialized source")
            self._print("  successor kernel")
            self._print("  successor ISO")
            self._print("  QEMU stdout")
            self._print("  QEMU stderr")
        else:
            self._print("Generation aborted by operator.")
            self._print("Artifacts:")
            self._print("  boot ISO")
            self._print("  hardware manifest")
            self._print("  QEMU stdout")
            self._print("  QEMU stderr")

    def _print_features(self) -> None:
        requests = self._runtime.feature_requests()
        if not requests:
            self._print("No feature requests.")
            return
        self._print("ID   GEN   STATUS     TITLE")
        for request in requests:
            self._print(
                f"{request.id:<4} {request.generation:<5} "
                f"{request.status:<10} "
                f"{_escape_terminal_text(request.title)}"
            )

    def _print_feature(self, request_id: int) -> None:
        request = self._runtime.feature_request(request_id)
        self._print(f"Feature request: #{request.id}")
        self._print(f"Generation: {request.generation}")
        self._print(f"Status: {request.status}")
        self._print(f"Title: {_escape_terminal_text(request.title)}")
        self._print()
        self._print("Description:")
        self._print_indented(
            _escape_terminal_text(request.description, preserve_newlines=True)
        )

    def _approve_feature(self, request_id: int) -> None:
        if not self._confirm(
            f"Mark feature request #{request_id} approved?\n"
            "Only do this after the trusted external capability has been "
            "provisioned. [y/N] "
        ):
            self._print("Feature approval cancelled.")
            return
        self._runtime.approve_feature_request(request_id)
        self._print(f"Feature request #{request_id} approved.")

    def _deny_feature(self, request_id: int) -> None:
        if not self._confirm(f"Deny feature request #{request_id}? [y/N] "):
            self._print("Feature denial cancelled.")
            return
        self._runtime.deny_feature_request(request_id)
        self._print(f"Feature request #{request_id} denied.")

    def _abort(self) -> None:
        generation = self._runtime.generation_number
        if not self._confirm(
            f"Abort generation {generation} permanently? [y/N] "
        ):
            self._print("Abort cancelled.")
            self._record_operator("abort", "cancelled")
            return
        try:
            self._terminate_agent_session(interrupt=True)
            self._runtime.abort_generation()
            self._clear_agent_generation()
            self._reconcile_git()
            self._print_gate()
        except Exception:
            self._record_operator("abort", "failed")
            raise
        self._record_operator("abort", "success")

    def _continue_generation(self) -> None:
        try:
            self._dispose_agent_at_gate()
            if self._runtime.pending_generation_finish is None:
                self._runtime.continue_generation()
            else:
                generation = (self._runtime.generation_number or 0) + 1
                self._print(
                    f"Starting generation {generation} from selected successor..."
                )
                self._runtime.continue_generation()
                self._clear_agent_generation()
                self._print_running_summary()
        except Exception:
            self._record_operator("continue", "failed")
            raise
        self._record_operator("continue", "success")

    def _rollback(self, parent: int) -> None:
        self._require_no_active_agent_turn_at_gate()
        generation = (self._runtime.generation_number or 0) + 1
        if not self._confirm(
            f"Fork generation {generation} from generation {parent}'s "
            "selected successor?\n"
            "This preserves all later archives unchanged. [y/N] "
        ):
            self._print("Rollback cancelled.")
            self._record_operator(
                "rollback", "cancelled", {"parent_generation": parent}
            )
            return
        try:
            self._dispose_agent_at_gate()
            self._runtime.fork_from_generation(parent)
            self._clear_agent_generation()
            self._print(
                f"Generation {self._runtime.generation_number} started from "
                f"generation {parent}."
            )
            self._print(f"State: {self._runtime.state.name}")
            self._print(f"QEMU PID: {self._runtime.active_pid}")
        except Exception:
            self._record_operator(
                "rollback", "failed", {"parent_generation": parent}
            )
            raise
        self._record_operator(
            "rollback", "success", {"parent_generation": parent}
        )

    def _quit(self) -> bool:
        if self._runtime.state in {RuntimeState.RUNNING, RuntimeState.PAUSED}:
            generation = self._runtime.generation_number
            if not self._confirm(
                f"Stop the run without archiving generation {generation}? "
                "[y/N] "
            ):
                self._print("Quit cancelled.")
                self._record_operator("quit", "cancelled")
                return False
        try:
            self._terminate_agent_session(interrupt=True)
            self._runtime.stop()
        except Exception:
            self._record_operator("quit", "failed")
            raise
        self._record_operator("quit", "success")
        return True

    def _start_agent(self, prompt: str | None = None) -> None:
        if self._runtime.state is not RuntimeState.RUNNING:
            raise RuntimeError("CodexOS generation is not running")
        generation = self._runtime.generation_number
        with self._agent_lock:
            if self._turn_thread is not None:
                raise RuntimeError("Codex implementor turn is already active")
            if self._agent_unavailable_generation == generation:
                raise RuntimeError(
                    "Codex session failed and cannot be replaced in this generation"
                )
            session = self._session
            if session is None:
                session = CodexGenerationSession(
                    self._runtime,
                    self._codex_executable,
                    self._codex_auth_file,
                    objective=self._objective,
                    reviewer_codex_executable=self._reviewer_codex_executable,
                    reviewer_auth_file=self._reviewer_auth_file,
                )
                self._session = session
                self._session_generation = generation
                initial = True
            else:
                if self._session_generation != generation:
                    raise RuntimeError("Codex session belongs to another generation")
                if not session.healthy:
                    raise RuntimeError("Codex generation session is unusable")
                initial = False
            turn = threading.Thread(
                target=self._run_agent_turn,
                args=(session, initial, prompt),
                name="codexos-implementor-turn",
                daemon=True,
            )
            self._turn_thread = turn
            turn.start()
        self._print(f"Codex turn started for generation {generation}.")

    def _run_agent_turn(
        self,
        session: CodexGenerationSession,
        initial: bool,
        prompt: str | None,
    ) -> None:
        result: CodexGenerationResult | None = None
        error: Exception | None = None
        try:
            if initial:
                result = session.run_initial_turn()
            else:
                result = session.run_continuation_turn(prompt or CONTINUE_PROMPT)
        except (OSError, RuntimeError, CodexGenerationWorkerError) as caught:
            error = caught
        finally:
            with self._agent_lock:
                self._turn_thread = None

        if error is not None:
            if (
                session.healthy
                and self._runtime.state is not RuntimeState.AWAITING_NEXT_GENERATION
            ):
                self._print(f"Codex turn failed: {error}")
            else:
                generation = self._runtime.generation_number
                session.close()
                with self._agent_lock:
                    if self._session is session:
                        self._session = None
                        self._session_generation = None
                        self._agent_unavailable_generation = generation
                self._print(f"Codex session failed: {error}")
            return

        if result is None:
            return
        if self._runtime.state is RuntimeState.AWAITING_NEXT_GENERATION:
            if result.turn_status == "completed":
                try:
                    session.retain_for_exit_interview()
                except (RuntimeError, CodexGenerationWorkerError):
                    session.close()
                    with self._agent_lock:
                        if self._session is session:
                            self._session = None
                            self._session_generation = None
            else:
                session.close()
                with self._agent_lock:
                    if self._session is session:
                        self._session = None
                        self._session_generation = None
            self._reconcile_git()
            self._print(
                f"Generation {self._runtime.generation_number} completed cooperatively."
            )
            self._print_gate()
        elif result.turn_status == "interrupted":
            self._print("Codex turn interrupted.")
        else:
            self._print(result.summary)
        if result.final_message:
            self._print("Codex:")
            self._print_indented(result.final_message)

    def _begin_exit_interview(self) -> None:
        if (
            self._runtime.state is not RuntimeState.AWAITING_NEXT_GENERATION
            or self._runtime.pending_generation_finish is None
        ):
            raise RuntimeError(
                "exit interview is available only after cooperative completion"
            )
        with self._agent_lock:
            session = self._session
            generation = self._session_generation
            turn = self._turn_thread
            if self._interview_open:
                raise RuntimeError("exit interview is already active")
        if (
            session is None
            or generation != self._runtime.generation_number
            or not session.exit_interview_available
        ):
            self._print(
                "No live generation session is available for an exit interview."
            )
            self._print(
                "Only generations completed after exit-interview support was active "
                "can retain their original Sol thread."
            )
            return
        if self._interview_store is None:
            raise RuntimeError(
                "exit interview persistence requires --git-repository"
            )
        if turn is not None:
            raise RuntimeError("Codex turn is already active")
        session.begin_exit_interview()
        with self._agent_lock:
            if self._session is not session:
                session.close()
                raise RuntimeError("retained Codex session changed unexpectedly")
            self._interview_open = True
        self._print(
            f"Exit interview started for generation {self._runtime.generation_number}."
        )
        self._print(
            "The generation, selected successor, and handoff are frozen. "
            "Interview turns are read-only."
        )

    def _ask_exit_interview(self, question: str) -> None:
        with self._agent_lock:
            session = self._session
            if not self._interview_open:
                raise RuntimeError("exit interview is not active")
            if self._turn_thread is not None:
                raise RuntimeError("exit interview turn is already active")
            if session is None or not session.exit_interview_available:
                raise RuntimeError("exit interview session is unavailable")
            turn = threading.Thread(
                target=self._run_exit_interview_turn,
                args=(session, question),
                name="codexos-exit-interview-turn",
                daemon=True,
            )
            self._turn_thread = turn
            turn.start()
        self._print("Exit interview question sent.")

    def _run_exit_interview_turn(
        self,
        session: CodexGenerationSession,
        question: str,
    ) -> None:
        result: CodexGenerationResult | None = None
        error: Exception | None = None
        try:
            result = session.run_exit_interview_turn(question)
        except (OSError, RuntimeError, ValueError, CodexGenerationWorkerError) as caught:
            error = caught
        finally:
            with self._agent_lock:
                self._turn_thread = None

        if error is not None:
            with self._agent_lock:
                retained = self._session is session
            if not retained:
                return
            if session.healthy:
                self._print(f"Exit interview turn failed: {error}")
            else:
                generation = self._runtime.generation_number
                self._persist_exit_interview(session, "failed")
                session.close()
                with self._agent_lock:
                    if self._session is session:
                        self._session = None
                        self._session_generation = None
                        self._interview_open = False
                        self._agent_unavailable_generation = generation
                self._print(f"Exit interview session failed: {error}")
            return

        if result is None:
            return
        if result.turn_status == "interrupted":
            self._print("Exit interview turn interrupted.")
        elif self._output_handler is None and result.final_message:
            self._print("Sol:")
            self._print_indented(result.final_message)

    def _end_exit_interview(self) -> None:
        with self._agent_lock:
            if not self._interview_open:
                raise RuntimeError("exit interview is not active")
        self._terminate_agent_session(
            interrupt=True,
            end_interview=True,
        )
        self._print("Exit interview ended.")

    def _pause(self) -> None:
        try:
            deadline = time.monotonic() + self._interrupt_timeout_seconds
            with self._agent_lock:
                session = self._session
                turn = self._turn_thread
            if turn is not None:
                if session is None:
                    raise RuntimeError("Codex turn has no generation session")
                session.interrupt_turn(max(0.0, deadline - time.monotonic()))
                turn.join(max(0.0, deadline - time.monotonic()))
                if turn.is_alive():
                    raise CodexGenerationWorkerError(
                        "Codex turn cleanup did not finish before timeout"
                    )
                self._resume_agent_after_pause = True
            else:
                self._resume_agent_after_pause = False
            self._runtime.pause()
            self._print(f"Generation {self._runtime.generation_number} paused.")
        except Exception:
            self._record_operator("pause", "failed")
            raise
        self._record_operator("pause", "success")

    def _resume(self) -> None:
        try:
            restart_agent = self._resume_agent_after_pause
            self._runtime.resume()
            self._resume_agent_after_pause = False
            generation = self._runtime.generation_number
            if restart_agent:
                self._start_agent(RESUME_PROMPT)
                self._print(
                    f"Generation {generation} resumed; "
                    "Codex continued in the same session."
                )
            else:
                self._print(f"Generation {generation} resumed.")
        except Exception:
            self._record_operator("resume", "failed")
            raise
        self._record_operator("resume", "success")

    def _terminate_agent_session(
        self,
        *,
        interrupt: bool,
        end_interview: bool = False,
    ) -> None:
        with self._agent_lock:
            session = self._session
            turn = self._turn_thread
        if session is None:
            return
        if interrupt and turn is not None:
            try:
                session.interrupt_turn(self._interrupt_timeout_seconds)
            except (OSError, RuntimeError, CodexGenerationWorkerError):
                pass
        if self._interview_open:
            if turn is not None:
                interview_outcome = "interrupted"
            elif end_interview:
                interview_outcome = "completed"
            else:
                interview_outcome = "incomplete"
            self._persist_exit_interview(session, interview_outcome)
        if end_interview:
            try:
                session.end_exit_interview()
            except RuntimeError:
                session.close()
        else:
            session.close()
        if turn is not None and turn is not threading.current_thread():
            turn.join(timeout=self._interrupt_timeout_seconds)
        with self._agent_lock:
            if self._session is session:
                self._session = None
            self._turn_thread = None
            self._session_generation = None
            self._interview_open = False

    def _persist_exit_interview(
        self,
        session: CodexGenerationSession,
        outcome: str,
    ) -> None:
        store = self._interview_store
        if store is None:
            return
        with self._agent_lock:
            if self._persisted_interview_session is session:
                return
            transcript = session.exit_interview_transcript()
            if transcript is None:
                return
            try:
                artifact = store.persist(transcript, outcome)
            except (OSError, ExitInterviewTranscriptError) as error:
                self._print(f"Exit interview transcript error: {error}")
                return
            if artifact is not None:
                self._persisted_interview_session = session
        if artifact is not None:
            self._print("Exit interview saved:")
            self._print(f"  {artifact.relative_path}")

    def _require_no_active_agent_turn_at_gate(self) -> None:
        with self._agent_lock:
            if self._turn_thread is not None:
                raise RuntimeError(
                    "previous generation Codex turn is still active"
                )

    def _dispose_agent_at_gate(self) -> None:
        self._require_no_active_agent_turn_at_gate()
        with self._agent_lock:
            session = self._session
        if session is not None and session.mode not in {
            CodexGenerationSessionMode.RETAINED_AT_GATE,
            CodexGenerationSessionMode.CLOSED,
        }:
            raise RuntimeError(
                "previous generation Codex session is not idle at the gate"
            )
        self._terminate_agent_session(interrupt=False)

    def _clear_agent_generation(self) -> None:
        with self._agent_lock:
            self._session = None
            self._turn_thread = None
            self._session_generation = None
            self._agent_unavailable_generation = None
            self._interview_open = False
            self._persisted_interview_session = None
        self._resume_agent_after_pause = False

    def _record_git(self) -> None:
        if self._git_recorder is None:
            self._print("Git provenance is not configured.")
            return
        if self._reconcile_git():
            self._print("Git provenance is up to date.")

    def _reconcile_git(self) -> bool:
        recorder = self._git_recorder
        if recorder is None:
            return True
        try:
            records = recorder.reconcile()
        except GenerationGitRecorderError as error:
            if self._observability is not None:
                self._observability.record(
                    "git_reconciliation_failed",
                    self._runtime.generation_number,
                    {"error": str(error)[:1024]},
                )
            self._print(f"Git provenance error: {error}")
            return False
        if self._observability is not None:
            for record in records:
                self._observability.record(
                    "git_generation_recorded",
                    record.generation,
                    {
                        "generation": record.generation,
                        "tag": record.tag,
                        "commit": record.commit,
                        "already_recorded": record.already_recorded,
                    },
                )
        return True

    def _record_operator(
        self,
        action: str,
        result: str,
        extra: dict[str, object] | None = None,
    ) -> None:
        if self._observability is not None:
            self._observability.record(
                f"operator_{action}",
                self._runtime.generation_number,
                {"result": result, **(extra or {})},
            )

    def _print_gate(self) -> None:
        generation = self._runtime.generation_number
        pending = self._runtime.pending_generation_finish
        self._print()
        if pending is not None:
            self._print(f"Generation {generation} complete.")
            self._print()
            self._print("Handoff:")
            self._print_indented(pending.handoff_message)
            self._print()
            self._print("A successor is selected.")
            self._print()
            self._print("Use:")
            self._print("  continue")
            if self.exit_interview_state == "available":
                self._print("  interview")
        else:
            self._print(f"Generation {generation} aborted.")
            self._print()
            self._print("No successor was selected.")
            self._print()
            self._print("Use:")
        self._print("  rollback N")
        self._print(f"  inspect {generation}")
        self._print("  history")
        pending_requests = [
            request for request in self._runtime.feature_requests()
            if request.status == "pending"
        ]
        if pending_requests:
            self._print()
            self._print("Pending feature requests:")
            self._print()
            for request in pending_requests:
                self._print(
                    f"#{request.id}  {_escape_terminal_text(request.title)}"
                )
            self._print()
            self._print("Use:")
            self._print("  features")
            self._print("  feature N")
            self._print("  feature-approve N")
            self._print("  feature-deny N")
        self._print("  quit")

    def _print_running_summary(self) -> None:
        generation = self._runtime.generation_number
        self._print(f"Generation {generation}: {self._runtime.state.name}")
        pid = self._runtime.active_pid
        self._print(f"QEMU PID: {pid if pid is not None else 'none'}")

    def _generation_argument(
        self,
        words: list[str],
        command: str,
    ) -> int | None:
        if (
            len(words) != 2
            or not words[1].isascii()
            or not words[1].isdecimal()
        ):
            self._print(f"Usage: {command} N")
            return None
        return int(words[1])

    def _confirm(self, prompt: str) -> bool:
        if self._confirmation_handler is not None:
            return self._confirmation_handler(prompt)
        return self._readline(prompt).strip() in {"y", "Y"}

    def _readline(self, prompt: str) -> str:
        with self._output_lock:
            self._output.write(prompt)
            self._output.flush()
        line = self._input.readline()
        if line == "":
            raise EOFError
        return line

    def _print_indented(self, text: str) -> None:
        lines = text.splitlines() or [""]
        for line in lines:
            self._print(f"  {line}")

    def _print(self, text: str = "") -> None:
        if self._output_handler is not None:
            try:
                self._output_handler(text)
            except Exception:
                # Presentation failure cannot affect trusted operator actions.
                pass
            return
        with self._output_lock:
            print(text, file=self._output)


def _escape_terminal_text(
    text: str,
    *,
    preserve_newlines: bool = False,
) -> str:
    escaped: list[str] = []
    for character in text:
        codepoint = ord(character)
        if character == "\n":
            escaped.append("\n" if preserve_newlines else "\\n")
        elif character == "\r":
            escaped.append("\\r")
        elif character == "\t":
            escaped.append("\\t")
        elif codepoint <= 0x1F or codepoint == 0x7F or 0x80 <= codepoint <= 0x9F:
            escaped.append(f"\\x{codepoint:02x}")
        else:
            escaped.append(character)
    return "".join(escaped)


def _question_from_command(
    command_line: str | None,
    words: list[str],
) -> str | None:
    if command_line is None or not words or words[0] != "ask":
        return None
    stripped = command_line.lstrip()
    if len(stripped) == 3 or not stripped[3].isspace():
        return None
    question = stripped[3:].lstrip()
    return question if question else None


def main(
    argv: Sequence[str] | None = None,
    input_stream: TextIO | None = None,
    output_stream: TextIO | None = None,
) -> int:
    parser = argparse.ArgumentParser(description="CodexOS operator console")
    parser.add_argument("--run-directory", required=True, type=Path)
    opening = parser.add_mutually_exclusive_group(required=True)
    opening.add_argument("--initial-iso", type=Path)
    opening.add_argument("--resume-at-gate", action="store_true")
    parser.add_argument("--git-repository", type=Path)
    parser.add_argument("--git-base-ref")
    parser.add_argument("--otlp-endpoint")
    display = parser.add_mutually_exclusive_group()
    display.add_argument(
        "--plain",
        action="store_true",
        help="force the line-oriented console even on an interactive terminal",
    )
    display.add_argument(
        "--tui",
        action="store_true",
        help="require the full-screen interactive terminal interface",
    )
    arguments = parser.parse_args(argv)
    if (arguments.git_repository is None) != (arguments.git_base_ref is None):
        parser.error("--git-repository and --git-base-ref must be supplied together")
    input_value = input_stream if input_stream is not None else sys.stdin
    output = output_stream if output_stream is not None else sys.stdout
    terminal_supported = _supports_tui(input_value, output)
    if arguments.tui and not terminal_supported:
        parser.error("--tui requires interactive stdin/stdout and a supported terminal")
    use_tui = arguments.tui or (terminal_supported and not arguments.plain)
    activity_stream = CodexActivityStream() if use_tui else None

    runtime: CodexOSRun | None = None
    recorder: GenerationGitRecorder | None = None
    observability: ExperimentObservability | None = None
    try:
        observability = ExperimentObservability(
            arguments.run_directory,
            otlp_endpoint=arguments.otlp_endpoint,
        )
        runtime = CodexOSRun(
            arguments.run_directory,
            observability=observability,
            activity_stream=activity_stream,
        )
        if arguments.git_repository is not None:
            recorder = GenerationGitRecorder(
                arguments.git_repository,
                arguments.run_directory,
                arguments.git_base_ref,
            )
        if arguments.resume_at_gate:
            runtime.reopen_at_gate()
        else:
            if arguments.initial_iso is None:
                raise RuntimeError("initial ISO is unavailable")
            runtime.start(arguments.initial_iso)
    except (
        ExperimentObservabilityError,
        OSError,
        RuntimeError,
        ValueError,
    ) as error:
        if runtime is not None:
            runtime.stop()
        if observability is not None:
            observability.close()
        print(f"Error: failed to start CodexOS: {error}", file=output)
        return 1

    try:
        if use_tui:
            if activity_stream is None:
                raise RuntimeError("interactive activity stream is unavailable")
            from .operator_tui import run_operator_tui

            run_operator_tui(
                runtime,
                activity_stream,
                git_recorder=recorder,
                interview_repository=arguments.git_repository,
            )
        else:
            OperatorConsole(
                runtime,
                input_value,
                output,
                git_recorder=recorder,
                interview_repository=arguments.git_repository,
            ).run()
    finally:
        try:
            runtime.stop()
        finally:
            observability.close()
    return 0


def _supports_tui(input_stream: TextIO, output_stream: TextIO) -> bool:
    try:
        interactive = input_stream.isatty() and output_stream.isatty()
    except (AttributeError, OSError):
        return False
    return interactive and os.environ.get("TERM", "") not in {"", "dumb"}


if __name__ == "__main__":
    raise SystemExit(main())
