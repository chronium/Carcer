# Provided assets

`--provided-assets PATH` explicitly supplies one human-managed external
directory to a new run or gate reopen. There is no default path. The harness
reads and derives the complete set once, retains exact bytes in memory for that
process, and never rereads external files while servicing a guest request.

Each direct child directory is one asset and its name is the stable asset ID.
IDs match `[a-z0-9]+(?:-[a-z0-9]+)*` and are at most 64 UTF-8 bytes. A child
containing exactly one regular file exposes that file verbatim under its safe
basename. Every other valid directory tree becomes a deterministic,
uncompressed `<asset-id>.tar` without an extra wrapper directory. Generated
archives use lexical path ordering, fixed ownership and timestamps, canonical
permissions, and no symlinks or special files.

The run records an append-only asset-set revision ledger in
`provided-assets.json`. Every revision contains the complete effective set,
ordered by ID, and records only its activation generation, IDs, exposed
filenames, byte sizes, and SHA-256 digests. A later gate reopen must explicitly
supply every previously introduced asset unchanged, but may add new IDs; the
physical source path may change. Removing or changing an existing ID fails
before a generation boots, while an unchanged reopen is idempotent. Existing
schema-version-1 exact-set manifests are retained as an activation-time-unknown
legacy revision when first adopted by the new ledger, rather than inventing
historical timing. Initializing or extending this run-level record at a
validated generation gate does not modify historical generation archives.

Each harness process derives and freezes the accepted set once. Changes to the
external directory after configuration cannot affect the active generation or
its candidate-validation VMs. A later gate reopen can accept an append-only
expansion and freezes that complete new revision for the next generation.

Guest access uses the existing serial host-service framing. The precise
`list_provided_assets` and `read_provided_asset` wire semantics are documented
in [`../protocol/provided-assets-host-service.md`](../protocol/provided-assets-host-service.md).
The active generation and candidate validation share the same in-memory
snapshot. One dispatcher owns each guest serial stream, so provided-asset
requests are serviced before and after READY without waiting for unrelated
development-tool traffic. Other trusted development host services retain their
existing scope inside a harness-initiated development-tool exchange; making
provided assets continuously available does not expose those services to idle
guest code. The sole dispatcher is a bounded duplex pump: it emits large host
responses incrementally while continuing to drain guest frames, preserves frame
ordering, and fails a response whose socket write stops making progress. This
allows the documented 1 MiB read while retaining one authoritative serial
reader and bounded shutdown. Asset bytes are not Codex dynamic-tool results and
do not enter source snapshots, generation archives, autonomous Git commits,
prompts, handoffs, interview artifacts, or operational telemetry.
