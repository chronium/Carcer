# Go migration parity matrix

This document tracks the side-by-side Go rewrite. The Python harness remains the
behavioral reference and the operational entry point until a separately approved
cutover. `Complete` means the listed behavior has direct Go tests or conformance
evidence; it does not mean that a larger calling flow is complete.

Status values are `Not started`, `Partial`, and `Complete`.

| Behavior / Python owner | Go owner | Compatibility requirements | Python evidence | Go evidence | Status / differences |
|---|---|---|---|---|---|
| Frame codec (`framing.py`) | `internal/guest` | Exact little-endian header; 16 MiB bound; fragmentation, coalescing, invalid-prefix, and closure behavior | `test_framing.py` | `framing_test.go`, `conformance_test.go`, framing fuzz target | Complete for codec; transport deadlines remain with the serial dispatcher |
| Source snapshot codec (`source_snapshot.py`) | `internal/guest` | Exact encoding; bounds; UTF-8; duplicate and `seed/` path rejection; canonical round trips | `test_source_snapshot.py`, guest build/finish tests | `snapshot_test.go`, `conformance_test.go`, snapshot fuzz target | Complete for codec |
| Host-service request/response codec (`host_service_protocol.py`) | `internal/guest` | Exact message IDs, nonzero request IDs, limits, binary arguments/status/output | `test_host_service_protocol.py` | `protocol_test.go` | Complete for codec |
| Guest tool codec/client (`tool_protocol.py`) | `internal/guest` | Discovery/invocation encodings, request-ID rollover and matching, five-second exchange timeout | `test_tool_protocol.py` | `protocol_test.go` | Partial: codecs complete; client awaits dispatcher |
| Serial connection and READY (`serial.py`, `guest_startup.py`) | `internal/guest` | Unix connection deadlines, bounded diagnostics, canonical READY marker | `test_serial.py`, `test_candidate_boot.py`, `test_seed_boot.py` | — | Not started |
| Duplex serial dispatcher (`serial_protocol.py`) | `internal/guest` | One reader; dispatcher-owned ordered writes; bounded queue/write/shutdown; nested host calls; timeout suspension and progress deadline | `test_tool_protocol.py`, `test_guest_build.py`, `test_guest_finish_generation.py`, `test_guest_feature_requests.py` | — | Not started |
| Build and finish host services (`build_host_service.py`, `generation_finish_host_service.py`) | `internal/build`, `internal/guest` | Fixed build operation only; staged validated candidate; selected-successor freeze; feature/assets routing | `test_build_host_service.py`, guest build/finish/feature tests | — | Not started |
| Trusted compilation (`trusted_build.py`) | `internal/build` | Input validation, deterministic materialization, sandbox command, ISO construction, bounded diagnostics | `test_trusted_build.py`, `test_guest_build.py` | — | Not started |
| Candidate boot validation (`candidate_boot.py`) | `internal/build` | Exact hardware profile, READY and tool-protocol proof, cleanup, fail-closed status | `test_candidate_boot.py` | — | Not started |
| Feature ledger (`feature_requests.py`) | `internal/store` | Sparse monotonic IDs, UTF-8 byte bounds, atomic durable decisions, gate-only approval/denial | `test_feature_requests.py`, feature guest/console tests | `feature_requests_test.go`, `feature_requests_conformance_test.go`, persistent-record fuzz target | Partial: compatible store complete for uint64 IDs/generations; runtime gate enforcement awaits `internal/experiment`; Python's larger-than-uint64 integers are conservatively rejected |
| Provided assets (`provided_assets.py`) | `internal/store`, `internal/guest` | Deterministic frozen bytes/tree tar, append-only manifest revisions, path/content privacy, bounded reads | `test_provided_assets.py` | — | Not started |
| Build/review forensic evidence (`forensic_provenance.py`) | `internal/provenance` | Immutable allocation, hashes, incomplete markers, atomic manifests, latest-success fail-closed behavior | `test_forensic_provenance.py` | — | Not started |
| Planning evidence (`planning_evidence.py`) | `internal/provenance` | Private text, public digest/identity only, attempts and interruption/failure durability | `test_planning_evidence.py`, generation worker tests | `planning_test.go`, `planning_conformance_test.go` | Partial: byte-compatible recorder complete; agent-session integration not started |
| Exit-interview transcript (`exit_interview_transcript.py`) | `internal/provenance` | Immutable finalized artifact, partial turns, safe paths, no prompt/handoff reuse | `test_exit_interview_transcript.py`, console/worker tests | — | Not started |
| Cross-run bootstrap (`cross_run_bootstrap.py`) | `internal/store` | Immutable source identity, ISO/hash/Git verification, collision and mismatch failure, inherited handoff/ledger | `test_cross_run_bootstrap.py`, `test_gate_reopen.py` | — | Not started |
| Run/generation lifecycle (`generation_runtime.py`) | `internal/experiment` | States and gates, archive format, continue/fork/abort/pause/resume/stop, rollback ancestry, compatible reopen | `test_generation_runtime.py`, `test_gate_reopen.py`, guest integration tests | — | Not started |
| Hardware manifest/profile (`hardware.py`) | `internal/qemu` | Exact q35 device/profile arguments, QEMU version, canonical manifest validation | `test_hardware.py` | — | Not started |
| QEMU/QMP ownership (`qemu.py`, `qmp.py`) | `internal/qemu` | Process-group lifecycle, bounded stop/kill, QMP greeting/capabilities/commands/errors | `test_qemu.py`, `test_qmp.py` | — | Not started |
| Generation Git lineage (`generation_git.py`) | `internal/provenance` | Base-ref identity, commit/tree/tag messages and namespace, reconciliation without worktree mutation, immutable conflicts | `test_generation_git.py`, `test_gate_reopen.py` | — | Not started |
| App-server transport (`codex_app_server.py`) | `internal/codexapp` | JSONL process protocol, catalog validation, notifications, interrupts, errors, bounded stderr and shutdown | generation/review worker tests, fake app server | — | Not started |
| Implementor/planner session (`codex_generation_worker.py`) | `internal/agent` | Same thread for planning and implementation, exact policies/prompts, dynamic tools, pause/resume, token accounting | `test_codex_generation_worker.py` | — | Not started |
| Reviewer (`codex_review_worker.py`) | `internal/agent` | Separate read-only session, source evidence, fixed model/policy, cancellation and result isolation | `test_codex_review_worker.py` | — | Not started |
| Activity and observability (`codex_activity.py`, `observability.py`) | `internal/observability` | Structured event schema, sequence/recovery, bounded labels, private text exclusion, optional OTLP | activity/observability/forensic tests | — | Not started |
| Operator commands/CLI (`operator_console.py`) | `internal/operator`, `cmd/codexos` | Existing flags/defaults/validation/exit codes; command confirmations; non-TUI behavior; clean session/runtime ordering | `test_operator_console.py`, `test_gate_reopen.py`, `test_provided_assets.py` | — | Not started; Python remains default entry point |
| TUI model and application (`operator_tui_model.py`, `operator_tui.py`) | `internal/tui` | Bubble Tea v2 cursed renderer; attribution/transcript/selection/prompts; shutdown and terminal restoration | `test_operator_tui_model.py`, `test_operator_tui.py` | — | Not started |

## Operational entry points and formats

The current Python entry point is `python -m harness.operator_console`. Its
required opening mode is exactly one of `--initial-iso` and `--resume-at-gate`.
The remaining public flags are `--run-directory`, `--git-repository`,
`--git-base-ref`, `--inherit-from-run`, `--inherit-from-generation`,
`--provided-assets`, `--otlp-endpoint`, `--plain`, and `--tui`. Pairing and mode
validation in `operator_console.main` is part of parity.

Authoritative persistent formats include generation directories and metadata,
selected source/ISO and handoff, aborted archives, hardware manifests, events
JSONL, feature request records, provided-assets manifests, build/review/planning
evidence, cross-run bootstrap records, Git refs/commits/tags, and finalized exit
interview Markdown. Their schemas and durability behavior must be derived from
the Python readers/writers and their failure-path tests before Go publishes any
of them.
