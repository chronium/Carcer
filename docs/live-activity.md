# Live activity stream

The trusted harness can optionally expose an in-memory `CodexActivityStream`
for a future interactive operator interface. It assigns one ordered sequence to
activity observed across implementor, reviewer, guest-tool, trusted-build, and
candidate-validation threads. Producers enqueue typed semantic events; they do
not call a renderer or wait for a consumer.

The stream is non-authoritative and operator-observation only. It is not part of
the guest protocol, is not written to `events.jsonl`, is not sent through OTLP,
and is not persisted yet. Its contents never enter guest state, a generation
handoff, a reviewer prompt, or a successor implementor prompt. The immutable
generation archives and existing structured observability retain their current
roles.

Tool events preserve structured arguments and results, including source text
and binary result bytes. Terminal escaping, truncation, syntax highlighting,
diffs, and other presentation choices belong to a later renderer. The current
plain operator console does not consume or print this stream.

Only app-server content explicitly exposed as renderable text may be surfaced.
The current installed protocol provides agent-message text and explicit
reasoning-summary text/deltas. Opaque/private reasoning state and raw reasoning
content are ignored; the harness does not decode, infer, or reconstruct hidden
reasoning.

A later change may attach an interactive TUI consumer. Slow, absent, stopped,
or broken observation must remain unable to affect Codex turns, reviewer
consultations, guest tools, builds, or generation lifecycle.
