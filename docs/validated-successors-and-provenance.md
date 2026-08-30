# Validated successors and generation provenance

The immutable generation archive is the source of truth for a completed
CodexOS generation. A generation may finish only from the exact current source
snapshot associated with its latest successful trusted build.

Guest-visible build success includes two mandatory stages:

1. deterministic trusted compilation, linking, and ISO creation;
2. an ephemeral boot of that exact ISO under the current trusted hardware
   profile, followed by the canonical READY marker and a successful read-only
   `list_tools` protocol exchange.

The candidate VM has the same external isolation as the running generation. It
has no persistent writable state, is never archived as a generation, and is
always terminated when validation ends. A compilation-successful image that
does not boot or speak the protocol is a failed guest build and cannot replace
the previous successful build. This validation exists because experiment-001
demonstrated that a valid compilation and ISO can still select an unbootable
successor after the active Codex session has permanently ended.

Local Git provenance is a derived human-readable projection of completed
archives. Each run uses the run-directory basename as its tag namespace:

```text
experiment-002/generation-0000
experiment-002/generation-0001
```

These are immutable annotated tags. Each tag points to the derived source
commit whose ancestry follows the archive's actual parent generation, and its
annotation includes deterministic run/generation metadata plus the exact
archived handoff text. Reconciliation reconstructs missing tags from archives
and rejects lightweight, moved, or differently annotated tags rather than
rewriting them.

The legacy `experiment/generation-*` tags published for experiment-001 predate
run-scoped naming. They remain historical and immutable; new recorders neither
migrate them nor use them as parents for another run.
