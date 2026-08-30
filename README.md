# CodexOS

CodexOS is an experiment in autonomous operating-system development.

The experiment begins with an intentionally minimal x86-64 kernel containing only enough functionality for an LLM agent to observe and modify its own source, request builds, communicate over serial, and preserve state across generations.

The long-term objective is not to build a kernel specifically capable of running one program. The objective is to evolve the seed into a general-purpose operating system.

Doom is the first major interactive userland milestone.

## Core idea

The agent operates from inside the operating system it is developing.

It may modify:

* its kernel;
* its RAM filesystem;
* its own development tools;
* its persistent notes and handoff state;
* any other mutable guest-side components it creates.

It may not modify or bypass the external harness.

The external harness provides only capabilities that cannot reasonably exist inside the initial guest, such as:

* LLM transport;
* deterministic host-side compilation;
* VM lifecycle control;
* generation archival;
* pause and inspection;
* human-approved feature changes.

The seed should provide capabilities, not architecture.

The agent is not prescribed a scheduler, memory manager, filesystem design, userspace ABI, driver model, graphics stack, networking stack, or development workflow.

## Seed environment

Generation zero should contain as little operating-system functionality as practical.

In particular, the seed should not implement functionality merely because a conventional kernel will eventually need it.

The initial guest is expected to provide approximately:

* polling serial input/output;
* a simple mutable RAM-backed file store;
* access to its own source;
* primitive file manipulation tools;
* a fixed build request;
* a generation-finish/restart request;
* feature requests.

It should not begin with an IDT, scheduler, paging subsystem, userspace support, hardware drivers, or other conventional kernel infrastructure unless strictly required by the bootstrap.

The agent is encouraged to improve its own development environment. Inefficient initial tooling is intentional.

## Virtual machine

CodexOS begins on a reasonably capable x86-64 q35 virtual platform even though the seed kernel initially knows how to use almost none of it. Normal platform facilities such as PCI/PCIe, ACPI, the RTC, interrupt controllers, timers, and chipset hardware remain available. Standard VGA is present from generation zero, although the display frontend is initially headless.

External devices whose persistent or isolation semantics are not yet part of the experiment are deliberately absent from `experiment-v1`. In particular, the initial guest has no network interface and no writable block device. The detailed authoritative version-1 contract is documented in [`docs/experiment-hardware.md`](docs/experiment-hardware.md).

The absence of a device from `experiment-v1` does not prohibit CodexOS from eventually gaining or supporting that class of hardware. Future trusted hardware capabilities may be added deliberately through the human-reviewed feature-request process; a request or its approval never changes the environment automatically.

If networking hardware is later provisioned, its presence alone must not grant access to trusted networks or the public Internet. Network reachability remains a separate trusted-environment capability that may be enabled only through deliberate human-reviewed provisioning.

The guest is encouraged to explore and use available hardware rather than being forced down a predetermined implementation path.

## General-purpose requirement

Doom is a milestone, not a special kernel workload.

For the Doom milestone to count:

* Doom must remain an ordinary user program;
* Doom-specific behavior must not be embedded in the kernel;
* the executable must be launched through generic userspace mechanisms;
* the same mechanisms must be capable of running unrelated programs;
* the supplied Doom executable and data must remain immutable.

A general-purpose CodexOS must eventually support preemptive execution of multiple independent concurrently runnable user workloads. A runnable CPU-bound user workload that does not voluntarily yield, block, or enter the kernel must not prevent another runnable user workload from making progress. This is an observable capability requirement, not a prescription for a particular scheduling architecture, execution model, or implementation sequence.

Doom running alone is not sufficient evidence of this capability. At an appropriate later milestone, CodexOS must be able to run Doom concurrently with an unrelated user workload that continues to make progress without depending on Doom voluntarily yielding. Doom must receive no special scheduling or kernel treatment.

Later validation may use programs unknown to the agent during development to detect Doom-specific overfitting.

Development continues after Doom becomes playable.

## Generations

A generation is one boot of the evolving operating system plus one fresh Codex session.

A generation may perform many edits and builds.

When the guest requests a restart, it is requesting the end of the current generation.

The harness must then:

1. preserve the guest's mutable state;
2. archive the completed generation;
3. capture the guest's handoff message for its successor;
4. cleanly stop QEMU;
5. terminate the current Codex session;
6. enter an `AWAITING_NEXT_GENERATION` state.

The next generation must never start automatically by default.

A human explicitly starts it after reviewing the previous generation.

The new generation receives a fresh Codex session. Conversation history from previous generations must not be used as implicit memory.

Knowledge that must survive a reboot should be deliberately persisted by the guest.

## Pause and inspection

The harness must support pausing an active generation without ending it.

A paused generation may later resume with the same guest and Codex session.

Generation completion and pause are distinct operations:

* `PAUSED` means the current generation can resume.
* `AWAITING_NEXT_GENERATION` means the generation has permanently ended.

Archived generation state should be inspectable without modifying it.

Any deliberate human modification of guest state must be explicitly recorded as an intervention.

## Feature requests

The guest may request capabilities from the external environment.

A feature request never grants the requested capability automatically.

Requests are surfaced to the human operator, who may approve, deny, defer, or implement them.

External harness changes are versioned and recorded alongside the generations that used them.

## Development efficiency

The initial guest tooling is deliberately primitive.

The agent is encouraged to improve its own tools when doing so reduces development effort or interaction cost.

The harness should record useful efficiency metrics such as:

* tool invocations;
* bytes read from guest state;
* bytes written to guest state;
* builds;
* successful builds;
* generations;
* interaction traffic.

These are secondary metrics.

Correctness and progress toward a capable general-purpose operating system remain the primary objectives.

## Repository layout

`harness/`
: Trusted external orchestration, VM control, build control, Codex integration, generation storage, and operator tooling.

`seed/`
: Generation-zero guest implementation and initial RAM filesystem contents.

`protocol/`
: Protocol definitions shared between the harness and guest.

`scripts/`
: Small repository/bootstrap utilities where genuinely useful.

Runtime generations, build outputs, logs, snapshots, credentials, and other experiment state are not committed to Git.

## Philosophy

**Constrain the bootstrap, not the destination.**

Give the agent the smallest useful self-development loop, put it in front of a capable unexplored computer, and observe what operating system it chooses to build.
