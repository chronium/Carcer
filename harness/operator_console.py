"""Plain-text trusted operator console for CodexOS."""

from __future__ import annotations

import argparse
import sys
from collections.abc import Sequence
from pathlib import Path
from typing import TextIO

from .generation_runtime import ArchivedGeneration, CodexOSRun, RuntimeState


class OperatorConsole:
    def __init__(
        self,
        runtime: CodexOSRun,
        input_stream: TextIO | None = None,
        output_stream: TextIO | None = None,
    ) -> None:
        self._runtime = runtime
        self._input = input_stream if input_stream is not None else sys.stdin
        self._output = output_stream if output_stream is not None else sys.stdout

    def run(self) -> None:
        self._print("CodexOS operator console")
        self._print()
        self._print(f"Run directory: {self._runtime.run_directory}")
        if self._runtime.state is RuntimeState.AWAITING_NEXT_GENERATION:
            self._print_gate()
        else:
            self._print_running_summary()
        self._print()
        self._print("Type 'help' for commands.")

        try:
            while True:
                try:
                    line = self._readline("codexos> ")
                    words = line.strip().split()
                    if not words:
                        continue
                    try:
                        if self._execute(words):
                            return
                    except (OSError, RuntimeError, ValueError) as error:
                        self._print(f"Error: {error}")
                except EOFError:
                    self._print("Input closed; stopping CodexOS run.")
                    return
                except KeyboardInterrupt:
                    self._print()
                    self._print("Interrupted; stopping CodexOS run.")
                    return
        finally:
            self._runtime.stop()

    def _execute(self, words: list[str]) -> bool:
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
        elif command == "pause":
            if len(words) != 1:
                self._print("Usage: pause")
            else:
                self._runtime.pause()
                self._print(
                    f"Generation {self._runtime.generation_number} paused."
                )
        elif command == "resume":
            if len(words) != 1:
                self._print("Usage: resume")
            else:
                self._runtime.resume()
                self._print(
                    f"Generation {self._runtime.generation_number} resumed."
                )
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
        self._print(
            "Selected successor: "
            + ("yes" if self._runtime.pending_generation_finish else "no")
        )
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
        if item.outcome == "completed":
            self._print("Handoff:")
            self._print_indented(item.handoff or "")
            self._print("Artifacts:")
            self._print("  boot ISO")
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
            self._print("  QEMU stdout")
            self._print("  QEMU stderr")

    def _abort(self) -> None:
        generation = self._runtime.generation_number
        if not self._confirm(
            f"Abort generation {generation} permanently? [y/N] "
        ):
            self._print("Abort cancelled.")
            return
        self._runtime.abort_generation()
        self._print_gate()

    def _continue_generation(self) -> None:
        if self._runtime.pending_generation_finish is None:
            self._runtime.continue_generation()
            return
        generation = (self._runtime.generation_number or 0) + 1
        self._print(
            f"Starting generation {generation} from selected successor..."
        )
        self._runtime.continue_generation()
        self._print_running_summary()

    def _rollback(self, parent: int) -> None:
        generation = (self._runtime.generation_number or 0) + 1
        if not self._confirm(
            f"Fork generation {generation} from generation {parent}'s "
            "selected successor?\n"
            "This preserves all later archives unchanged. [y/N] "
        ):
            self._print("Rollback cancelled.")
            return
        self._runtime.fork_from_generation(parent)
        self._print(
            f"Generation {self._runtime.generation_number} started from "
            f"generation {parent}."
        )
        self._print(f"State: {self._runtime.state.name}")
        self._print(f"QEMU PID: {self._runtime.active_pid}")

    def _quit(self) -> bool:
        if self._runtime.state in {RuntimeState.RUNNING, RuntimeState.PAUSED}:
            generation = self._runtime.generation_number
            if not self._confirm(
                f"Stop the run without archiving generation {generation}? "
                "[y/N] "
            ):
                self._print("Quit cancelled.")
                return False
        self._runtime.stop()
        return True

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
        else:
            self._print(f"Generation {generation} aborted.")
            self._print()
            self._print("No successor was selected.")
            self._print()
            self._print("Use:")
        self._print("  rollback N")
        self._print(f"  inspect {generation}")
        self._print("  history")
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
        return self._readline(prompt).strip() in {"y", "Y"}

    def _readline(self, prompt: str) -> str:
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
        print(text, file=self._output)


def main(
    argv: Sequence[str] | None = None,
    input_stream: TextIO | None = None,
    output_stream: TextIO | None = None,
) -> int:
    parser = argparse.ArgumentParser(description="CodexOS operator console")
    parser.add_argument("--run-directory", required=True, type=Path)
    parser.add_argument("--initial-iso", required=True, type=Path)
    arguments = parser.parse_args(argv)
    output = output_stream if output_stream is not None else sys.stdout

    runtime: CodexOSRun | None = None
    try:
        runtime = CodexOSRun(arguments.run_directory)
        runtime.start(arguments.initial_iso)
    except (OSError, RuntimeError) as error:
        if runtime is not None:
            runtime.stop()
        print(f"Error: failed to start CodexOS: {error}", file=output)
        return 1

    OperatorConsole(runtime, input_stream, output).run()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
