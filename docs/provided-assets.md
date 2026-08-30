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

The run records only schema version, IDs, exposed filenames, byte sizes, and
SHA-256 digests in `provided-assets.json`. A later reopen must explicitly
supply a set that derives to exactly that metadata; the physical source path
may change. Omission or mismatch fails before a generation boots. Initializing
this run-level record at a validated generation gate does not modify historical
generation archives.

Guest access uses the existing serial host-service framing. The precise
`list_provided_assets` and `read_provided_asset` wire semantics are documented
in [`../protocol/provided-assets-host-service.md`](../protocol/provided-assets-host-service.md).
The active generation and candidate validation share the same in-memory
snapshot. One dispatcher owns each guest serial stream, so provided-asset
requests are serviced before and after READY without waiting for unrelated
development-tool traffic. Asset bytes are not Codex dynamic-tool results and
do not enter source snapshots, generation archives, autonomous Git commits,
prompts, handoffs, interview artifacts, or operational telemetry.
