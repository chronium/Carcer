# Live activity stream

The trusted harness can optionally expose an in-memory `CodexActivityStream`
for the interactive operator interface. It assigns one ordered sequence to
activity observed across implementor, reviewer, guest-tool, trusted-build, and
candidate-validation threads. Producers enqueue typed semantic events; they do
not call a renderer or wait for a consumer.

The stream is non-authoritative and operator-observation only. It is not part of
the guest protocol, is not written to `events.jsonl`, and is not sent through
OTLP. The general live stream is not persisted. Its contents never enter guest
state, a generation handoff, a reviewer prompt, or a successor implementor
prompt. The immutable generation archives and existing structured observability
retain their current roles.

Tool events preserve structured arguments and results, including source text
and binary result bytes. The full-screen renderer safely escapes terminal
controls, displays binary data deterministically, and bounds its own scrollback
without truncating or dropping the semantic producer events. The plain operator
console does not create or consume a stream.

Only app-server content explicitly exposed as renderable text may be surfaced.
The current installed protocol provides agent-message text and explicit
reasoning-summary text/deltas. Opaque/private reasoning state and raw reasoning
content are ignored; the harness does not decode, infer, or reconstruct hidden
reasoning. Sol and Luna use high reasoning effort and explicitly request the
separate `auto` reasoning-summary mode.

## Interactive operator interface

When both standard input and output are interactive and the terminal is
supported, the operator console opens a Textual full-screen interface. Redirected
or scripted operation retains the line-oriented console. `--plain` forces that
fallback on a terminal; `--tui` explicitly requires an interactive supported
terminal and fails clearly otherwise.

The compact header shows the run, generation, runtime state, Sol activity,
pending feature-request count, and command state. The main pane presents Sol
and Luna messages as the primary transcript, reasoning summaries as subordinate
plain text, dynamic tools as compact activity, and trusted builds as coherent
phase progress. Operator startup and each typed command are grouped into one
logical output block. Command input remains pinned at the bottom and isolated
from asynchronous updates.

Streaming agent messages remain safe plain text so token deltas do not trigger
repeated Markdown parsing and reflow. The authoritative completed message
updates the same logical row and renders as Markdown. Terminal controls are
escaped before either path is rendered, and Markdown is presentation only: it
cannot add a capability or affect the experiment.

Tool rows derive their concise name, target, state, and result summary from the
structured activity payload rather than parsing a formatted log string. Bounded
read, write, and result detail remains available from the row, but successful
large detail is collapsed by default so source payloads do not dominate the
transcript. Failed-tool detail opens by default. Expanding a row changes only
that mounted row and does not take command-input focus. Feature requests use a
more prominent presentation because they may require operator action, while
remaining advisory until the existing operator commands act on them.

One trusted build occupies one stable row containing explicit compile/link,
candidate-boot, READY, and protocol phase states. Routine successful
session/turn/review lifecycle notifications are omitted from transcript rows;
interruptions, cancellations, and failures remain visible. These are display
policies only—the underlying typed events and structured operational
observability are unchanged.

`PageUp` and `PageDown` scroll activity without taking editing focus from the
command input. Manual scrolling suspends live follow and shows the count of new
activity. `End` returns to the newest item and resumes following. Mouse-wheel
scrolling is also supported by terminals that report it. Confirmation prompts
remain inside the full-screen interface and default to No.

While a generation is running, one `Esc` press only arms a pause confirmation;
it has no runtime effect and leaves command input intact. A second `Esc` within
2.5 seconds submits the ordinary authoritative `pause` command. The armed state
expires automatically and is cleared by other input, commands, generation/state
changes, and shutdown. An existing in-TUI confirmation takes precedence, where
`Esc` rejects it as No rather than arming pause.

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

The transcript keeps event interpretation in the Textual-independent
`OperatorActivityModel`. It produces bounded typed presentation state for
messages, reasoning, tools, builds, feature requests, operator blocks, and
abnormal lifecycle activity. Specialized Textual rows consume those types; they
do not parse flattened prose to rediscover event semantics. Each keyed logical
entry still has one stable mounted outer row. New keys mount rows, streamed
updates change only their existing row, detail expansion changes only its tool
row, and scrollback trimming unmounts only discarded keys. Ordinary deltas never
reconstruct the complete transcript widget tree. This preserves stable row
identity and historical viewport position while activity continues below.

Live follow uses the scroll container's bottom anchor. Manual scrolling releases
that anchor while new events and row updates continue to accumulate, and `End`
returns to the newest row and clears the unread count. Display history remains
bounded to 800 logical entries and 2 MiB, with each individual payload rendered
up to 64 KiB and an explicit marker when older displayed activity is discarded.
These are UI bounds only: they neither truncate nor apply backpressure to the
semantic `CodexActivityStream`.

At an eligible completed-generation gate, the header advertises the optional
same-thread exit interview and switches to an `interview>` prompt while it is
open. Human retrospective questions appear as distinct `You` transcript rows;
Sol answers and explicitly exposed reasoning summaries use their existing
presentation. On interview conclusion, the trusted operator layer writes the
questions, explicit reasoning summaries, and final Sol answers to
`artifacts/interviews/<run>/generation-NNNN.md` in the configured Git repository.
The write is atomic and never overwrites conflicting research provenance. The
artifact remains an uncommitted human worktree change until the operator records
it separately; generation Git reconciliation does not include it.

This narrow transcript artifact is separate from live-stream persistence. It is
not written to operational telemetry, a generation archive, a handoff, guest
state, reviewer context, or successor context. Only app-server reasoning-summary
content already admitted by the existing visibility boundary is eligible;
private/raw reasoning remains ignored.
