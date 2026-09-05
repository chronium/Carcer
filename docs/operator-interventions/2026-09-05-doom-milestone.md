# Operator intervention: Doom milestone clarification

Date: 2026-09-05. Authority: explicit human operator instruction. Scope: the
trusted Go harness objective, effective with agent contract version 9.

The operator reported that `experiment-004` generation 12's handoff said
“source DoomGeneric would not satisfy supplied-executable milestone.” The
operator explicitly rejected that interpretation and authorized this clarification:

- A source-built Doom port, including DoomGeneric, can satisfy the milestone.
- Running the exact supplied DOS executable is not required. Its availability
  does not create a DOS/4G compatibility requirement.
- Supplied original assets remain immutable. Guest-authored adaptations and
  build outputs are separate artifacts.
- Doom remains an ordinary workload executed through generic userland mechanisms,
  without Doom-specific kernel behavior or scheduling treatment. The requirement
  for concurrent progress by an unrelated workload is preserved, including its
  existing milestone timing and independence from Doom voluntarily yielding.

This permits the source route without selecting a port, prescribing an
implementation, changing guest code, or provisioning an external capability.
It supersedes contrary interpretations in inherited handoffs, guest notes,
review requests or proposals, and older objective wording.

The Go implementor and reviewer prompts carry the same dated authoritative
clarification. The implementor receives it in the first planning turn of every
fresh session, retained in the same thread for implementation; each fresh reviewer
receives it independently. This delivery does not depend on a per-generation
objective option or a corrected handoff. The existing implementor provenance
records the contract version, and harness identity identifies the deployed change.

This document and its Git commit record the intervention. No historical handoff,
archive, or guest state is rewritten. Adoption requires running the updated Go
harness before starting the next generation's fresh agent session; this change
does not update an existing session, start a generation, or authorize live cutover.
The Python reference and README are unchanged.
