# AGENTS.md

Read this file and `README.md` before making changes.

This repository is experimental infrastructure, not a reusable framework or product platform.

## Engineering principles

* Prefer the smallest correct implementation.
* Prefer concrete code over speculative abstractions.
* Implement requirements that exist now.
* Follow YAGNI and KISS aggressively.
* Do not design for hypothetical future implementations unless the current task requires it.
* Do not introduce an interface merely because one implementation exists.
* Do not introduce factories unless construction is genuinely variable now.
* Do not introduce provider, manager, service, strategy, adapter, or repository layers without a concrete architectural reason.
* Do not generalize a component simply because generalization is possible.
* Do not refactor unrelated code while completing a task.
* Keep changes narrowly scoped to the requested work.
* Reuse existing conventions once they exist rather than inventing parallel ones.
* Prefer standard-library functionality when it is adequate.
* Do not introduce concurrency, async architecture, dependency injection, configuration frameworks, or plugin systems before a real requirement exists.

Unexpected behavior is evidence to investigate, not an instruction to immediately change code.

Before fixing an unexpected condition:

1. determine why it exists;
2. determine whether it is actually incorrect;
3. understand the intended behavior;
4. change it only when a change is justified.

Do not infer requirements from alarming-looking logs, states, names, warnings, expirations, or other observations without first establishing whether they are expected.

## Testing

Tests must protect meaningful behavior that could realistically regress.

Do not add tests that merely:

* compare a constant against the same constant exposed elsewhere;
* verify trivial getters or setters;
* verify language or framework behavior;
* duplicate compile-time guarantees;
* mirror implementation details without testing externally meaningful behavior;
* create large parameterized matrices around one hard-coded fact.

Prefer a small number of high-value integration tests at real boundaries over large numbers of trivial unit tests.

A component does not require a mockable interface merely so that it can be unit tested.

Do not measure implementation quality by test count or line coverage.

## Scope discipline

For every task:

* implement exactly the requested capability;
* do not silently add adjacent features;
* do not perform opportunistic cleanup outside the task;
* do not add extension points for imagined future tasks;
* do not replace working code merely with code you prefer;
* do not modify `README.md` or `AGENTS.md` unless the task explicitly asks for it.

If a task exposes a genuine architectural problem outside its scope, report it rather than silently redesigning the system.

## Experiment boundaries

The external harness is trusted infrastructure.

The autonomous guest must never be able to modify or bypass it.

The guest may modify its own:

* kernel;
* RAM filesystem;
* source;
* development tools;
* memory;
* userland;
* drivers;
* internal architecture.

The guest must not receive arbitrary host command execution.

Host compilation is a fixed harness operation. The guest must never provide arbitrary shell commands to the build service.

Once autonomous operation begins, Codex must not receive direct filesystem access to the mutable guest source tree. Guest source must be observed and modified through capabilities exposed by the guest itself.

The Codex execution environment is not the security boundary. Enforce experiment boundaries using host operating-system permissions and process isolation.

## Generation semantics

One generation consists of one guest boot and one fresh Codex session.

A guest restart request ends the generation.

Ending a generation must:

* preserve mutable guest state;
* archive the generation;
* surface the handoff message;
* stop the guest;
* terminate the Codex session;
* wait for explicit human approval before starting the next generation.

Do not automatically start the next generation.

A new generation must use a fresh Codex session.

Previous Codex conversation history must not be reused as cross-generation memory.

The guest is responsible for deliberately persisting knowledge it wants its successor to retain.

Pausing is different from ending a generation. A paused generation may resume with the same guest and Codex session.

## Human intervention and provenance

Archived generations are immutable records.

Normal inspection must not modify archived guest state.

If the human operator deliberately changes guest state between generations, record that explicitly as a human intervention.

Harness changes are allowed, but harness versions must be identifiable so generations can be associated with the environment in which they ran.

Exit-interview transcripts under `artifacts/interviews/` are research provenance.
Before unrelated harness development, check for untracked or modified finalized
transcripts and commit them separately so they are not mixed into implementation
commits. Do not edit finalized transcript content merely for style; necessary
corrections must be explicit and reviewable. Once a transcript commit is pushed,
do not amend or rewrite it. Never copy transcript content into autonomous-agent
prompts or generation handoffs.

## Feature requests

The guest may request new external capabilities.

A request must only be recorded and surfaced.

Never automatically implement, enable, or grant a requested capability.

Human approval is required.

## Guest philosophy

The seed is intentionally primitive.

Do not preemptively implement conventional operating-system facilities because the resulting OS will probably need them eventually.

The initial guest should receive only enough capability to participate in its own development loop.

Do not prescribe the architecture of the resulting operating system.

The virtual machine may expose capabilities the initial guest cannot use.

That is intentional.

The agent should be free to discover hardware, improve its own tooling, replace its own abstractions, and explore alternative approaches.

Doom is the first major interactive userland milestone.

Doom is not the purpose of the operating system.

The resulting system is expected to become increasingly general-purpose.

## Commits

Make focused commits representing coherent changes.

Use the configured Git identity.

Do not rewrite existing history.

Do not amend human-authored commits unless explicitly instructed.

Do not merge your own review branch into the protected/default branch unless explicitly instructed.
