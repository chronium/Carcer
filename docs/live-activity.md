# Live activity stream

The trusted harness can optionally expose an in-memory `CodexActivityStream`
for the interactive operator interface. It assigns one ordered sequence to
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
and binary result bytes. The full-screen renderer safely escapes terminal
controls, displays binary data deterministically, and bounds its own scrollback
without truncating or dropping the semantic producer events. The plain operator
console does not create or consume a stream.

Only app-server content explicitly exposed as renderable text may be surfaced.
The current installed protocol provides agent-message text and explicit
reasoning-summary text/deltas. Opaque/private reasoning state and raw reasoning
content are ignored; the harness does not decode, infer, or reconstruct hidden
reasoning.

## Interactive operator interface

When both standard input and output are interactive and the terminal is
supported, the operator console opens a Textual full-screen interface. Redirected
or scripted operation retains the line-oriented console. `--plain` forces that
fallback on a terminal; `--tui` explicitly requires an interactive supported
terminal and fails clearly otherwise.

The compact header shows the run, generation, runtime state, Sol activity,
pending feature-request count, and command state. The main pane follows detailed
Sol, Luna, tool, source-write, and trusted-build activity. Operator command
output appears in the same pane, while command input remains pinned at the
bottom and isolated from asynchronous updates.

`PageUp` and `PageDown` scroll activity without taking editing focus from the
command input. Manual scrolling suspends live follow and shows the count of new
activity. `End` returns to the newest item and resumes following. Mouse-wheel
scrolling is also supported by terminals that report it. Confirmation prompts
remain inside the full-screen interface and default to No.

Renderable reasoning is labelled as a reasoning summary. It is only the summary
text deliberately exposed by app-server, never full/private chain-of-thought.
Codex, guest, diagnostic, and feature-request text is treated as untrusted data:
embedded control sequences are shown visibly rather than interpreted by the
terminal or Textual markup.

The TUI remains a non-authoritative, non-persistent observer. Its bounded display
history is not a transcript archive, does not enter `events.jsonl` or OTLP, and
never becomes guest or successor memory. Slow, absent, stopped, or broken
observation remains unable to affect Codex turns, reviewer consultations, guest
tools, builds, or generation lifecycle.
