# Go operator feature-request decision notes

At the existing generation gate, the Go console accepts:

```text
feature-approve N [NOTE]
feature-deny N [NOTE]
```

The note is optional UTF-8 text of at most 4096 encoded bytes. It is the remainder
of the command after the request ID, with surrounding whitespace trimmed; inner
whitespace and quotation marks are literal. Validation happens before confirmation.
The console shows the escaped note, then asks for the existing decision
confirmation. Cancellation leaves both status and note unchanged. The decision
still requires a pending request and the ordinary generation gate.

`features` lists the complete request history with operator notes. `feature N`
shows the guest description and operator decision note under separate labels.
The implementor's `list_requests` result includes `decision_note` when present;
approved requests also include a labeled note in the fresh implementor context.
Pending and denied requests remain available through `list_requests`.

Notes clarify operator intent. Neither a note nor an approval provisions a
capability, changes service limits, creates job credits, or starts a generation.
For example, an operator can explain that a requested batch is within already
provisioned bootstrap limits without establishing a new enforced batch quota.

## Persistence and inheritance

The Go request record extends the existing five JSON fields with optional
`decision_note`. Nonempty notes are valid only on approved or denied records;
`null`, non-string values, invalid Unicode and notes over 4 KiB are rejected.
Empty notes are omitted when encoding, preserving the existing bytes for records
without notes. Status and note are written in one file, synced, atomically renamed,
and followed by containing-directory and run-directory syncs before success is reported. A finalized
decision cannot be replaced through the decision command.

Notes remain in the run-level request store across reopen, continuation and
rollback. Cross-run initialization includes them in both the immutable inherited
ledger (and its hash) and the destination request store. A finalized inherited
note must match the immutable ledger. An inherited pending request can acquire
its decision and note after a destination generation has been archived, under the
existing gate rules; subsequent inheritance carries that decision intact.
Historical generation archives and handoffs are not rewritten.

## Compatibility

Existing records and cross-run ledgers without notes retain their canonical
representation. Older readers reject records containing `decision_note`; a run
that uses notes must be reopened or inherited with a harness supporting
this extension. No guest wire protocol changes: `request_feature` continues to
accept only the guest-authored title and description.
