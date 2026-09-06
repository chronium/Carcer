# AGENTS.md

Choose the context needed for the task. [README.md](README.md) describes the
experiment's purpose and behavioral requirements; consult it when those are relevant.

These instructions govern repository development. The autonomous guest receives
a separate trusted contract described in [docs/agent-contract.md](docs/agent-contract.md).
Repository access during harness development does not grant the guest that access.

CodexOS is experimental infrastructure, not a reusable framework or product platform.

## Working approach

* Carry an authorized task to completion. Resolve routine
  choices from context; ask when missing information would materially change the result.
* Continue independent work while awaiting clarification. Existing authorization
  covers necessary edits and checks; do not ask for it again.
* Prepare a concrete, reviewable change before requesting any required approval.
  The experiment gates below still apply; development authorization does not start
  a generation or provision a capability.
* Explicit user instructions take precedence over repository workflow preferences
  and skill guidance, subject to system and developer instructions.
* If an instruction blocks work, cite its file and exact wording, explain its
  applicability, and distinguish the rule from your interpretation.
* Give concise progress updates and report the result and unresolved limitations
  in plain language.

## Repository map

* `cmd/codexos/` and `internal/`: Go harness entry point and implementation.
* `seed/`: minimal generation-zero C guest; `Makefile` builds its bootable ISO.
* `protocol/`: shared wire and host-service contracts.
* Go tests live beside their packages.
* `artifacts/interviews/`: human-facing research provenance, never guest context.

Contextual references:

* Package ownership and lifecycle design: [docs/go-architecture.md](docs/go-architecture.md).
* Verification and acceptance: [docs/verification.md](docs/verification.md).

Go is the maintained harness implementation. Preserve wire and persisted-state
compatibility, including support for existing experiment archives, when changing
those boundaries. Read the relevant `protocol/` specification before changing a protocol.

## Engineering and scope

* Implement the smallest correct change for the current requirement. Apply YAGNI
  and KISS; prefer concrete code, existing conventions, and adequate standard-library tools.
* Add abstractions only for a present architectural need. One implementation or a
  desire to mock it does not justify an interface, factory, or extra layer.
* Introduce concurrency, dependency injection, configuration frameworks, and plugin
  systems only when the task requires them.
* Keep changes within the requested scope. Avoid adjacent features, speculative
  extension points, unrelated refactoring, and replacing working code by preference.
* Report architectural problems outside the task instead of silently redesigning them.
* Modify `README.md` or `AGENTS.md` only when explicitly requested.

Investigate unexpected behavior before changing it: establish why it exists,
whether it is incorrect, and the intended behavior. Logs, warnings, names, and
expirations alone do not establish a requirement or defect.

## Test design and command reference

Tests should protect meaningful behavior that could realistically regress. Prefer
a few tests at real boundaries over checks of constants, trivial accessors,
language behavior, compile-time guarantees, or implementation details. Test count
and coverage are not quality targets.

These commands are references, not a checklist for every edit. Paths are relative
to the repository root.

* Go formatting: `gofmt`.
* Go package tests: `go test ./internal/<package>`; race detection: `go test -race ./internal/<package>`.
* Full Go suite: `go test ./...`; static analysis: `go vet ./...`.
* Seed/build: `make seed`. It requires the pinned Limine submodule,
  `x86_64-elf-gcc`, `x86_64-elf-ld`, a host C compiler, Python, and `xorriso`.

Go requires the version in `go.mod`. The seed source-table generator uses only
the Python standard library. Some integration tests require Linux, QEMU, KVM,
or the cross-toolchain.
Use disposable test state. See `docs/verification.md` for resource-limited
commands and opt-in real-image acceptance. Report missing prerequisites and skipped
checks accurately; never validate against a live run as a substitute.

## Experiment boundaries

The external harness is trusted infrastructure. The autonomous guest must never
modify or bypass it. Enforce this through host OS permissions and process isolation;
the Codex execution environment is not the security boundary.

The guest may change its kernel, RAM filesystem, source, tools, memory, userland,
drivers, and internal architecture. Once autonomous operation begins, its Codex
session must observe and modify mutable guest source only through guest-exposed
capabilities, with no direct filesystem access to that source tree.

Do not grant arbitrary host command execution. Host compilation is a fixed harness
operation and must never accept guest-provided shell commands.

## Generation semantics

One generation consists of one guest boot and one fresh Codex session. A guest
restart request permanently ends that generation. Completion must:

1. Preserve mutable guest state and archive the generation.
2. Surface the deliberate handoff and stop the guest.
3. Retire the Codex session from development. A healthy session may remain only
   for the existing read-only exit interview at the frozen gate; close it before
   continuation, rollback, or shutdown.
4. Wait for explicit human approval before starting the next generation.

Never start a successor automatically. Its Codex session must be fresh, without
previous conversation history as implicit memory. The guest must deliberately
persist knowledge it wants its successor to retain.

A pause may resume the same guest and session. It does not end the generation.
Build success requires both compilation and candidate boot/protocol validation;
see [docs/validated-successors-and-provenance.md](docs/validated-successors-and-provenance.md).

## Human intervention and provenance

Archived generations are immutable. Inspection must not change archived guest
state. Record deliberate human changes between generations as interventions.
Harness versions must remain identifiable so each generation can be associated
with the environment that ran it.

Before unrelated harness development, check `git status --short --untracked-files=all -- artifacts/interviews`
for modified or untracked finalized transcripts and commit them separately from
implementation changes. Do not edit finalized content for style; necessary
corrections must be explicit and reviewable. Never amend or rewrite a pushed
transcript commit. Never copy interview content into autonomous prompts or handoffs.

## Feature requests

A guest request for an external capability is recorded and surfaced only. Never
automatically implement, enable, or grant it. Human approval is required, and
approval itself does not provision anything.

## Guest philosophy

Keep the seed primitive: enough capability to participate in its own development
loop. Do not preemptively add conventional OS facilities or prescribe its eventual
architecture. The VM may expose hardware the seed cannot yet use; this is intentional.
The guest is free to discover hardware, improve tooling, and replace abstractions.

Doom is the first major interactive userland milestone. Development continues
toward a general-purpose OS afterward; Doom must use generic userland mechanisms
and receive no special kernel treatment. See `README.md` for the behavioral requirements.

## Commits

* Preserve unrelated user changes and keep commits focused on coherent changes.
* Use the configured Git identity.
* Do not rewrite existing history or amend human-authored commits without explicit instruction.
* Do not merge your own review branch into the protected/default branch without explicit instruction.
